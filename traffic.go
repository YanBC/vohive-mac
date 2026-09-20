// Cellular data usage tracking.
//
// The dongle in ECM mode is a plain network interface to macOS, so usage is
// measured from the interface byte counters (netstat -ibn) of the Baiwang
// ECM interface, sampled every few seconds. Daily totals are write-through
// persisted to the SQLite store; counter resets (replug, reboot) are handled
// by treating a lower reading as a fresh baseline.
package main

import (
	"log"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sampleInterval = 3 * time.Second
	historyLen     = 100 // ~5 min of rate points
	persistEvery   = 30 * time.Second
)

type RatePoint struct {
	T  int64   `json:"t"`  // unix seconds
	Rx float64 `json:"rx"` // bytes/sec down
	Tx float64 `json:"tx"` // bytes/sec up
}

type DayUsage struct {
	Day string `json:"day"`
	Rx  uint64 `json:"rx"`
	Tx  uint64 `json:"tx"`
}

type MonthUsage struct {
	Month string `json:"month"` // "2006-01"
	Rx    uint64 `json:"rx"`
	Tx    uint64 `json:"tx"`
}

type TrafficSnapshot struct {
	SimID     int64       `json:"sim_id"`
	Iface     string      `json:"iface"`
	Up        bool        `json:"up"`
	SessionRx uint64      `json:"session_rx"`
	SessionTx uint64      `json:"session_tx"`
	Today     DayUsage    `json:"today"`
	Month     MonthUsage  `json:"month"`
	Days      []DayUsage  `json:"days"`
	History   []RatePoint `json:"history"`
	RxBps     float64     `json:"rx_bps"`
	TxBps     float64     `json:"tx_bps"`
}

type TrafficTracker struct {
	mu    sync.Mutex
	store *Store
	sims  *SIMRegistry

	// bytes are attributed to whichever SIM was in the dongle when they were
	// counted; simID/today are rebound when the card or the day changes.
	simID     int64
	bound     bool
	today     DayUsage
	iface     string
	up        bool
	link      linkState // last ifconfig parse: link + low-data flags
	lastRx    uint64
	lastTx    uint64
	haveLast  bool
	lastTime  time.Time
	sessionRx uint64
	sessionTx uint64
	history   []RatePoint
	lastSave  time.Time
	stop      chan struct{}
}

func NewTrafficTracker(store *Store, sims *SIMRegistry) *TrafficTracker {
	// today's totals are loaded on the first sample, once the SIM is known
	return &TrafficTracker{store: store, sims: sims, simID: unknownSIM,
		stop: make(chan struct{})}
}

var bsdNameRe = regexp.MustCompile(`"BSD Name"\s*=\s*"(en\d+)"`)

