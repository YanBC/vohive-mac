// Low-data ("metered") marking of the dongle's ECM link.
//
// macOS turns Low Data Mode on by itself for an iPhone personal hotspot, but
// the QDC507 in ECM mode presents as ordinary wired Ethernet: the OS sees a
// free link and runs iCloud sync, Photos uploads, App Store and system-update
// downloads over what is actually a capped LTE SIM. The switch underneath Low
// Data Mode is a pair of interface flags — IFEF_EXPENSIVE and
// IFXF_CONSTRAINED — which `ifconfig <iface> expensive constrained` sets and
// which NWPath then reports to every app as isExpensive/isConstrained. System
// services and well-behaved apps back off on their own; nothing is enforced,
// so this is a hint to macOS, not a cap (see store.Store.SetMetered for where
// the per-SIM setting lives).
//
// Two properties shape the design:
//
//   - The flags need root. There is no networksetup command for them and no
//     preferences.plist key, so either the process runs as root and can mark
//     the link, or it cannot and says so rather than pretending the setting
//     took.
//   - The flags live on the interface instance, not in stored configuration.
//     They are lost on every replug and on every USB port reset the watchdog
//     issues after sleep/wake — unattended events, nobody at the keyboard. So
//     they are re-asserted continuously rather than set once.
package metered

import (
	"fmt"
	"log"
	"sync"
	"time"

	"vohive-mac/internal/netif"
	"vohive-mac/internal/store"
)

const reconcileInterval = 5 * time.Second

// State is what the API reports: what the SIM's plan says, what the
// interface actually carries, and why those two might disagree.
type State struct {
	SimID      int64  `json:"sim_id"`
	Enabled    bool   `json:"enabled"` // the SIM is on a metered plan
	Applied    bool   `json:"applied"` // the link carries the flags right now
	Iface      string `json:"iface,omitempty"`
	Up         bool   `json:"up"`
	Present    bool   `json:"present"`    // the dongle's interface exists
	Privileged bool   `json:"privileged"` // this process can set the flags
	Error      string `json:"error,omitempty"`
}

// currentSIM is the part of the SIM registry this reconciler needs: which
// card's plan the link should be following right now.
type currentSIM interface {
	CurrentID() int64
}

// Controller keeps the ECM link's flags in step with the metered setting of
// whichever SIM is in the dongle.
type Controller struct {
	traffic *netif.Tracker
	store   *store.Store
	sims    currentSIM

	// applyMu serializes reconciliation: the ticker and the API handler both
	// drive it, and two ifconfig calls racing would fight over the flags.
	// `reported` belongs to it; mu guards lastErr alone, which is read by
	// the API while a reconcile is in flight.
	applyMu  sync.Mutex
	reported string // last condition logged, so a stuck one is not re-logged
	attempt  applyAttempt
	mu       sync.Mutex
	lastErr  string
	stop     chan struct{}
}

// applyAttempt counts how often the same change has been asked of the same
// interface without the link ever coming back showing it. ifconfig reports
// success by exiting 0, which is not the same as the flags having stuck, so
// an unobservable or refused change would otherwise be re-applied every tick
// forever — a line in the log every few seconds and a pointless subprocess.
type applyAttempt struct {
	iface string
	want  bool
	n     int
}

// applyLimit is deliberately more than one: a re-enumerating dongle can lose
// the flags between the apply and the next read quite legitimately.
const applyLimit = 3

// shouldApply records an attempt and reports whether it is worth making.
// Caller holds applyMu.
func (c *Controller) shouldApply(iface string, want bool) bool {
	if c.attempt.iface != iface || c.attempt.want != want {
		c.attempt = applyAttempt{iface: iface, want: want}
	}
	c.attempt.n++
	return c.attempt.n <= applyLimit
}

// applied notes that the link now reads back as asked, so the next divergence
// starts from a clean slate.
func (c *Controller) applied() { c.attempt = applyAttempt{} }

// New returns a controller; call Start to begin reconciling.
func New(t *netif.Tracker, s *store.Store, sims currentSIM) *Controller {
	return &Controller{traffic: t, store: s, sims: sims,
		stop: make(chan struct{})}
}

