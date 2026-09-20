package netif

import "testing"

// realIfconfig is `ifconfig -v en6` for the dongle as macOS prints it,
// unmarked. Verbose is the only mode that prints the eflags/xflags lists at
// all, which is why readLink asks for it. The cases below edit the flag
// lists of this same output.
const realIfconfig = `en6: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500 index 28
	eflags=440008c0<ACCEPT_RTADV,TXSTART,ARPLL,CHANNEL_DRV,FASTLN_ON>
	xflags=4000000<INBAND_WAKE_PKT>
	options=6464<VLAN_MTU,TSO4,TSO6,CHANNEL_IO,PARTIAL_CSUM,ZEROINVERT_CSUM>
	ether 3a:d2:f7:24:24:44
	inet 192.168.225.27 netmask 0xffffff00 broadcast 192.168.225.255
	media: autoselect (100baseTX <full-duplex>)
	status: active
	type: Ethernet
`

// TestParseLinkState covers the two ways this parse can lie: reading the ECM
// link as up when the interface merely exists (it stays "inactive" after a
// sleep/wake wedge), and reading CONSTRAINED off ULTRA_CONSTRAINED, a
// different flag ifconfig prints in the same comma-separated list.
func TestParseLinkState(t *testing.T) {
	marked := `en6: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	eflags=440008c0<ACCEPT_RTADV,TXSTART,EXPENSIVE,ARPLL>
	xflags=4000000<INBAND_WAKE_PKT,CONSTRAINED>
	status: active
`
	// the flag whose name contains the one we look for
	ultra := `en6: flags=8863<UP,BROADCAST> mtu 1500
	eflags=440008c0<ACCEPT_RTADV,TXSTART>
	xflags=4000000<ULTRA_CONSTRAINED>
	status: inactive
`
	// trailing position in the list: the delimiter on the right is '>'
	trailing := `en6: flags=8863<UP> mtu 1500
	eflags=8c0<TXSTART,EXPENSIVE>
	xflags=4000000<INBAND_WAKE_PKT,CONSTRAINED>
	status: active
`
	for _, tc := range []struct {
		name string
		out  string
		want LinkState
	}{
		{"unmarked", realIfconfig, LinkState{Exists: true, Up: true, RoutableV4: true}},
		{"marked", marked, LinkState{Exists: true, Up: true, Expensive: true, Constrained: true}},
		{"ultra constrained is not constrained", ultra, LinkState{Exists: true}},
		{"flag last in list", trailing, LinkState{Exists: true, Up: true, Expensive: true, Constrained: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLink(tc.out); got != tc.want {
				t.Errorf("parseLink = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLinkStateMarkedNeedsBothFlags(t *testing.T) {
	// half-applied is not applied: ifconfig sets the two flags in one call,
	// so a link carrying only one is a link something else has been at
	for _, tc := range []struct {
		l    LinkState
		want bool
	}{
		{LinkState{Exists: true, Expensive: true, Constrained: true}, true},
		{LinkState{Exists: true, Expensive: true}, false},
		{LinkState{Exists: true, Constrained: true}, false},
		{LinkState{}, false},
	} {
		if got := tc.l.Marked(); got != tc.want {
			t.Errorf("%+v.Marked() = %v, want %v", tc.l, got, tc.want)
		}
	}
}

// plainIfconfig is the same interface without -v: no eflags/xflags lines, so
// the low-data flags are invisible however they are actually set. Reading this
// form would leave a marked link looking unmarked forever, and the reconciler
// re-applying it on every tick.
const plainIfconfig = `en6: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500
	options=6464<VLAN_MTU,TSO4,TSO6,CHANNEL_IO,PARTIAL_CSUM,ZEROINVERT_CSUM>
	ether 3a:d2:f7:24:24:44
	inet 192.168.225.27 netmask 0xffffff00 broadcast 192.168.225.255
	media: autoselect (100baseTX <full-duplex>)
	status: active
`

func TestParseLinkStateNonVerboseHidesFlags(t *testing.T) {
	if got := parseLink(plainIfconfig); got.Expensive || got.Constrained {
		t.Fatalf("non-verbose output reported flags: %+v", got)
	}
}

// apipaIfconfig is the dongle as macOS 27 printed it while the failure this
// parse exists for was happening: the ECM link is up and carrying IPv6
// perfectly well, and the only IPv4 address is the 169.254 one macOS
// self-assigns after its DHCP client gives up on finding a server.
const apipaIfconfig = `en6: flags=8863<UP,BROADCAST,SMART,RUNNING,SIMPLEX,MULTICAST> mtu 1500 constrained
	options=6464<VLAN_MTU,TSO4,TSO6,CHANNEL_IO,PARTIAL_CSUM,ZEROINVERT_CSUM>
	ether fe:87:cb:a7:d3:c0
	inet6 fe80::ce6:611f:8a3:33c3%en6 prefixlen 64 secured scopeid 0x1a 
	inet6 2408:8456:3211:585c:892:bb58:3ce0:25db prefixlen 64 autoconf secured 
	inet 169.254.182.86 netmask 0xffff0000 broadcast 169.254.255.255
	nd6 options=201<PERFORMNUD,DAD>
	media: autoselect (100baseTX <full-duplex>)
	status: active
`

// TestHasRoutableV4 covers the ways "does this link have IPv4" can be read
// wrong: counting the self-assigned 169.254 address as a real one (the whole
// failure would then be invisible), matching an inet6 line as if it were
// IPv4 (this link always has several, and they work), and discarding a real
// lease because a stale link-local address is still sitting next to it.
func TestHasRoutableV4(t *testing.T) {
	both := apipaIfconfig + "\tinet 192.168.225.22 netmask 0xffffff00 broadcast 192.168.225.255\n"
	v6only := `en6: flags=8863<UP,BROADCAST> mtu 1500
	inet6 fe80::ce6:611f:8a3:33c3%en6 prefixlen 64 secured scopeid 0x1a 
	status: active
`
	for _, tc := range []struct {
		name string
		out  string
		want bool
	}{
		{"real lease", realIfconfig, true},
		{"self-assigned only", apipaIfconfig, false},
		{"ipv6 only", v6only, false},
		{"no addresses at all", "en6: flags=8863<UP> mtu 1500\n\tstatus: active\n", false},
		{"real lease alongside a stale link-local", both, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasRoutableV4(tc.out); got != tc.want {
				t.Errorf("hasRoutableV4 = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestParseLinkStateAPIPA is the state the watchdog keys on: up, so the
// link-down reset path leaves it alone, and no IPv4, so healDHCP takes it.
func TestParseLinkStateAPIPA(t *testing.T) {
	got := parseLink(apipaIfconfig)
	if !got.Up || got.RoutableV4 {
		t.Errorf("parseLink = %+v, want up with no routable IPv4", got)
	}
}