// resolveIface finds the BSD interface name of the dongle's ECM function.
func resolveIface() string {
	out, err := exec.Command("ioreg", "-r", "-n", "Baiwang", "-l").Output()
	if err != nil {
		return ""
	}
	if m := bsdNameRe.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// readCounters parses `netstat -ibn` for the interface's Link-level row.
// Columns: Name Mtu Network Address Ipkts Ierrs Ibytes Opkts Oerrs Obytes Coll
func readCounters(iface string) (rx, tx uint64, ok bool) {
	out, err := exec.Command("netstat", "-ibn").Output()
	if err != nil {
		return 0, 0, false
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[0] != iface || !strings.HasPrefix(f[2], "<Link#") {
			continue
		}
		// Some rows omit the Address column; Ibytes/Obytes stay at fixed
		// offsets from the end: ... Ibytes Opkts Oerrs Obytes Coll
		n := len(f)
		rxv, err1 := strconv.ParseUint(f[n-5], 10, 64)
		txv, err2 := strconv.ParseUint(f[n-2], 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		return rxv, txv, true
	}
	return 0, 0, false
}

func (t *TrafficTracker) Start() {
	go func() {
		ticker := time.NewTicker(sampleInterval)
		defer ticker.Stop()
		t.sample()
		for {
			select {
			case <-ticker.C:
				t.sample()
			case <-t.stop:
				return
			}
		}
	}()
}

func (t *TrafficTracker) Stop() {
	close(t.stop)
	t.mu.Lock()
	t.persistLocked()
	t.mu.Unlock()
}

func (t *TrafficTracker) persistLocked() {
	if !t.bound {
		return // nothing accumulated yet
	}
	if err := t.store.SetDayUsage(t.simID, t.today); err != nil {
		log.Printf("persist usage: %v", err)
	}
	t.lastSave = time.Now()
}

// rebindLocked closes out the accumulation in progress and starts one for
// (simID, day), seeded with whatever the store already holds for that pair.
func (t *TrafficTracker) rebindLocked(simID int64, day string) {
	swapped := t.bound && simID != t.simID
	t.persistLocked() // close out the finished day / the outgoing SIM
	if swapped {
		t.sessionRx, t.sessionTx = 0, 0 // a session belongs to one SIM
	}
	today, err := t.store.GetDayUsage(simID, day)
	if err != nil {
		log.Printf("load usage for sim %d on %s: %v", simID, day, err)
		today = DayUsage{Day: day}
	}
	t.simID, t.today, t.bound = simID, today, true
}

func (t *TrafficTracker) sample() {
	// read outside the lock: the registry never touches the AT port here, but
	// this keeps the sampler's critical section free of any cross-component call
	simID := t.sims.CurrentID()

	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now()
	if day := now.Format("2006-01-02"); !t.bound || simID != t.simID || day != t.today.Day {
		t.rebindLocked(simID, day)
	}

	if t.iface == "" {
		t.iface = resolveIface()
	}
	rx, tx, ok := t.readIface()
	if !ok {
		t.up, t.link = false, linkState{}
		t.haveLast = false
		t.history = appendPoint(t.history, RatePoint{T: now.Unix()})
		return
	}
	t.link = readLinkState(t.iface)
	t.up = t.link.up

	if t.haveLast {
		drx := counterDelta(rx, t.lastRx)
		dtx := counterDelta(tx, t.lastTx)
		dt := now.Sub(t.lastTime).Seconds()
		if dt > 0 {
			t.history = appendPoint(t.history, RatePoint{
				T: now.Unix(), Rx: float64(drx) / dt, Tx: float64(dtx) / dt,
			})
		}
		t.sessionRx += drx
		t.sessionTx += dtx
		t.today.Rx += drx
		t.today.Tx += dtx
	} else {
		t.history = appendPoint(t.history, RatePoint{T: now.Unix()})
	}
	t.lastRx, t.lastTx, t.lastTime, t.haveLast = rx, tx, now, true

	if time.Since(t.lastSave) > persistEvery {
		t.persistLocked()
	}
}

// readIface reads counters, re-resolving the interface once if it vanished
// (dongle replugged and got a new BSD name).
func (t *TrafficTracker) readIface() (uint64, uint64, bool) {
	if t.iface == "" {
		return 0, 0, false
	}
	if rx, tx, ok := readCounters(t.iface); ok {
		return rx, tx, ok
	}
	if fresh := resolveIface(); fresh != "" && fresh != t.iface {
		t.iface = fresh
		t.haveLast = false
		return readCounters(t.iface)
	}
	return 0, 0, false
}

func counterDelta(cur, last uint64) uint64 {
	if cur >= last {
		return cur - last
	}
	return cur // counter reset: treat current value as the delta baseline
}

func appendPoint(h []RatePoint, p RatePoint) []RatePoint {
	h = append(h, p)
	if len(h) > historyLen {
		h = h[len(h)-historyLen:]
	}
	return h
}

// monthUsage totals the calendar month that `today` falls in: the store's rows
// for the other days plus the given today, whose in-memory copy can be ahead
// of its write-through row.
func monthUsage(store *Store, simID int64, today DayUsage) MonthUsage {
	m := MonthUsage{Month: today.Day[:7], Rx: today.Rx, Tx: today.Tx}
	rx, tx, err := store.MonthUsage(simID, m.Month, today.Day)
	if err != nil {
		log.Printf("month usage: %v", err)
		return m
	}
	m.Rx += rx
	m.Tx += tx
	return m
}

// Link returns the ECM interface and its last parsed state. Reusing the
// sampler's ifconfig output keeps the metered reconciler and the watchdog
// from spawning one of their own every few seconds.
func (t *TrafficTracker) Link() (iface string, link linkState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.iface, t.link
}

func (t *TrafficTracker) Snapshot() TrafficSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	today := t.today
	if !t.bound { // no sample has landed yet
		today = DayUsage{Day: time.Now().Format("2006-01-02")}
	}
	snap := TrafficSnapshot{
		SimID:     t.simID,
		Iface:     t.iface,
		Up:        t.up,
		SessionRx: t.sessionRx,
		SessionTx: t.sessionTx,
		Today:     today,
		History:   append([]RatePoint(nil), t.history...),
	}
	days, err := t.store.RecentDays(t.simID, 14)
	if err != nil {
		log.Printf("recent days: %v", err)
	}
	// the DB may lag the in-memory today by up to persistEvery
	found := false
	for i := range days {
		if days[i].Day == today.Day {
			days[i] = today
			found = true
		}
	}
	if !found {
		days = append([]DayUsage{today}, days...)
	}
	snap.Days = days
	snap.Month = monthUsage(t.store, t.simID, today)
	if n := len(t.history); n > 0 {
		snap.RxBps = t.history[n-1].Rx
		snap.TxBps = t.history[n-1].Tx
	}
	return snap
}