func (c *Controller) Start() {
	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		c.reconcile()
		for {
			select {
			case <-ticker.C:
				c.reconcile()
			case <-c.stop:
				return
			}
		}
	}()
}

// Stop ends the reconciler without clearing the flags: the dongle keeps
// carrying data after this process exits, and an unmarked link would quietly
// invite the OS to sync over it. macOS drops the flags on the next replug.
func (c *Controller) Stop() { close(c.stop) }

// say logs a condition once, until it changes — the reconciler runs every few
// seconds and a wedged state would otherwise fill the log. Caller holds applyMu.
func (c *Controller) say(key, format string, args ...any) {
	if c.reported == key {
		return
	}
	c.reported = key
	log.Printf(format, args...)
}

// reconcile brings the interface's flags to what the current SIM asks for.
// It is also the whole of the replug/USB-reset story: a re-enumerated
// interface comes back unmarked and is re-marked on the next tick.
func (c *Controller) reconcile() {
	c.applyMu.Lock()
	defer c.applyMu.Unlock()
	c.reconcileLocked()
}

func (c *Controller) reconcileLocked() {
	simID := c.sims.CurrentID()
	want, err := c.store.Metered(simID)
	if err != nil {
		c.setErr(err.Error())
		c.say("store", "metered: read setting for sim %d: %v", simID, err)
		return
	}
	iface, link := c.traffic.Link()
	if iface == "" || !link.Exists {
		return // no dongle to mark; nothing to report either
	}
	if link.Marked() == want {
		c.setErr("")
		c.reported = "" // next divergence is worth a line again
		c.applied()
		return
	}
	if !netif.Privileged() {
		c.setErr("needs root: restart the server with sudo")
		c.say("noroot", "metered: cannot set %s %s — "+
			"not running as root; restart with sudo", iface, meteredWord(want))
		return
	}
	if !c.shouldApply(iface, want) {
		c.setErr(fmt.Sprintf("%s did not take the low-data flags after %d tries",
			iface, applyLimit))
		c.say("stuck", "metered: %s still reads back %s after %d attempts — "+
			"giving up until something changes", iface,
			meteredWord(!want), applyLimit)
		return
	}
	if err := netif.SetMetered(iface, want); err != nil {
		c.setErr(err.Error())
		c.say("apply", "metered: %v", err)
		return
	}
	c.setErr("")
	c.reported = ""
	log.Printf("%s marked %s (sim %d)", iface, meteredWord(want), simID)
}

func meteredWord(on bool) string {
	if on {
		return "low-data (expensive, constrained)"
	}
	return "unmetered"
}

func (c *Controller) setErr(msg string) {
	c.mu.Lock()
	c.lastErr = msg
	c.mu.Unlock()
}

func (c *Controller) err() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastErr
}

// State reports the setting and the link as they stand, for one SIM. Only the
// card in the dongle has a link to compare against; for any other SIM this is
// just the stored setting.
func (c *Controller) State(simID int64) (State, error) {
	want, err := c.store.Metered(simID)
	if err != nil {
		return State{}, err
	}
	st := State{SimID: simID, Enabled: want, Privileged: netif.Privileged(),
		Error: c.err()}
	if simID != c.sims.CurrentID() {
		return st, nil
	}
	iface, link := c.traffic.Link()
	st.Iface, st.Present, st.Up, st.Applied = iface, link.Exists, link.Up, link.Marked()
	return st, nil
}

// Set records the metered setting for a SIM and, when that card is the one in
// the dongle, applies it to the link immediately rather than waiting for the
// next tick.
func (c *Controller) Set(simID int64, on bool) (State, error) {
	if err := c.store.SetMetered(simID, on); err != nil {
		return State{}, err
	}
	if simID == c.sims.CurrentID() {
		c.applyMu.Lock()
		c.reported = "" // a hand-made change is always worth a line
		c.reconcileLocked()
		c.applyMu.Unlock()
	}
	return c.State(simID)
}
