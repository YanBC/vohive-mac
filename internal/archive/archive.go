// SMS archiver: periodically drains the modem's hardware message storages
// (SIM "SM" and modem flash "ME") into the SQLite store, then — unless
// disabled — deletes archived messages from the hardware so the small slot
// pools (50 on SIM, 23 in flash) never fill up and start bouncing messages.
//
// Concatenated messages are only archived (and deleted) once all parts are
// present, except that incomplete groups older than partGracePeriod are
// archived as-is with a "[x/y parts]" marker so stragglers can't pin slots
// forever.
package archive

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/YanBC/vohive-mac/internal/modem"
	"github.com/YanBC/vohive-mac/internal/pdu"
	"github.com/YanBC/vohive-mac/internal/sims"
	"github.com/YanBC/vohive-mac/internal/store"
)

const partGracePeriod = 24 * time.Hour

var smsStorages = []string{"SM", "ME"}

// Archiver drains the modem's hardware message storages into the store.
type Archiver struct {
	modem       *modem.Modem
	store       *store.Store
	sims        *sims.Registry
	deleteAfter bool

	mu       sync.Mutex
	lastSync time.Time
	stop     chan struct{}
}

// New returns an archiver; call Start to drain on a timer.
func New(m *modem.Modem, s *store.Store, reg *sims.Registry, deleteAfter bool) *Archiver {
	return &Archiver{modem: m, store: s, sims: reg, deleteAfter: deleteAfter,
		stop: make(chan struct{})}
}

func (a *Archiver) Start() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		a.SyncIfStale(0) //nolint:errcheck
		for {
			select {
			case <-ticker.C:
				a.SyncIfStale(0) //nolint:errcheck
			case <-a.stop:
				return
			}
		}
	}()
}

func (a *Archiver) Stop() { close(a.stop) }

// SyncIfStale runs a sync unless one completed within maxAge. It is safe to
// call from request handlers; concurrent calls serialize on the mutex.
func (a *Archiver) SyncIfStale(maxAge time.Duration) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if maxAge > 0 && time.Since(a.lastSync) < maxAge {
		return nil
	}
	var firstErr error
	for _, storage := range smsStorages {
		if err := a.syncStorage(storage); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		a.lastSync = time.Now()
	}
	return firstErr
}

func (a *Archiver) syncStorage(storage string) error {
	msgs, err := a.modem.ListStorage(storage)
	if err != nil {
		return fmt.Errorf("list %s: %w", storage, err)
	}
	if len(msgs) == 0 {
		return nil
	}

	// Messages are credited to the SIM now in the dongle. For "SM" that is
	// exact — the storage is on the card itself. For "ME" (modem flash) it is
	// an assumption: a message received under a previous card and never drained
	// would be credited to the current one. Draining every 60 s keeps that
	// window to messages that arrived while the app was not running.
	simID := a.sims.CurrentID()

	var deletable []int
	archive := func(sender, ts, body string, indexes ...int) {
		fresh, err := a.store.ArchiveInbound(simID, sender, ts, body)
		if err != nil {
			log.Printf("archive from %s failed: %v", storage, err)
			return // keep on modem; retry next sync
		}
		if fresh {
			log.Printf("archived sms from %s (%s, %d part(s))", sender, storage, len(indexes))
		}
		deletable = append(deletable, indexes...)
	}

	type key struct {
		sender string
		ref    int
	}
	groups := map[key]map[int]pdu.Record{}
	for _, m := range msgs {
		if m.ConcatRef >= 0 {
			k := key{m.Sender, m.ConcatRef}
			if groups[k] == nil {
				groups[k] = map[int]pdu.Record{}
			}
			groups[k][m.ConcatSeq] = m
		} else {
			archive(m.Sender, m.Timestamp, m.Text, m.Index)
		}
	}

	for _, parts := range groups {
		seqs := make([]int, 0, len(parts))
		total := 0
		var indexes []int
		for s, p := range parts {
			seqs = append(seqs, s)
			total = p.ConcatTot
			indexes = append(indexes, p.Index)
		}
		sort.Ints(seqs)
		first := parts[seqs[0]]
		complete := len(parts) >= total
		if !complete && !olderThan(first.Timestamp, partGracePeriod) {
			continue // wait for the remaining parts
		}
		var sb strings.Builder
		for _, s := range seqs {
			sb.WriteString(parts[s].Text)
		}
		if !complete {
			fmt.Fprintf(&sb, " …[%d/%d parts]", len(parts), total)
		}
		archive(first.Sender, first.Timestamp, sb.String(), indexes...)
	}

	if a.deleteAfter {
		if err := a.modem.DeleteMessages(storage, deletable); err != nil {
			return fmt.Errorf("delete from %s: %w", storage, err)
		}
	}
	return nil
}

// olderThan reports whether an SMS timestamp ("2006-01-02 15:04:05") is older
// than d. Unparseable timestamps count as old so they can't pin slots.
func olderThan(ts string, d time.Duration) bool {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", ts, time.Local)
	if err != nil {
		return true
	}
	return time.Since(t) > d
}
