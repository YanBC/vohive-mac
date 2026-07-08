// ECM link watchdog: auto-recovers the dongle's data link after sleep/wake.
//
// When the USB bus suspends (lid close / screen lock), the QDC507's ECM
// function drops its Ethernet link and — firmware bug — never re-asserts it
// after resume (it stops sending the CDC "NetworkConnection: connected"
// notification). macOS then keeps the interface inactive forever: no DHCP,
// no data path, while the AT function keeps answering and the modem stays
// registered on LTE. The only software remedy is a USB port reset, which
// makes macOS re-enumerate the dongle and the firmware bring ECM up fresh.
//
// Trigger: link down for >= linkDownThreshold while the AT port still
// answers — proof the dongle is attached and it's the ECM side that is
// wedged, not an unplug. Resets are rate-limited by resetCooldown so a
// genuinely dead link (e.g. service disabled in macOS) can't cause a
// tight reset loop.
//
// The down-time must be observed while continuously awake: this process
// freezes during system sleep, so wall-clock time since downSince can span
// a sleep and be satisfied the moment the machine wakes. Resetting the
// device while the USB stack is still resuming has been observed to knock
// the dongle off the bus entirely (libusb reset timeout, device gone until
// replugged). A sleep gap is detected as a wall-clock jump between ticks;
// it clears the timer and imposes wakeSettle of quiet time first.
package main

import (
	"log"
	"time"
)

const (
	watchInterval     = 3 * time.Second
	linkDownThreshold = 10 * time.Second
	resetCooldown     = 60 * time.Second
	wakeSettle        = 15 * time.Second
)

type Watchdog struct {
	modem   *Modem
	traffic *TrafficTracker
	stop    chan struct{}
}

func NewWatchdog(m *Modem, t *TrafficTracker) *Watchdog {
	return &Watchdog{modem: m, traffic: t, stop: make(chan struct{})}
}

func (w *Watchdog) Start() { go w.run() }

func (w *Watchdog) Stop() { close(w.stop) }

func (w *Watchdog) run() {
	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()
	var downSince time.Time
	var lastReset time.Time
	var settleUntil time.Time
	var verifyAt time.Time
	lastTick := time.Now()
	for {
		select {
		case <-w.stop:
			return
		case <-ticker.C:
		}

		// Detect a sleep period as a wall-clock jump between ticks (the
		// monotonic clock is unreliable across sleep, so compare wall time
		// via Round(0)). Ticks are watchInterval apart when running.
		now := time.Now()
		if gap := now.Round(0).Sub(lastTick.Round(0)); gap > 3*watchInterval {
			log.Printf("watchdog: wake detected (%s gap); settling %s before link checks",
				gap.Round(time.Second), wakeSettle)
			downSince = time.Time{}
			settleUntil = now.Add(wakeSettle)
		}
		lastTick = now
		if now.Before(settleUntil) {
			continue
		}

		// After a reset, confirm the dongle actually came back; if it fell
		// off the bus the only remedy is a physical replug — say so.
		if !verifyAt.IsZero() && now.After(verifyAt) {
			verifyAt = time.Time{}
			if !w.modem.Connected() {
				log.Printf("watchdog: dongle did not re-enumerate after usb reset — replug it")
			}
		}

		iface, up := w.traffic.LinkState()
		if up || iface == "" {
			downSince = time.Time{}
			continue
		}
		if downSince.IsZero() {
			downSince = time.Now()
			continue
		}
		if time.Since(downSince) < linkDownThreshold ||
			(!lastReset.IsZero() && time.Since(lastReset) < resetCooldown) {
			continue
		}
		// Only reset if the modem still answers on the AT port; if it
		// doesn't, the dongle is likely unplugged and a reset is pointless.
		if _, err := w.modem.Cmd("AT", 3*time.Second); err != nil {
			continue
		}
		log.Printf("watchdog: %s link down for %s but modem alive — resetting USB device",
			iface, time.Since(downSince).Round(time.Second))
		if err := w.modem.ResetUSB(); err != nil {
			// NOT_FOUND etc. after a successful reset is normal: the device
			// re-enumerated out from under the handle.
			log.Printf("watchdog: usb reset: %v", err)
		}
		lastReset = time.Now()
		downSince = time.Time{}
		verifyAt = lastReset.Add(30 * time.Second)
	}
}
