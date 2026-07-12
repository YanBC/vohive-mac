package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

// legacySchema is the pre-SIM schema (schemaVersion 0), reproduced verbatim so
// the migration is tested against the shape real databases actually have.
const legacySchema = `
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    direction  TEXT NOT NULL CHECK (direction IN ('in','out')),
    peer       TEXT NOT NULL,
    body       TEXT NOT NULL,
    ts         TEXT NOT NULL,
    dedup      TEXT UNIQUE,
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages (ts DESC);

CREATE TABLE IF NOT EXISTS usage (
    day TEXT PRIMARY KEY,
    rx  INTEGER NOT NULL DEFAULT 0,
    tx  INTEGER NOT NULL DEFAULT 0
);
`

// seedLegacyDB writes a v0 database with two messages and two usage days.
func seedLegacyDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", filepath.Join(dir, "vohive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	// the old dedup hash had no SIM in it: sha256(sender \x00 ts \x00 body)
	if _, err := db.Exec(`INSERT INTO messages (direction, peer, body, ts, dedup)
	    VALUES ('in','10086','balance low','2026-07-08 09:00:00','oldhash1'),
	           ('out','+8613800138000','ack','2026-07-08 09:01:00',NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO usage (day, rx, tx) VALUES
	    ('2026-07-08', 889838593, 48201317), ('2026-07-09', 6877, 93997)`); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := OpenStore(dir)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestMigrateLegacyPreservesHistory: upgrading a v0 database must not lose or
// double-count anything; every old row lands on the unknown SIM.
func TestMigrateLegacyPreservesHistory(t *testing.T) {
	s := openStore(t, seedLegacyDB(t))

	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Errorf("user_version = %d, want %d", version, schemaVersion)
	}

	msgs, err := s.ListMessages(unknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages after migration, want 2", len(msgs))
	}

	days, err := s.RecentDays(unknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("got %d usage days after migration, want 2", len(days))
	}
	var rx, tx uint64
	for _, d := range days {
		rx += d.Rx
		tx += d.Tx
	}
	if rx != 889845470 || tx != 48295314 {
		t.Errorf("usage totals after migration: rx=%d tx=%d, want rx=889845470 tx=48295314", rx, tx)
	}
}

// TestMigrateIsIdempotent: reopening an already-migrated database must be a
// no-op, not a second migration that duplicates rows.
func TestMigrateIsIdempotent(t *testing.T) {
	dir := seedLegacyDB(t)
	s1 := openStore(t, dir)
	s1.Close()
	s2 := openStore(t, dir)

	msgs, err := s2.ListMessages(unknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages after reopen, want 2", len(msgs))
	}
	days, err := s2.RecentDays(unknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Errorf("got %d usage days after reopen, want 2", len(days))
	}
}

// TestAdoptUnknown: the first SIM identified claims the pre-SIM history, byte
// totals survive the move, and a day already recorded for that SIM merges
// rather than overwrites.
func TestAdoptUnknown(t *testing.T) {
	s := openStore(t, seedLegacyDB(t))

	simID, err := s.EnsureSIM(SIMIdentity{
		ICCID: "89860025128748043019", Number: "+8613700137000", Operator: "CMCC"})
	if err != nil {
		t.Fatal(err)
	}
	if simID == unknownSIM {
		t.Fatal("EnsureSIM returned the unknown SIM id")
	}
	// a day the tracker already wrote for this SIM before adoption ran
	if err := s.SetDayUsage(simID, DayUsage{Day: "2026-07-08", Rx: 1000, Tx: 500}); err != nil {
		t.Fatal(err)
	}

	msgs, days, err := s.AdoptUnknown(simID)
	if err != nil {
		t.Fatal(err)
	}
	if msgs != 2 {
		t.Errorf("adopted %d messages, want 2", msgs)
	}
	if days != 2 {
		t.Errorf("adopted %d usage days, want 2", days)
	}

	if left, err := s.ListMessages(unknownSIM, 100); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("%d messages still on the unknown SIM", len(left))
	}
	if left, err := s.RecentDays(unknownSIM, 100); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("%d usage days still on the unknown SIM", len(left))
	}

	if got, err := s.ListMessages(simID, 100); err != nil {
		t.Fatal(err)
	} else if len(got) != 2 {
		t.Errorf("SIM has %d messages after adoption, want 2", len(got))
	}
	// 889838593 adopted + 1000 already there
	d, err := s.GetDayUsage(simID, "2026-07-08")
	if err != nil {
		t.Fatal(err)
	}
	if d.Rx != 889839593 || d.Tx != 48201817 {
		t.Errorf("merged day: rx=%d tx=%d, want rx=889839593 tx=48201817", d.Rx, d.Tx)
	}

	// adopted inbound messages must carry SIM-scoped dedup hashes, or the same
	// message re-listed off the hardware would archive a second time
	fresh, err := s.ArchiveInbound(simID, "10086", "2026-07-08 09:00:00", "balance low")
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Error("re-archiving an adopted message inserted a duplicate")
	}

	// running again finds nothing left to adopt
	if m, d, err := s.AdoptUnknown(simID); err != nil || m != 0 || d != 0 {
		t.Errorf("second AdoptUnknown: (%d, %d, %v), want (0, 0, nil)", m, d, err)
	}
}

