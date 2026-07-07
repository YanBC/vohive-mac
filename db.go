// SQLite persistence: archived SMS messages and data-usage history.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    direction  TEXT NOT NULL CHECK (direction IN ('in','out')),
    peer       TEXT NOT NULL,          -- sender for 'in', recipient for 'out'
    body       TEXT NOT NULL,
    ts         TEXT NOT NULL,          -- SMS timestamp ("2006-01-02 15:04:05")
    dedup      TEXT UNIQUE,            -- content hash for inbound dedup; NULL for out
    created_at TEXT NOT NULL DEFAULT (datetime('now','localtime'))
);
CREATE INDEX IF NOT EXISTS idx_messages_ts ON messages (ts DESC);

CREATE TABLE IF NOT EXISTS usage (
    day TEXT PRIMARY KEY,              -- "2006-01-02"
    rx  INTEGER NOT NULL DEFAULT 0,
    tx  INTEGER NOT NULL DEFAULT 0
);
`

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
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	s := &Store{db: db}
	s.importLegacyUsage(filepath.Join(dataDir, "usage.json"))
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// importLegacyUsage moves pre-SQLite usage.json data into the usage table,
// then renames the file so the import runs only once.
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
		s.db.Exec(`INSERT INTO usage (day, rx, tx) VALUES (?,?,?)
		           ON CONFLICT(day) DO UPDATE SET
		           rx = MAX(rx, excluded.rx), tx = MAX(tx, excluded.tx)`,
			d.Day, d.Rx, d.Tx) //nolint:errcheck
	}
	os.Rename(jsonPath, jsonPath+".imported") //nolint:errcheck
}

// ---------------------------------------------------------------- usage

// SetDayUsage stores the absolute totals for one day (write-through from the
// tracker's in-memory accumulation).
func (s *Store) SetDayUsage(d DayUsage) error {
	_, err := s.db.Exec(`INSERT INTO usage (day, rx, tx) VALUES (?,?,?)
	                     ON CONFLICT(day) DO UPDATE SET rx=excluded.rx, tx=excluded.tx`,
		d.Day, d.Rx, d.Tx)
	return err
}

func (s *Store) GetDayUsage(day string) (DayUsage, error) {
	d := DayUsage{Day: day}
	err := s.db.QueryRow(`SELECT rx, tx FROM usage WHERE day = ?`, day).
		Scan(&d.Rx, &d.Tx)
	if err == sql.ErrNoRows {
		return d, nil
	}
	return d, err
}

func (s *Store) RecentDays(n int) ([]DayUsage, error) {
	rows, err := s.db.Query(
		`SELECT day, rx, tx FROM usage ORDER BY day DESC LIMIT ?`, n)
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
	Direction string `json:"direction"`
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	Timestamp string `json:"timestamp"`
}

func msgHash(sender, ts, body string) string {
	h := sha256.Sum256([]byte(sender + "\x00" + ts + "\x00" + body))
	return hex.EncodeToString(h[:16])
}

// ArchiveInbound stores an inbound message; returns false if it was already
// archived (dedup hit).
func (s *Store) ArchiveInbound(sender, ts, body string) (bool, error) {
	res, err := s.db.Exec(`INSERT OR IGNORE INTO messages
	    (direction, peer, body, ts, dedup) VALUES ('in',?,?,?,?)`,
		sender, body, ts, msgHash(sender, ts, body))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) RecordOutbound(to, body string) error {
	_, err := s.db.Exec(`INSERT INTO messages (direction, peer, body, ts)
	    VALUES ('out',?,?,?)`,
		to, body, time.Now().Format("2006-01-02 15:04:05"))
	return err
}

func (s *Store) ListMessages(limit int) ([]StoredMessage, error) {
	rows, err := s.db.Query(`SELECT id, direction, peer, body, ts
	    FROM messages ORDER BY ts DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredMessage
	for rows.Next() {
		var m StoredMessage
		if err := rows.Scan(&m.ID, &m.Direction, &m.Peer, &m.Body, &m.Timestamp); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
