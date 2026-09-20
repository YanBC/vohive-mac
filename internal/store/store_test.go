package store

import (
	"database/sql"
	"path/filepath"
	"testing"

	"vohive-mac/internal/sim"
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
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
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

	msgs, err := s.ListMessages(UnknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages after migration, want 2", len(msgs))
	}

	days, err := s.RecentDays(UnknownSIM, 100)
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

	msgs, err := s2.ListMessages(UnknownSIM, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("got %d messages after reopen, want 2", len(msgs))
	}
	days, err := s2.RecentDays(UnknownSIM, 100)
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

	simID, err := s.EnsureSIM(sim.Identity{
		ICCID: "89860025128748043019", Number: "+8613700137000", Operator: "CMCC"})
	if err != nil {
		t.Fatal(err)
	}
	if simID == UnknownSIM {
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

	if left, err := s.ListMessages(UnknownSIM, 100); err != nil {
		t.Fatal(err)
	} else if len(left) != 0 {
		t.Errorf("%d messages still on the unknown SIM", len(left))
	}
	if left, err := s.RecentDays(UnknownSIM, 100); err != nil {
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

	simID, err := s.EnsureSIM(sim.Identity{ICCID: "89860025128748043019"})
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
	moved, skipped, err := s.ReassignMessages(UnknownSIM, []int64{msgs[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 || skipped != 0 {
		t.Fatalf("ReassignMessages: moved=%d skipped=%d, want 1/0", moved, skipped)
	}
	s.Close()

	// restart: adoption must not undo the correction
	s2 := openStore(t, dir)
	simID2, err := s2.EnsureSIM(sim.Identity{ICCID: "89860025128748043019"})
	if err != nil {
		t.Fatal(err)
	}
	if m, d, err := s2.AdoptUnknown(simID2); err != nil || m != 0 || d != 0 {
		t.Errorf("AdoptUnknown after restart: (%d, %d, %v), want (0, 0, nil)", m, d, err)
	}
	left, err := s2.ListMessages(UnknownSIM, 100)
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

	simA, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000001"})
	if err != nil {
		t.Fatal(err)
	}
	simB, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000002"})
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

	simA, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000001", Number: "+8613700137001"})
	if err != nil {
		t.Fatal(err)
	}
	simB, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000002"})
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

	id, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000003"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSIMLabel(id, "work sim"); err != nil {
		t.Fatal(err)
	}
	// seen again, now with a number the operator has since provisioned
	if _, err := s.EnsureSIM(sim.Identity{
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

// TestEnsureSIMUpgradesGeneratedLabel: a card first seen at its PIN prompt has
// only a readable ICCID, so the label generated for it is the ICCID tail. Once
// the PIN is entered and the real identity arrives, that placeholder must give
// way — it was never a name the user chose.
func TestEnsureSIMUpgradesGeneratedLabel(t *testing.T) {
	s := openStore(t, t.TempDir())

	// PIN-locked: AT+CIMI/+CNUM/+COPS all refuse, only the ICCID answers
	locked := sim.Identity{ICCID: "8986000000000000004"}
	id, err := s.EnsureSIM(locked)
	if err != nil {
		t.Fatal(err)
	}
	if got := labelOf(t, s, id); got != locked.ICCID {
		t.Fatalf("label while locked = %q, want the ICCID %q", got, locked.ICCID)
	}

	// unlocked: the same card, now fully readable
	open := sim.Identity{ICCID: "8986000000000000004", IMSI: "460099948800096",
		Number: "+8613700137004", Operator: "Mi Mobile"}
	if _, err := s.EnsureSIM(open); err != nil {
		t.Fatal(err)
	}
	if got := labelOf(t, s, id); got != "+8613700137004" {
		t.Errorf("label after unlock = %q, want it to follow the number", got)
	}
}

// TestEnsureSIMUpgradesStaleGeneratedLabel: the label and the identity beside
// it are not written in lockstep — older builds refreshed number/operator on
// every sighting while freezing the label — so a row can hold a generated
// label that no longer matches any default its own identity would produce
// today. That label is still not the user's, and must still give way.
func TestEnsureSIMUpgradesStaleGeneratedLabel(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)

	id, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000005"})
	if err != nil {
		t.Fatal(err)
	}
	// reproduce the divergence: identity moves on, label left behind — in the
	// truncated form generated labels used to take, which is what a database
	// written by an older build actually holds
	if _, err := s.db.Exec(
		`UPDATE sims SET number = ?, operator = ?, label = ? WHERE id = ?`,
		"+8613700137005", "Mi Mobile", "…000005", id); err != nil {
		t.Fatal(err)
	}
	if got := labelOf(t, s, id); got != "…000005" {
		t.Fatalf("setup: label = %q, want the stale ICCID tail", got)
	}

	if _, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000005",
		Number: "+8613700137005", Operator: "Mi Mobile"}); err != nil {
		t.Fatal(err)
	}
	if got := labelOf(t, s, id); got != "+8613700137005" {
		t.Errorf("label = %q, want the stale placeholder replaced by the number", got)
	}
}

// TestEnsureSIMKeepsIdentityWhenLocked: a PIN-locked card answers only its
// ICCID — AT+CIMI/+CNUM/+COPS all refuse — so a sighting taken at the PIN
// prompt carries blanks. Unplugging and replugging the dongle produces exactly
// that sighting, and it must not erase what the card gave up while it was
// open, nor drop its label back to the ICCID tail.
func TestEnsureSIMKeepsIdentityWhenLocked(t *testing.T) {
	s := openStore(t, t.TempDir())

	open := sim.Identity{ICCID: "8986000000000000006", IMSI: "460099948800096",
		Number: "+8613700137006", Operator: "Mi Mobile"}
	id, err := s.EnsureSIM(open)
	if err != nil {
		t.Fatal(err)
	}
	// replugged: the card is back at its PIN prompt
	if _, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000006"}); err != nil {
		t.Fatal(err)
	}

	var got sim.Identity
	var label string
	if err := s.db.QueryRow(
		`SELECT imsi, number, operator, label FROM sims WHERE id = ?`, id).
		Scan(&got.IMSI, &got.Number, &got.Operator, &label); err != nil {
		t.Fatal(err)
	}
	if got.IMSI != open.IMSI || got.Number != open.Number || got.Operator != open.Operator {
		t.Errorf("identity after a locked sighting = %+v, want it preserved as %+v", got, open)
	}
	if label != open.Number {
		t.Errorf("label = %q, want it to stay %q", label, open.Number)
	}
}

// TestEnsureSIMKeepsUserLabelWhenLocked: the same replug must not disturb a
// name the user chose either.
func TestEnsureSIMKeepsUserLabelWhenLocked(t *testing.T) {
	s := openStore(t, t.TempDir())

	id, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000007",
		Number: "+8613700137007", Operator: "Mi Mobile"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSIMLabel(id, "travel sim"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000007"}); err != nil {
		t.Fatal(err)
	}
	if got := labelOf(t, s, id); got != "travel sim" {
		t.Errorf("label = %q, want %q", got, "travel sim")
	}
}

// TestMigrateExpandsTruncatedLabels: v1 stored generated labels as the last
// six ICCID digits, which is not enough to identify a card against the number
// printed on it. The migration spells them out — without touching a name the
// user typed, which is the whole reason it matches on the generated forms
// rather than rewriting every label it finds.
func TestMigrateExpandsTruncatedLabels(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)

	bare := sim.Identity{ICCID: "8986000000000000006"}
	withOp := sim.Identity{ICCID: "8986000000000000007", Operator: "Mi Mobile"}
	named := sim.Identity{ICCID: "8986000000000000008"}
	ids := map[string]int64{}
	for _, id := range []sim.Identity{bare, withOp, named} {
		rowID, err := s.EnsureSIM(id)
		if err != nil {
			t.Fatal(err)
		}
		ids[id.ICCID] = rowID
	}
	// wind the database back to what v1 wrote
	for _, l := range []struct {
		id    int64
		label string
	}{
		{ids[bare.ICCID], "…000006"},
		{ids[withOp.ICCID], "Mi Mobile …000007"},
		{ids[named.ICCID], "travel sim"},
	} {
		if _, err := s.db.Exec(`UPDATE sims SET label = ? WHERE id = ?`,
			l.label, l.id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = openStore(t, dir)
	for _, want := range []struct {
		id    int64
		label string
	}{
		{ids[bare.ICCID], bare.ICCID},
		{ids[withOp.ICCID], "Mi Mobile " + withOp.ICCID},
		{ids[named.ICCID], "travel sim"},
	} {
		if got := labelOf(t, s, want.id); got != want.label {
			t.Errorf("sim %d label = %q, want %q", want.id, got, want.label)
		}
	}
}

func labelOf(t *testing.T, s *Store, simID int64) string {
	t.Helper()
	sims, err := s.ListSIMs()
	if err != nil {
		t.Fatal(err)
	}
	for _, sim := range sims {
		if sim.ID == simID {
			return sim.Label
		}
	}
	t.Fatalf("sim %d missing from ListSIMs", simID)
	return ""
}

// TestMonthUsage: the month total covers only the asked month and SIM, and
// excludes the day the tracker holds in memory (whose store row may be stale).
func TestMonthUsage(t *testing.T) {
	s := openStore(t, t.TempDir())

	simA, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000004"})
	if err != nil {
		t.Fatal(err)
	}
	simB, err := s.EnsureSIM(sim.Identity{ICCID: "8986000000000000005"})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		sim    int64
		day    string
		rx, tx uint64
	}{
		{simA, "2026-06-30", 999, 999}, // previous month
		{simA, "2026-07-01", 100, 10},
		{simA, "2026-07-11", 200, 20},
		{simA, "2026-07-12", 5, 5}, // stale write-through of "today"
		{simB, "2026-07-05", 777, 777},
	} {
		if err := s.SetDayUsage(d.sim, DayUsage{Day: d.day, Rx: d.rx, Tx: d.tx}); err != nil {
			t.Fatal(err)
		}
	}

	rx, tx, err := s.MonthUsage(simA, "2026-07", "2026-07-12")
	if err != nil {
		t.Fatal(err)
	}
	if rx != 300 || tx != 30 {
		t.Errorf("month total rx=%d tx=%d, want 300/30 (other month, other SIM and excluded day must not count)", rx, tx)
	}

	// a month with no rows sums to zero, not an error
	if rx, tx, err = s.MonthUsage(simA, "2026-01", "2026-01-15"); err != nil {
		t.Fatal(err)
	} else if rx != 0 || tx != 0 {
		t.Errorf("empty month rx=%d tx=%d, want 0/0", rx, tx)
	}
}
