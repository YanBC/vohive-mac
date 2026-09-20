package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// realIfconfig is `ifconfig -v en6` for the dongle as macOS prints it,
// unmarked. Verbose is the only mode that prints the eflags/xflags lists at
// all, which is why readLinkState asks for it. The cases below edit the flag
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
		want linkState
	}{
		{"unmarked", realIfconfig, linkState{exists: true, up: true, routableV4: true}},
		{"marked", marked, linkState{exists: true, up: true, expensive: true, constrained: true}},
		{"ultra constrained is not constrained", ultra, linkState{exists: true}},
		{"flag last in list", trailing, linkState{exists: true, up: true, expensive: true, constrained: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseLinkState(tc.out); got != tc.want {
				t.Errorf("parseLinkState = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestLinkStateMarkedNeedsBothFlags(t *testing.T) {
	// half-applied is not applied: ifconfig sets the two flags in one call,
	// so a link carrying only one is a link something else has been at
	for _, tc := range []struct {
		l    linkState
		want bool
	}{
		{linkState{exists: true, expensive: true, constrained: true}, true},
		{linkState{exists: true, expensive: true}, false},
		{linkState{exists: true, constrained: true}, false},
		{linkState{}, false},
	} {
		if got := tc.l.marked(); got != tc.want {
			t.Errorf("%+v.marked() = %v, want %v", tc.l, got, tc.want)
		}
	}
}

// simsSchemaV2 is the `sims` table as v2 wrote it — no metered column.
const simsSchemaV2 = `
CREATE TABLE sims (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    iccid      TEXT NOT NULL UNIQUE,
    imsi       TEXT NOT NULL DEFAULT '',
    number     TEXT NOT NULL DEFAULT '',
    operator   TEXT NOT NULL DEFAULT '',
    label      TEXT NOT NULL DEFAULT '',
    first_seen TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    last_seen  TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
INSERT INTO sims (id, iccid, label) VALUES (0, '', 'unknown SIM');
INSERT INTO sims (iccid, label) VALUES ('8986000000000000010', 'travel sim');
PRAGMA user_version = 2;
`

// TestMigrateAddsMeteredColumn: a v2 database gains the column without losing
// its cards, and every one of them arrives metered — the default a card is
// assumed to be on until someone calls its plan free.
func TestMigrateAddsMeteredColumn(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "vohive.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(simsSchemaV2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s := openStore(t, dir)
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}
	sims, err := s.ListSIMs()
	if err != nil {
		t.Fatal(err)
	}
	if len(sims) != 2 {
		t.Fatalf("got %d sims, want 2 (the unknown bucket and the card)", len(sims))
	}
	for _, sim := range sims {
		if !sim.Metered {
			t.Errorf("sim %d (%s) came out of the migration unmetered", sim.ID, sim.Label)
		}
		if sim.ID != unknownSIM && sim.Label != "travel sim" {
			t.Errorf("sim %d label = %q, want the stored one", sim.ID, sim.Label)
		}
	}
}

// TestMeteredIsPerSIM: the flag follows the card, not the dongle — calling one
// SIM's plan free must not free the other, and the choice must survive a
// restart rather than being re-defaulted by the migration.
func TestMeteredIsPerSIM(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)

	capped, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	unlimited, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	// new cards start metered; this one is on an unlimited plan
	if err := s.SetMetered(unlimited, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, dir)
	for _, tc := range []struct {
		simID int64
		want  bool
	}{{capped, true}, {unlimited, false}, {unknownSIM, true}} {
		got, err := s.Metered(tc.simID)
		if err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("Metered(%d) = %v, want %v", tc.simID, got, tc.want)
		}
	}

	// and it can be turned back on
	if err := s.SetMetered(unlimited, true); err != nil {
		t.Fatal(err)
	}
	if on, err := s.Metered(unlimited); err != nil || !on {
		t.Errorf("Metered(%d) = %v, %v after setting; want true, nil", unlimited, on, err)
	}
}

// TestMeteredUnknownSIM: reading a card with no row is not an error — the
// reconciler asks about whatever CurrentID() returns, which during a dongle
// blip can be a SIM nothing was written for. It follows the default, so the
// reconciler holds the flags on rather than stripping them off a live link.
// Writing such a card is still an error.
func TestMeteredUnknownSIM(t *testing.T) {
	s := openStore(t, t.TempDir())

	on, err := s.Metered(4242)
	if err != nil {
		t.Errorf("Metered(4242) errored: %v", err)
	}
	if !on {
		t.Error("Metered(4242) = false, want the default for a SIM with no row")
	}
	if err := s.SetMetered(4242, true); err == nil {
		t.Error("SetMetered on a nonexistent SIM succeeded, want an error")
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
	if got := parseLinkState(plainIfconfig); got.expensive || got.constrained {
		t.Fatalf("non-verbose output reported flags: %+v", got)
	}
}

// TestShouldApplyStopsRetrying: ifconfig exiting 0 is not proof the flags
// stuck. When a link keeps reading back unmarked, the reconciler must stop
// re-applying rather than spawn a subprocess and log a line every tick — and
// must start over the moment the interface or the wanted state changes.
func TestShouldApplyStopsRetrying(t *testing.T) {
	c := &MeteredController{}
	for i := 1; i <= applyLimit; i++ {
		if !c.shouldApply("en6", true) {
			t.Fatalf("attempt %d refused, want it allowed (limit %d)", i, applyLimit)
		}
	}
	if c.shouldApply("en6", true) {
		t.Errorf("attempt %d allowed, want it refused", applyLimit+1)
	}

	// a replug under a new name is a fresh situation
	if !c.shouldApply("en7", true) {
		t.Error("a new interface was refused, want a fresh budget")
	}
	// so is the user turning the setting the other way
	if !c.shouldApply("en7", false) {
		t.Error("the opposite change was refused, want a fresh budget")
	}
	// and so is the link finally reading back as asked
	c.attempt = applyAttempt{iface: "en7", want: false, n: applyLimit + 5}
	c.applied()
	if !c.shouldApply("en7", false) {
		t.Error("refused after the link came good, want the counter reset")
	}
}

// TestMigrateKeepsDeliberateOptOut: turning metered *off* for a card is a
// choice, and the v4 default must not undo it on the next start — the same
// property the label and history-adoption guards protect.
func TestMigrateKeepsDeliberateOptOut(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	unlimited, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000009"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetMetered(unlimited, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// reopening runs migrate() again; the version guard must keep it off the
	// rows this time
	s = openStore(t, dir)
	if on, err := s.Metered(unlimited); err != nil || on {
		t.Errorf("Metered(%d) = %v, %v after restart; the opt-out was undone",
			unlimited, on, err)
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
	got := parseLinkState(apipaIfconfig)
	if !got.up || got.routableV4 {
		t.Errorf("parseLinkState = %+v, want up with no routable IPv4", got)
	}
}
