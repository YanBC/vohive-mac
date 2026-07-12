// SQLite persistence: SIM identities, archived SMS messages, data-usage history.
//
// Everything the user accumulates belongs to one SIM: the dongle is a card
// reader, and swapping the card swaps the phone number, the operator and the
// bill. Rows in `messages` and `usage` therefore carry a sim_id pointing at
// `sims`. SIMs are keyed on ICCID rather than the phone number because
// AT+CNUM is blank on many prepaid/MVNO SIMs — the number is a label, not an
// identity. sim_id 0 is the reserved "unknown SIM" row: it holds rows written
// before any SIM was identified (including everything migrated from the
// pre-SIM schema), until AdoptUnknown() attributes them.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// schemaVersion is stored in PRAGMA user_version. 1 introduced per-SIM rows.
const schemaVersion = 1

// unknownSIM is the sim_id of the reserved catch-all row.
const unknownSIM int64 = 0

const schema = `
CREATE TABLE IF NOT EXISTS sims (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    iccid      TEXT NOT NULL UNIQUE,   -- '' only for the reserved unknown row
    imsi       TEXT NOT NULL DEFAULT '',
    number     TEXT NOT NULL DEFAULT '',  -- MSISDN (AT+CNUM); often blank
    operator   TEXT NOT NULL DEFAULT '',
    label      TEXT NOT NULL DEFAULT '',  -- user-facing name, defaults to number
    first_seen TEXT NOT NULL DEFAULT (datetime('now','localtime')),
    last_seen  TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
INSERT OR IGNORE INTO sims (id, iccid, label) VALUES (0, '', 'unknown SIM');

CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    sim_id     INTEGER NOT NULL DEFAULT 0,
    direction  TEXT NOT NULL CHECK (direction IN ('in','out')),
    peer       TEXT NOT NULL,          -- sender for 'in', recipient for 'out'
    body       TEXT NOT NULL,
    ts         TEXT NOT NULL,          -- SMS timestamp ("2006-01-02 15:04:05")
    dedup      TEXT UNIQUE,            -- content hash for inbound dedup; NULL for out
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_messages_sim_ts ON messages (sim_id, ts DESC);

CREATE TABLE IF NOT EXISTS usage (
    sim_id INTEGER NOT NULL DEFAULT 0,
    day    TEXT NOT NULL,              -- "2006-01-02"
    rx     INTEGER NOT NULL DEFAULT 0,
    tx     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (sim_id, day)
);

CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// metaAdopted marks that the pre-SIM history has been claimed. It is a
// once-ever event, not once-per-process: rows the user later moves *back* to
// the unknown SIM (messages from a card this app never saw) must stay there
// across restarts instead of being re-adopted.
const metaAdopted = "history_adopted"

type Store struct {
	db *sql.DB
}

func OpenStore(dataDir string) (*Store, error) {
	path := filepath.Join(dataDir, "vohive.db")
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single-writer simplicity; this app is low-traffic
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db}
	s.importLegacyUsage(filepath.Join(dataDir, "usage.json"))
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ---------------------------------------------------------------- migration

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version >= schemaVersion {
		return nil
	}
	// A pre-v1 database has `messages`/`usage` without sim_id. `messages`
	// takes the column with an ALTER; `usage` needs a rebuild because sim_id
	// joins its primary key. Both land existing rows on the unknown SIM.
	legacyMessages, err := tableExists(db, "messages") // NB: before schema runs
	if err != nil {
		return err
	}
	legacyUsage, err := tableExists(db, "usage")
	if err != nil {
		return err
	}
	if legacyUsage {
		if hasSim, err := columnExists(db, "usage", "sim_id"); err != nil {
			return err
		} else if hasSim {
			legacyUsage = false
		}
	}
	if legacyMessages {
		if hasSim, err := columnExists(db, "messages", "sim_id"); err != nil {
			return err
		} else if hasSim {
			legacyMessages = false
		}
	}

	// Both legacy tables are brought to the v1 shape *before* `schema` runs:
	// its CREATE TABLE IF NOT EXISTS would skip them, but its indexes would
	// then be built against columns that do not exist yet.
	if legacyMessages {
		// No REFERENCES clause: SQLite rejects adding a foreign-key column
		// with a non-NULL default, and a fresh DB must get the same shape.
		if _, err := db.Exec(fmt.Sprintf(
			`ALTER TABLE messages ADD COLUMN sim_id INTEGER NOT NULL DEFAULT %d`,
			unknownSIM)); err != nil {
			return err
		}
		// superseded by idx_messages_sim_ts; every query is SIM-scoped now
		if _, err := db.Exec(`DROP INDEX IF EXISTS idx_messages_ts`); err != nil {
			return err
		}
	}
	if legacyUsage {
		if _, err := db.Exec(`ALTER TABLE usage RENAME TO usage_pre_sim`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	if legacyUsage {
		if _, err := db.Exec(`INSERT INTO usage (sim_id, day, rx, tx)
		    SELECT ?, day, rx, tx FROM usage_pre_sim`, unknownSIM); err != nil {
			return err
		}
		if _, err := db.Exec(`DROP TABLE usage_pre_sim`); err != nil {
			return err
		}
	}
	_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion))
	return err
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`,
		name).Scan(&n)
	return n > 0, err
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull int
		var dflt sql.NullString
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

