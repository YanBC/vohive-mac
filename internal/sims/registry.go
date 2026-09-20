// SIM registry: tracks which card is currently in the dongle and maps it to a
// row in the `sims` table, so messages and usage can be attributed to it.
//
// The card is not fixed for the life of the process — it can be swapped, and
// after a swap the number, the operator and the bill all change. Everything
// that writes SIM-scoped rows asks the registry (a mutex read, no USB I/O);
// only this file's poller touches the modem, so the 3 s traffic sampler never
// blocks on the AT port.
package sims

import (
	"log"
	"sync"
	"time"

	"github.com/YanBC/vohive-mac/internal/modem"
	"github.com/YanBC/vohive-mac/internal/sim"
	"github.com/YanBC/vohive-mac/internal/store"
)

const pollInterval = 30 * time.Second

// Registry tracks which card is in the dongle and which `sims` row it maps to.
type Registry struct {
	modem *modem.Modem
	store *store.Store

	mu   sync.RWMutex
	id   int64
	info sim.Identity
	ok   bool

	adoptOnce sync.Once
	stop      chan struct{}
}

// New returns a registry; call Refresh for the first reading and Start to
// keep it current.
func New(m *modem.Modem, s *store.Store) *Registry {
	return &Registry{modem: m, store: s, id: store.UnknownSIM, stop: make(chan struct{})}
}

func (r *Registry) Start() {
	go func() {
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		r.Refresh()
		for {
			select {
			case <-ticker.C:
				r.Refresh()
			case <-r.stop:
				return
			}
		}
	}()
}

func (r *Registry) Stop() { close(r.stop) }

// Refresh re-reads the SIM from the modem and upserts it. A read failure (AT
// port down, dongle unplugged, modem rebooting) is not a swap: the last known
// SIM stays current, so traffic and messages keep their attribution instead of
// falling into the unknown bucket every time the dongle blips.
func (r *Registry) Refresh() {
	info, err := r.modem.SIMInfo()
	if err != nil {
		return
	}
	r.mu.RLock()
	unchanged := r.ok && r.info == info
	prev := r.info
	hadPrev := r.ok
	r.mu.RUnlock()
	if unchanged {
		return
	}

	id, err := r.store.EnsureSIM(info)
	if err != nil {
		log.Printf("record SIM %s: %v", info.ICCID, err)
		return
	}
	if hadPrev && prev.ICCID != info.ICCID {
		log.Printf("SIM swapped: %s -> %s (sim %d)", prev.DefaultLabel(), info.DefaultLabel(), id)
	} else if !hadPrev {
		log.Printf("SIM: %s (iccid %s, sim %d)", info.DefaultLabel(), info.ICCID, id)
	}

	r.mu.Lock()
	r.id, r.info, r.ok = id, info, true
	r.mu.Unlock()

	// The first SIM identified in this process claims the history that predates
	// SIM attribution (see Store.AdoptUnknown).
	r.adoptOnce.Do(func() {
		msgs, days, err := r.store.AdoptUnknown(id)
		if err != nil {
			log.Printf("adopt pre-SIM history: %v", err)
			return
		}
		if msgs > 0 || days > 0 {
			log.Printf("attributed %d message(s) and %d usage day(s) to %s",
				msgs, days, info.DefaultLabel())
		}
	})
}

// Current returns the SIM in the dongle. ok is false until one is identified;
// callers that must write a row anyway use store.UnknownSIM.
func (r *Registry) Current() (id int64, info sim.Identity, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.id, r.info, r.ok
}

// CurrentID is Current() for callers that only need somewhere to put the row.
func (r *Registry) CurrentID() int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.id
}
