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
//
// There is a second failure mode with the opposite signature — the link up
// and healthy, carrying IPv6, but with no IPv4 address at all — which the
// trigger above deliberately does not catch and healDHCP handles instead.
package recovery

import (
	"log"
	"strings"
	"time"

	"github.com/YanBC/vohive-mac/internal/modem"
	"github.com/YanBC/vohive-mac/internal/netif"
)

const (
	watchInterval     = 3 * time.Second
	linkDownThreshold = 10 * time.Second
	resetCooldown     = 60 * time.Second
	wakeSettle        = 15 * time.Second

	// The DHCP failure below is a much slower business than a wedged link:
	// the threshold has to clear a normal lease acquisition (observed at ~9 s
	// from enumeration on this dongle) with room to spare, and the remedy
	// costs a full modem reboot and every connection on the link, so it is
	// rationed far more tightly than a USB reset.
	dhcpFailThreshold = 45 * time.Second
	dhcpRebootCd      = 10 * time.Minute
	// dhcpRebootLimit stops a dongle whose DHCP server is broken for some
	// other reason from being rebooted every cooldown forever. Only an
	// interface that actually comes back with an IPv4 address clears it.
	dhcpRebootLimit = 3
)

// Watchdog recovers the ECM data link after the two failures the dongle's
// firmware leaves behind: a link that never comes back after sleep, and a
// link that comes back without an IPv4 lease.
type Watchdog struct {
	modem   *modem.Modem
	traffic *netif.Tracker
	stop    chan struct{}
}

// New returns a watchdog; call Start to begin watching.
func New(m *modem.Modem, t *netif.Tracker) *Watchdog {
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
	var heal dhcpHeal
	pinNoted := false // "SIM is locked" is logged on entry to that state, not every tick
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

		iface, link := w.traffic.Link()
		if iface == "" {
			// Dongle gone. heal keeps its counters: a replug is not evidence
			// that anything was fixed, and the reboots this watchdog issues
			// make the interface vanish too — zeroing here would reset the
			// limit on every attempt and loop forever.
			downSince, heal.noV4Since = time.Time{}, time.Time{}
			continue
		}
		if link.Up {
			// The link being up is not the same as it carrying traffic: the
			// ECM side can be perfectly alive over IPv6 with no IPv4 lease.
			downSince = time.Time{}
			if link.RoutableV4 {
				heal = dhcpHeal{}
			} else {
				w.healDHCP(iface, &heal)
			}
			continue
		}
		heal.noV4Since = time.Time{}
		// Link down because cellular data is deliberately disabled, or
		// because a commanded modem reboot (data toggle) is still settling,
		// is the expected state, not a wedged ECM — never reset for it.
		if w.modem.DataOff() || w.modem.Rebooting() {
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
		// The data-enabled query doubles as the aliveness probe and covers
		// the case where data was disabled before this process started
		// (DataOff above knows nothing until the modem has been asked once).
		enabled, _, err := w.modem.DataEnabled()
		if err != nil {
			continue
		}
		if !enabled {
			log.Printf("watchdog: %s link down but cellular data is disabled — leaving it alone", iface)
			downSince = time.Time{}
			continue
		}
		// A card waiting for its PIN carries no data, and re-enumerating the
		// dongle only puts the same locked card back. Asked here rather than
		// read from a cache: with no browser polling, nothing else would have
		// looked at +CPIN? recently enough to be trusted.
		if ready, need, err := w.modem.PINReady(); err == nil && !ready {
			if !pinNoted {
				log.Printf("watchdog: %s link down but the SIM is waiting for its %s — leaving it alone",
					iface, strings.ToUpper(need))
				pinNoted = true
			}
			downSince = time.Time{}
			continue
		}
		pinNoted = false
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

// dhcpHeal is the state of the second failure mode: the ECM link is up, but
// macOS has no usable IPv4 address on it.
type dhcpHeal struct {
	noV4Since  time.Time
	lastReboot time.Time
	reboots    int
	gaveUp     bool // the limit was hit and said so; say it once
}

// healDHCP recovers an ECM link that is up but carries no IPv4.
//
// The dongle runs its own DHCP server on the USB LAN (192.168.225.0/24) and
// hands out 12 h leases keyed on the host's MAC address. macOS 27 mints a new
// random locally-administered MAC for the ECM interface on *every*
// enumeration — a replug, a lid-close USB suspend, a USB reset from the
// watchdog above, a modem reboot — so each one arrives at that server as a
// brand-new client and burns another lease. Once the pool is used up the
// server stops answering, macOS self-assigns a 169.254 address, and there is
// no IPv4 route at all.
//
// This is easy to miss, because nothing looks broken: the link stays
// "status: active", the modem stays registered on LTE with a WAN address, and
// IPv6 keeps working throughout (SLAAC needs no server to answer), so the
// link-down watchdog above never fires. What the user sees is "no internet".
//
// The remedy is a modem reboot, which clears the lease table. Renewing from
// this side is not an alternative: macOS is already retrying on its own and
// there is nothing to retry against.
func (w *Watchdog) healDHCP(iface string, h *dhcpHeal) {
	// A link that is deliberately off, or one whose reboot is still settling,
	// has no business having a lease yet.
	if w.modem.DataOff() || w.modem.Rebooting() {
		h.noV4Since = time.Time{}
		return
	}
	if h.noV4Since.IsZero() {
		h.noV4Since = time.Now()
		return
	}
	if time.Since(h.noV4Since) < dhcpFailThreshold || h.gaveUp {
		return
	}
	if !h.lastReboot.IsZero() && time.Since(h.lastReboot) < dhcpRebootCd {
		return
	}
	if h.reboots >= dhcpRebootLimit {
		log.Printf("watchdog: %s still has no IPv4 after %d modem reboots — leaving it alone; "+
			"replug the dongle, or check whether the SIM's data plan is still active", iface, h.reboots)
		h.gaveUp = true
		return
	}
	// Same aliveness and don't-touch-it checks the USB reset path makes: the
	// data-enabled query doubles as the probe that the dongle is still there.
	enabled, _, err := w.modem.DataEnabled()
	if err != nil {
		return
	}
	if !enabled {
		h.noV4Since = time.Time{}
		return
	}
	// A card waiting for its PIN carries no data by design, and a reboot only
	// brings the same locked card back. Asked, not read from a cache, for the
	// reason given on the reset path.
	if ready, _, err := w.modem.PINReady(); err == nil && !ready {
		h.noV4Since = time.Time{}
		return
	}
	log.Printf("watchdog: %s link up but no IPv4 for %s (dhcp unanswered) — rebooting the modem",
		iface, time.Since(h.noV4Since).Round(time.Second))
	if err := w.modem.Reboot(); err != nil {
		log.Printf("watchdog: modem reboot: %v", err)
	}
	h.lastReboot = time.Now()
	h.reboots++
	h.noV4Since = time.Time{}
}