// importLegacyUsage moves pre-SQLite usage.json data into the usage table,
// then renames the file so the import runs only once. It predates SIM
// attribution, so it lands on the unknown SIM like the rest of the old rows.
func (s *Store) importLegacyUsage(jsonPath string) {
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return
	}
	var days []DayUsage
	if json.Unmarshal(raw, &days) != nil {
		return
	}
	for _, d := range days {
		s.db.Exec(`INSERT INTO usage (sim_id, day, rx, tx) VALUES (?,?,?,?)
		           ON CONFLICT(sim_id, day) DO UPDATE SET
		           rx = MAX(rx, excluded.rx), tx = MAX(tx, excluded.tx)`,
			unknownSIM, d.Day, d.Rx, d.Tx) //nolint:errcheck
	}
	os.Rename(jsonPath, jsonPath+".imported") //nolint:errcheck
}

// ------------------------------------------------------------------- sims

// SIM is a card the dongle has held, as recorded in the database.
type SIM struct {
	ID        int64  `json:"id"`
	ICCID     string `json:"iccid"`
	IMSI      string `json:"imsi,omitempty"`
	Number    string `json:"number,omitempty"`
	Operator  string `json:"operator,omitempty"`
	Label     string `json:"label"`
	FirstSeen string `json:"first_seen"`
	LastSeen  string `json:"last_seen"`
	Messages  int    `json:"messages"` // how much history hangs off this SIM,
	Bytes     uint64 `json:"bytes"`    // so the UI can hide empty buckets
}

// EnsureSIM upserts the SIM currently in the dongle and returns its row id.
// Identity is the ICCID; number/operator/IMSI are refreshed on every sighting
// (a number can be provisioned onto a SIM later), but a non-empty label is
// never overwritten — that one belongs to the user.
func (s *Store) EnsureSIM(id SIMIdentity) (int64, error) {
	if id.ICCID == "" {
		return unknownSIM, fmt.Errorf("SIM has no ICCID")
	}
	_, err := s.db.Exec(`INSERT INTO sims (iccid, imsi, number, operator, label)
	    VALUES (?,?,?,?,?)
	    ON CONFLICT(iccid) DO UPDATE SET
	        imsi      = excluded.imsi,
	        number    = excluded.number,
	        operator  = excluded.operator,
	        label     = CASE WHEN sims.label = '' THEN excluded.label ELSE sims.label END,
	        last_seen = datetime('now','localtime')`,
		id.ICCID, id.IMSI, id.Number, id.Operator, id.defaultLabel())
	if err != nil {
		return unknownSIM, err
	}
	var rowID int64
	err = s.db.QueryRow(`SELECT id FROM sims WHERE iccid = ?`, id.ICCID).Scan(&rowID)
	return rowID, err
}