// TestReassignSurvivesRestart: the whole point of the once-ever adoption guard.
// A message moved back to the unknown SIM (it came off a card this app never
// saw) must not be swept onto the current SIM again on the next startup.
func TestReassignSurvivesRestart(t *testing.T) {
	dir := seedLegacyDB(t)
	s := openStore(t, dir)

	simID, err := s.EnsureSIM(SIMIdentity{ICCID: "89860025128748043019"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AdoptUnknown(simID); err != nil {
		t.Fatal(err)
	}
	msgs, err := s.ListMessages(simID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("adoption left %d messages on the SIM, want 2", len(msgs))
	}

	// "this one is from my old carrier's SIM" — send it back to the unknown SIM
	moved, skipped, err := s.ReassignMessages(unknownSIM, []int64{msgs[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 || skipped != 0 {
		t.Fatalf("ReassignMessages: moved=%d skipped=%d, want 1/0", moved, skipped)
	}
	s.Close()

	// restart: adoption must not undo the correction
	s2 := openStore(t, dir)
	simID2, err := s2.EnsureSIM(SIMIdentity{ICCID: "89860025128748043019"})
	if err != nil {
		t.Fatal(err)
	}
	if m, d, err := s2.AdoptUnknown(simID2); err != nil || m != 0 || d != 0 {
		t.Errorf("AdoptUnknown after restart: (%d, %d, %v), want (0, 0, nil)", m, d, err)
	}
	left, err := s2.ListMessages(unknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 1 {
		t.Errorf("unknown SIM has %d messages after restart, want the 1 moved back", len(left))
	}
}

// TestReassignSkipsDuplicate: moving a message onto a SIM that already has the
// identical message must not blow up on the dedup UNIQUE index.
func TestReassignSkipsDuplicate(t *testing.T) {
	s := openStore(t, t.TempDir())

	simA, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	simB, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	for _, sim := range []int64{simA, simB} {
		if _, err := s.ArchiveInbound(sim, "10086", "2026-07-12 10:00:00", "hello"); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := s.ListMessages(simA, 10)
	if err != nil {
		t.Fatal(err)
	}

	moved, skipped, err := s.ReassignMessages(simB, []int64{msgs[0].ID})
	if err != nil {
		t.Fatalf("ReassignMessages: %v", err)
	}
	if moved != 0 || skipped != 1 {
		t.Errorf("moved=%d skipped=%d, want 0/1 (target already has it)", moved, skipped)
	}
	if got, err := s.ListMessages(simA, 10); err != nil {
		t.Fatal(err)
	} else if len(got) != 1 {
		t.Errorf("sim A lost the message it kept: %d rows", len(got))
	}
}

// TestPerSIMIsolation: two SIMs must not see each other's messages or usage,
// and identical message content on each must not be deduped away.
func TestPerSIMIsolation(t *testing.T) {
	s := openStore(t, t.TempDir())

	simA, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000001", Number: "+8613700137001"})
	if err != nil {
		t.Fatal(err)
	}
	simB, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	if simA == simB {
		t.Fatal("two ICCIDs collapsed to one SIM row")
	}

	// same sender, same minute, same text — one message per SIM, not one total
	for _, sim := range []int64{simA, simB} {
		fresh, err := s.ArchiveInbound(sim, "10086", "2026-07-12 10:00:00", "hello")
		if err != nil {
			t.Fatal(err)
		}
		if !fresh {
			t.Errorf("sim %d: identical text on another SIM was deduped away", sim)
		}
	}
	// but a repeat on the same SIM is still a dedup hit
	if fresh, err := s.ArchiveInbound(simA, "10086", "2026-07-12 10:00:00", "hello"); err != nil {
		t.Fatal(err)
	} else if fresh {
		t.Error("same message archived twice on one SIM")
	}

	if err := s.SetDayUsage(simA, DayUsage{Day: "2026-07-12", Rx: 100, Tx: 10}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDayUsage(simB, DayUsage{Day: "2026-07-12", Rx: 7, Tx: 3}); err != nil {
		t.Fatal(err)
	}
	if d, err := s.GetDayUsage(simA, "2026-07-12"); err != nil {
		t.Fatal(err)
	} else if d.Rx != 100 || d.Tx != 10 {
		t.Errorf("sim A usage leaked: rx=%d tx=%d, want 100/10", d.Rx, d.Tx)
	}
	if msgs, err := s.ListMessages(simB, 100); err != nil {
		t.Fatal(err)
	} else if len(msgs) != 1 {
		t.Errorf("sim B sees %d messages, want 1", len(msgs))
	}
}

// TestEnsureSIMKeepsUserLabel: re-seeing a SIM refreshes its number/operator
// but must not clobber a name the user chose.
func TestEnsureSIMKeepsUserLabel(t *testing.T) {
	s := openStore(t, t.TempDir())

	id, err := s.EnsureSIM(SIMIdentity{ICCID: "8986000000000000003"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSIMLabel(id, "work sim"); err != nil {
		t.Fatal(err)
	}
	// seen again, now with a number the operator has since provisioned
	if _, err := s.EnsureSIM(SIMIdentity{
		ICCID: "8986000000000000003", Number: "+8613700137003", Operator: "CMCC"}); err != nil {
		t.Fatal(err)
	}

	sims, err := s.ListSIMs()
	if err != nil {
		t.Fatal(err)
	}
	for _, sim := range sims {
		if sim.ID != id {
			continue
		}
		if sim.Label != "work sim" {
			t.Errorf("label = %q, want %q", sim.Label, "work sim")
		}
		if sim.Number != "+8613700137003" {
			t.Errorf("number = %q, want it refreshed to +8613700137003", sim.Number)
		}
		return
	}
	t.Fatalf("sim %d missing from ListSIMs", id)
}
