package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
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

	statusMu   sync.Mutex
	statusVal  Status
	statusTime time.Time
}

func NewServer(m *Modem, t *TrafficTracker, s *Store, a *Archiver) *Server {
	return &Server{modem: m, traffic: t, store: s, archiver: a}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
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

func (s *Server) handleTraffic(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.traffic.Snapshot())
}

func (s *Server) handleInbox(w http.ResponseWriter, r *http.Request) {
	// pull fresh messages off the modem first (no-op if synced recently)
	if err := s.archiver.SyncIfStale(30 * time.Second); err != nil {
		log.Printf("sms sync: %v", err)
	}
	msgs, err := s.store.ListMessages(200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if msgs == nil {
		msgs = []StoredMessage{}
	}
	writeJSON(w, http.StatusOK, msgs)
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
	if err := s.store.RecordOutbound(req.To, req.Text); err != nil {
		log.Printf("record outbound sms: %v", err)
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/traffic", s.handleTraffic)
	mux.HandleFunc("/api/sms/inbox", s.handleInbox)
	mux.HandleFunc("/api/sms/send", s.handleSend)

	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}