func (s *Store) ListSIMs() ([]SIM, error) {
	rows, err := s.db.Query(`SELECT s.id, s.iccid, s.imsi, s.number, s.operator,
	    s.label, s.first_seen, s.last_seen,
	    (SELECT COUNT(*) FROM messages m WHERE m.sim_id = s.id),
	    (SELECT COALESCE(SUM(u.rx + u.tx), 0) FROM usage u WHERE u.sim_id = s.id)
	    FROM sims s ORDER BY s.last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SIM
	for rows.Next() {
		var m SIM
		if err := rows.Scan(&m.ID, &m.ICCID, &m.IMSI, &m.Number, &m.Operator,
			&m.Label, &m.FirstSeen, &m.LastSeen, &m.Messages, &m.Bytes); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) SetSIMLabel(simID int64, label string) error {
	_, err := s.db.Exec(`UPDATE sims SET label = ? WHERE id = ?`, label, simID)
	return err
}

// AdoptUnknown attributes every row still parked on the unknown SIM to simID.
// It runs once ever (guarded by meta[metaAdopted]), when the first SIM is
// identified, so that history predating SIM attribution — everything migrated
// from the old schema — is not stranded in a separate bucket.
//
// It is a guess: the pre-SIM archive can contain messages drained from a card
// that was in the dongle years ago. Wrong guesses are corrected with
// ReassignMessages, and the guard is what makes those corrections stick.
func (s *Store) AdoptUnknown(simID int64) (messages, days int, err error) {
	if simID == unknownSIM {
		return 0, 0, nil
	}
	var done string
	switch err := s.db.QueryRow(`SELECT value FROM meta WHERE key = ?`,
		metaAdopted).Scan(&done); err {
	case nil:
		return 0, 0, nil // already claimed, by this SIM or another
	case sql.ErrNoRows:
	default:
		return 0, 0, err
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful commit

	// Inbound dedup hashes are SIM-scoped, so adopted messages need theirs
	// recomputed or the same message re-listed off the hardware would archive
	// twice (only reachable with -archive-delete=false, but cheap to prevent).
	rows, err := tx.Query(`SELECT id, direction, peer, ts, body
	    FROM messages WHERE sim_id = ?`, unknownSIM)
	if err != nil {
		return 0, 0, err
	}
	type adopted struct {
		id    int64
		dedup sql.NullString
	}
	var list []adopted
	for rows.Next() {
		var id int64
		var direction, peer, ts, body string
		if err := rows.Scan(&id, &direction, &peer, &ts, &body); err != nil {
			rows.Close()
			return 0, 0, err
		}
		a := adopted{id: id}
		if direction == "in" {
			a.dedup = sql.NullString{String: msgHash(simID, peer, ts, body), Valid: true}
		}
		list = append(list, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}
	for _, a := range list {
		if _, err := tx.Exec(`UPDATE messages SET sim_id = ?, dedup = ? WHERE id = ?`,
			simID, a.dedup, a.id); err != nil {
			return 0, 0, err
		}
	}

	// Usage rows merge: a day may already exist for simID if the tracker wrote
	// one before adoption ran. The two accumulations are disjoint, so they add.
	res, err := tx.Exec(`INSERT INTO usage (sim_id, day, rx, tx)
	    SELECT ?, day, rx, tx FROM usage WHERE sim_id = ?
	    ON CONFLICT(sim_id, day) DO UPDATE SET
	        rx = usage.rx + excluded.rx, tx = usage.tx + excluded.tx`,
		simID, unknownSIM)
	if err != nil {
		return 0, 0, err
	}
	nDays, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM usage WHERE sim_id = ?`, unknownSIM); err != nil {
		return 0, 0, err
	}
	if _, err := tx.Exec(`INSERT INTO meta (key, value) VALUES (?, ?)`,
		metaAdopted, strconv.FormatInt(simID, 10)); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(list), int(nDays), nil
}

