// Cellular data usage tracking.
//
// The dongle in ECM mode is a plain network interface to macOS, so usage is
// measured from the interface byte counters (netstat -ibn) of the Baiwang
// ECM interface, sampled every few seconds. Daily totals are persisted to
// data/usage.json so history survives restarts; counter resets (replug,
// reboot) are handled by treating a lower reading as a fresh baseline.
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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

type TrafficSnapshot struct {
	Iface     string      `json:"iface"`
	Up        bool        `json:"up"`
	SessionRx uint64      `json:"session_rx"`
	SessionTx uint64      `json:"session_tx"`
	Today     DayUsage    `json:"today"`
	Days      []DayUsage  `json:"days"`
	History   []RatePoint `json:"history"`
	RxBps     float64     `json:"rx_bps"`
	TxBps     float64     `json:"tx_bps"`
}

type TrafficTracker struct {
	mu        sync.Mutex
	dataFile  string
	iface     string
	up        bool
	lastRx    uint64
	lastTx    uint64
	haveLast  bool
	lastTime  time.Time
	sessionRx uint64
	sessionTx uint64
	daily     map[string]*DayUsage
	history   []RatePoint
	lastSave  time.Time
	stop      chan struct{}
}

func NewTrafficTracker(dataDir string) *TrafficTracker {
	t := &TrafficTracker{
		dataFile: filepath.Join(dataDir, "usage.json"),
		daily:    map[string]*DayUsage{},
		stop:     make(chan struct{}),
	}
	t.load()
	return t
}

func (t *TrafficTracker) load() {
	raw, err := os.ReadFile(t.dataFile)
	if err != nil {
		return
	}
	var days []DayUsage
	if json.Unmarshal(raw, &days) == nil {
		for i := range days {
			d := days[i]
			t.daily[d.Day] = &d
		}
	}
}

func (t *TrafficTracker) saveLocked() {
	days := make([]DayUsage, 0, len(t.daily))
	for _, d := range t.daily {
		days = append(days, *d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day < days[j].Day })
	if raw, err := json.MarshalIndent(days, "", "  "); err == nil {
		os.WriteFile(t.dataFile, raw, 0o644) //nolint:errcheck
	}
	t.lastSave = time.Now()
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
	t.saveLocked()
	t.mu.Unlock()
}

func (t *TrafficTracker) sample() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.iface == "" {
		t.iface = resolveIface()
	}
	rx, tx, ok := t.readIface()
	now := time.Now()
	if !ok {
		t.up = false
		t.haveLast = false
		t.history = appendPoint(t.history, RatePoint{T: now.Unix()})
		return
	}
	t.up = true

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
		day := now.Format("2006-01-02")
		d := t.daily[day]
		if d == nil {
			d = &DayUsage{Day: day}
			t.daily[day] = d
		}
		d.Rx += drx
		d.Tx += dtx
	} else {
		t.history = appendPoint(t.history, RatePoint{T: now.Unix()})
	}
	t.lastRx, t.lastTx, t.lastTime, t.haveLast = rx, tx, now, true

	if time.Since(t.lastSave) > persistEvery {
		t.saveLocked()
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

func (t *TrafficTracker) Snapshot() TrafficSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()

	today := time.Now().Format("2006-01-02")
	snap := TrafficSnapshot{
		Iface:     t.iface,
		Up:        t.up,
		SessionRx: t.sessionRx,
		SessionTx: t.sessionTx,
		Today:     DayUsage{Day: today},
		History:   append([]RatePoint(nil), t.history...),
	}
	if d := t.daily[today]; d != nil {
		snap.Today = *d
	}
	days := make([]DayUsage, 0, len(t.daily))
	for _, d := range t.daily {
		days = append(days, *d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day > days[j].Day })
	if len(days) > 14 {
		days = days[:14]
	}
	snap.Days = days
	if n := len(t.history); n > 0 {
		snap.RxBps = t.history[n-1].Rx
		snap.TxBps = t.history[n-1].Tx
	}
	return snap
}
