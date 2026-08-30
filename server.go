package main

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	modem    *Modem
	traffic  *TrafficTracker
	store    *Store
	archiver *Archiver
	sims     *SIMRegistry

	statusMu   sync.Mutex
	statusVal  Status
	statusTime time.Time
}

func NewServer(m *Modem, t *TrafficTracker, s *Store, a *Archiver, sims *SIMRegistry) *Server {
	return &Server{modem: m, traffic: t, store: s, archiver: a, sims: sims}
}

// simParam resolves the ?sim=<id> query parameter, defaulting to the SIM
// currently in the dongle. Every SIM-scoped view goes through it.
func (s *Server) simParam(r *http.Request) (int64, error) {
	q := r.URL.Query().Get("sim")
	if q == "" {
		return s.sims.CurrentID(), nil
	}
	id, err := strconv.ParseInt(q, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid sim id %q", q)
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// writeModemErr splits a refusal this process made from one the modem made:
// bad input is the caller's fault (400), anything else is the dongle's (502).
func writeModemErr(w http.ResponseWriter, err error) {
	var bad inputError
	if errors.As(err, &bad) {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeErr(w, http.StatusBadGateway, err)
}

// cachedStatus throttles AT-port polling: concurrent/frequent UI refreshes
// reuse a snapshot at most 5 s old instead of hammering the modem.
func (s *Server) cachedStatus() Status {
	s.statusMu.Lock()
	defer s.statusMu.Unlock()
	if time.Since(s.statusTime) < 5*time.Second {
		return s.statusVal
	}
	s.statusVal = s.modem.Status()
	s.statusTime = time.Now()
	return s.statusVal
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cachedStatus())
}

// handleSims lists every SIM the dongle has held (GET) or renames one (POST
// {"sim_id":N,"label":"..."}).
func (s *Server) handleSims(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		sims, err := s.store.ListSIMs()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if sims == nil {
			sims = []SIM{}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"sims":    sims,
			"current": s.sims.CurrentID(),
		})
	case http.MethodPost:
		var req struct {
			SimID int64  `json:"sim_id"`
			Label string `json:"label"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.SetSIMLabel(req.SimID, strings.TrimSpace(req.Label)); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleTraffic serves the live view for the SIM in the dongle, or the stored
// history for any other SIM — rates, session bytes and the interface belong to
// the card that is actually online, so they are omitted for the rest.
func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	simID, err := s.simParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	snap := s.traffic.Snapshot()
	if simID != snap.SimID {
		snap, err = s.storedTraffic(simID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, snap)
}

func (s *Server) storedTraffic(simID int64) (TrafficSnapshot, error) {
	day := time.Now().Format("2006-01-02")
	today, err := s.store.GetDayUsage(simID, day)
	if err != nil {
		return TrafficSnapshot{}, err
	}
	days, err := s.store.RecentDays(simID, 14)
	if err != nil {
		return TrafficSnapshot{}, err
	}
	if len(days) == 0 || days[0].Day != day {
		days = append([]DayUsage{today}, days...)
	}
	return TrafficSnapshot{
		SimID:   simID,
		Today:   today,
		Month:   monthUsage(s.store, simID, today),
		Days:    days,
		History: []RatePoint{},
	}, nil
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	simID, err := s.simParam(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// pull fresh messages off the modem first (no-op if synced recently)
	if err := s.archiver.SyncIfStale(30 * time.Second); err != nil {
		log.Printf("sms sync: %v", err)
	}
	msgs, err := s.store.ListMessages(simID, 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if msgs == nil {
		msgs = []StoredMessage{}
	}
	writeJSON(w, http.StatusOK, msgs)
}

// handleData reads (GET) or sets (POST) the modem's cellular-data switch —
// the firmware's autoconnect flag. Setting it reboots the modem, so the
// dongle drops off the bus for ~20 s before the new state is observable.
func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		enabled, supported, err := s.modem.DataEnabled()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		if !supported {
			writeErr(w, http.StatusNotImplemented,
				errors.New(`firmware does not support AT+QCFG="qcautoconnect"`))
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": enabled})
	case http.MethodPost:
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.modem.SetDataEnabled(req.Enabled); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		log.Printf("cellular data switched %s (modem rebooting)", onOff(req.Enabled))
		// drop the cached status so the UI sees the new state next poll
		s.invalidateStatus()
		writeJSON(w, http.StatusOK, map[string]bool{"enabled": req.Enabled})
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// invalidateStatus drops the cached status so the next UI poll sees the modem
// as it is now rather than as it was up to 5 s ago.
func (s *Server) invalidateStatus() {
	s.statusMu.Lock()
	s.statusTime = time.Time{}
	s.statusMu.Unlock()
}

// handleSIMLock reads the PIN state of the card in the dongle (GET) or turns
// its power-on PIN request on or off (POST {"enabled":bool,"pin":"...."}).
// Enabling does not choose a PIN — it starts demanding the one the card
// already has; /api/sim/pin changes that code.
func (s *Server) handleSIMLock(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		lock, err := s.modem.SIMLockState()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, lock)
	case http.MethodPost:
		var req struct {
			Enabled bool   `json:"enabled"`
			PIN     string `json:"pin"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		lock, err := s.modem.SetPINLock(req.Enabled, req.PIN)
		if err != nil {
			writeModemErr(w, err)
			return
		}
		log.Printf("SIM PIN lock switched %s", onOff(req.Enabled))
		s.invalidateStatus()
		writeJSON(w, http.StatusOK, lock)
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// handleSIMUnlock feeds a locked card the code it is waiting for:
// POST {"code":"....","new_pin":"...."}. new_pin is required only in PUK
// state, where the PUK replaces the blocked PIN rather than restoring it.
func (s *Server) handleSIMUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Code   string `json:"code"`
		NewPIN string `json:"new_pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	lock, err := s.modem.EnterPIN(req.Code, req.NewPIN)
	if err != nil {
		var bad inputError
		code := http.StatusBadGateway
		if errors.As(err, &bad) {
			code = http.StatusBadRequest
		}
		body := map[string]any{"error": err.Error()}
		// on a rejected code the state comes back too, so the UI can show the
		// attempts the card has left; it is empty when the modem is unreachable
		if lock.State != "" {
			body["lock"] = lock
		}
		writeJSON(w, code, body)
		return
	}
	log.Printf("SIM unlocked (%s)", lock.State)
	s.invalidateStatus()
	// number, IMSI and operator are unreadable while locked — re-read the card
	// now so messages and usage are attributed to it rather than to the
	// unknown SIM
	s.sims.Refresh()
	writeJSON(w, http.StatusOK, lock)
}

// handleSIMPIN changes the card's PIN: POST {"pin":"....","new_pin":"...."}.
func (s *Server) handleSIMPIN(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		PIN    string `json:"pin"`
		NewPIN string `json:"new_pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	lock, err := s.modem.ChangePIN(req.PIN, req.NewPIN)
	if err != nil {
		writeModemErr(w, err)
		return
	}
	log.Printf("SIM PIN changed")
	writeJSON(w, http.StatusOK, lock)
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// handleMove re-files messages onto another SIM: POST {"ids":[..],"sim_id":N}.
// The pre-SIM archive can hold messages drained from a card the app never saw,
// and adoption has to guess; this is how a wrong guess gets corrected. sim_id 0
// ("unknown SIM") is a valid target and is not re-adopted later.
func (s *Server) handleMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		IDs   []int64 `json:"ids"`
		SimID int64   `json:"sim_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	moved, skipped, err := s.store.ReassignMessages(req.SimID, req.IDs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("moved %d message(s) to sim %d (%d skipped)", moved, req.SimID, skipped)
	writeJSON(w, http.StatusOK, map[string]int{"moved": moved, "skipped": skipped})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	res, err := s.modem.SendSMS(req.To, req.Text)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	log.Printf("sms sent to %s (%d part(s))", req.To, res.Parts)
	// credited to the SIM that actually sent it, not to the one being viewed
	if err := s.store.RecordOutbound(s.sims.CurrentID(), req.To, req.Text); err != nil {
		log.Printf("record outbound sms: %v", err)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/sims", s.handleSims)
	mux.HandleFunc("/api/traffic", s.handleTraffic)
	mux.HandleFunc("/api/data", s.handleData)
	mux.HandleFunc("/api/sim/lock", s.handleSIMLock)
	mux.HandleFunc("/api/sim/unlock", s.handleSIMUnlock)
	mux.HandleFunc("/api/sim/pin", s.handleSIMPIN)
	mux.HandleFunc("/api/sms/inbox", s.handleInbox)
	mux.HandleFunc("/api/sms/send", s.handleSend)
	mux.HandleFunc("/api/sms/move", s.handleMove)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}