// ReassignMessages moves messages to another SIM — the manual correction for
// an archive that turned out to hold more than one card's history. Moving to
// unknownSIM is legitimate: it means "a card this app has never seen".
//
// Inbound dedup hashes are SIM-scoped, so they are recomputed. A message whose
// new hash already exists on the target SIM is skipped rather than moved: the
// target already has that exact message, and merging them would mean deleting
// one, which is not this function's call to make.
func (s *Store) ReassignMessages(simID int64, ids []int64) (moved, skipped int, err error) {
	if len(ids) == 0 {
		return 0, 0, nil
	}
	var exists bool
	if err := s.db.QueryRow(`SELECT EXISTS (SELECT 1 FROM sims WHERE id = ?)`,
		simID).Scan(&exists); err != nil {
		return 0, 0, err
	}
	if !exists {
		return 0, 0, fmt.Errorf("no such SIM: %d", simID)
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck — no-op after a successful commit

	for _, id := range ids {
		var direction, peer, ts, body string
		switch err := tx.QueryRow(
			`SELECT direction, peer, ts, body FROM messages WHERE id = ?`,
			id).Scan(&direction, &peer, &ts, &body); err {
		case nil:
		case sql.ErrNoRows:
			skipped++
			continue
		default:
			return 0, 0, err
		}

		var dedup sql.NullString
		if direction == "in" {
			h := msgHash(simID, peer, ts, body)
			var clash bool
			if err := tx.QueryRow(
				`SELECT EXISTS (SELECT 1 FROM messages WHERE dedup = ? AND id != ?)`,
				h, id).Scan(&clash); err != nil {
				return 0, 0, err
			}
			if clash {
				skipped++
				continue
			}
			dedup = sql.NullString{String: h, Valid: true}
		}
		if _, err := tx.Exec(`UPDATE messages SET sim_id = ?, dedup = ? WHERE id = ?`,
			simID, dedup, id); err != nil {
			return 0, 0, err
		}
		moved++
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return moved, skipped, nil
}

// ---------------------------------------------------------------- usage

// SetDayUsage stores the absolute totals for one day on one SIM (write-through
// from the tracker's in-memory accumulation).
func (s *Store) SetDayUsage(simID int64, d DayUsage) error {
	_, err := s.db.Exec(`INSERT INTO usage (sim_id, day, rx, tx) VALUES (?,?,?,?)
	                     ON CONFLICT(sim_id, day) DO UPDATE SET rx=excluded.rx, tx=excluded.tx`,
		simID, d.Day, d.Rx, d.Tx)
	return err
}

func (s *Store) GetDayUsage(simID int64, day string) (DayUsage, error) {
	d := DayUsage{Day: day}
	err := s.db.QueryRow(`SELECT rx, tx FROM usage WHERE sim_id = ? AND day = ?`,
		simID, day).Scan(&d.Rx, &d.Tx)
	if err == sql.ErrNoRows {
		return d, nil
	}
	return d, err
}

func (s *Store) RecentDays(simID int64, n int) ([]DayUsage, error) {
	rows, err := s.db.Query(
		`SELECT day, rx, tx FROM usage WHERE sim_id = ? ORDER BY day DESC LIMIT ?`,
		simID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DayUsage
	for rows.Next() {
		var d DayUsage
		if err := rows.Scan(&d.Day, &d.Rx, &d.Tx); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- messages

type StoredMessage struct {
	ID        int64  `json:"id"`
	SimID     int64  `json:"sim_id"`
	Direction string `json:"direction"`
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
}

func msgHash(simID int64, sender, ts, body string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s\x00%s\x00%s", simID, sender, ts, body))
	return hex.EncodeToString(h[:16])
}

// ArchiveInbound stores an inbound message received on simID; returns false if
// it was already archived (dedup hit). Dedup is per-SIM: the same text from the
// same sender at the same minute on two SIMs is two messages.
func (s *Store) ArchiveInbound(simID int64, sender, ts, body string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages
	    (sim_id, direction, peer, body, ts, dedup) VALUES (?,'in',?,?,?,?)`,
		simID, sender, body, ts, msgHash(simID, sender, ts, body))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) RecordOutbound(simID int64, to, body string) error {
	_, err := s.db.Exec(`INSERT INTO messages (sim_id, direction, peer, body, ts)
	    VALUES (?,'out',?,?,?)`,
		simID, to, body, time.Now().Format("2006-01-02 15:04:05"))
	return err
}

// ListMessages returns the newest messages for one SIM.
func (s *Store) ListMessages(simID int64, limit int) ([]StoredMessage, error) {
	rows, err := s.db.Query(`SELECT id, sim_id, direction, peer, body, ts
	    FROM messages WHERE sim_id = ? ORDER BY ts DESC, id DESC LIMIT ?`,
		simID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.ID, &m.SimID, &m.Direction, &m.Peer, &m.Body,
			&m.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
