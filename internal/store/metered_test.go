package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/YanBC/vohive-mac/internal/sim"
)

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
	for _, card := range sims {
		if !card.Metered {
			t.Errorf("sim %d (%s) came out of the migration unmetered", card.ID, card.Label)
		}
		if card.ID != UnknownSIM && card.Label != "travel sim" {
			t.Errorf("sim %d label = %q, want the stored one", card.ID, card.Label)
		}
	}
}

// TestMeteredIsPerSIM: the flag follows the card, not the dongle — calling one
// SIM's plan free must not free the other, and the choice must survive a
// restart rather than being re-defaulted by the migration.
func TestMeteredIsPerSIM(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)

	capped, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	unlimited, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000002"})
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
	}{{capped, true}, {unlimited, false}, {UnknownSIM, true}} {
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

// TestMigrateKeepsDeliberateOptOut: turning metered *off* for a card is a
// choice, and the v4 default must not undo it on the next start — the same
// property the label and history-adoption guards protect.
func TestMigrateKeepsDeliberateOptOut(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	unlimited, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000009"})
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
