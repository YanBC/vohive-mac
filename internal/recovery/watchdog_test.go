package recovery

import (
	"testing"
	"time"

	"github.com/YanBC/vohive-mac/internal/netif"
)

func TestRecoveryTimersTrackInterfaceInstance(t *testing.T) {
	priorFailure := time.Unix(100, 0)
	for _, tc := range []struct {
		name      string
		iface     string
		link      netif.LinkState
		wantClear bool
	}{
		{"disconnected with cached name", "en6", netif.LinkState{}, true},
		{"reconnected with same name", "en6", netif.LinkState{Exists: true, Index: "51"}, true},
		{"reconnected with new name", "en7", netif.LinkState{Exists: true, Index: "51"}, true},
		{"same inactive interface", "en6", netif.LinkState{Exists: true, Index: "50"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			timers := recoveryTimers{
				iface: "en6", index: "50", downSince: priorFailure,
				dhcp: dhcpHeal{noV4Since: priorFailure, lastReboot: priorFailure, reboots: 2},
			}
			timers.observe(tc.iface, tc.link)

			wantSince := priorFailure
			if tc.wantClear {
				wantSince = time.Time{}
			}
			if timers.downSince != wantSince || timers.dhcp.noV4Since != wantSince {
				t.Fatalf("failure timers = (%v, %v), want both %v", timers.downSince, timers.dhcp.noV4Since, wantSince)
			}
			if timers.dhcp.reboots != 2 || timers.dhcp.lastReboot != priorFailure {
				t.Fatalf("DHCP reboot budget changed: %+v", timers.dhcp)
			}
		})
	}
}
