package engine

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
)

// Engine maintains per-player difficulty state and adjusts it in fixed
// periods. Each player is owned by a dedicated goroutine, so ingestion for
// one player never blocks on another player's adjustment. Reads go through
// immutable snapshots and never block.
type Engine struct {
	cfg       atomic.Pointer[Config]
	store     Store
	workersMu sync.Mutex
	workers   map[string]*playerWorker
	tickMu    sync.Mutex
	workerWg  sync.WaitGroup
}

// Option configures an Engine.
type Option func(*Engine)

// WithStore sets the persistence store. Defaults to NewMemoryStore.
func WithStore(s Store) Option {
	return func(e *Engine) { e.store = s }
}

// NewEngine creates an engine and recovers any persisted state.
func NewEngine(opts ...Option) (*Engine, error) {
	e := &Engine{
		workers: make(map[string]*playerWorker),
	}
	for _, opt := range opts {
		opt(e)
	}
	if e.store == nil {
		e.store = NewMemoryStore()
	}
	cfg := DefaultConfig()
	loaded := false
	if loadedCfg, err := e.store.LoadConfig(); err != nil {
		return nil, err
	} else if loadedCfg != nil {
		cfg = *loadedCfg
		loaded = true
	}
	if !loaded {
		// Version 0 marks the factory default that has never been
		// explicitly committed; the first UpdateConfig must use version 1.
		cfg.Version = 0
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	e.cfg.Store(&cfg)
	if err := e.recover(); err != nil {
		return nil, err
	}
	return e, nil
}

// Config returns a copy of the current configuration.
func (e *Engine) Config() Config {
	return *e.cfg.Load()
}

// UpdateConfig validates and atomically swaps in a new configuration.
// The new version must be strictly greater than the current one. The
// swap is a single atomic operation: observers see either the old or the
// new configuration, never a partial state.
func (e *Engine) UpdateConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	current := e.cfg.Load()
	if cfg.Version <= current.Version {
		return ErrConfigVersion
	}
	if err := e.store.WriteConfig(cfg); err != nil {
		return err
	}
	e.cfg.Store(&cfg)
	return nil
}

// Ingest submits a performance signal for a player. The result is
// synchronous with respect to the player's worker: when Ingest returns,
// the signal has been validated, deduplicated, ordered and applied (or
// rejected), and the returned difficulty is the complete post-ingest
// state.
func (e *Engine) Ingest(ctx context.Context, sig Signal) (Result, error) {
	if sig.ID == "" || sig.PlayerID == "" || sig.Seq <= 0 ||
		math.IsNaN(sig.Value) || sig.Value < 0 || sig.Value > 1 {
		return Result{Status: StatusInvalid, PlayerID: sig.PlayerID, Seq: sig.Seq,
			Reason: "signal failed shape validation"}, nil
	}
	w := e.getOrCreateWorker(sig.PlayerID)
	if e.store != nil {
		if err := e.store.AppendWAL(sig.PlayerID, sig); err != nil {
			return Result{}, err
		}
	}
	ch := make(chan Result, 1)
	select {
	case w.ch <- ingestMsg{sig: sig, done: ch}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	select {
	case r := <-ch:
		return r, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}

// Tick advances one adjustment period. Every player with pending signals
// is adjusted exactly once; idle players relax slowly toward the default.
// The returned map covers all known players.
func (e *Engine) Tick(ctx context.Context) (map[string]TickInfo, error) {
	e.tickMu.Lock()
	defer e.tickMu.Unlock()

	ws := e.workersSnapshot()
	results := make(map[string]TickInfo, len(ws))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, w := range ws {
		wg.Add(1)
		go func(w *playerWorker) {
			defer wg.Done()
			ch := make(chan TickInfo, 1)
			select {
			case w.ch <- tickMsg{done: ch}:
			case <-ctx.Done():
				return
			}
			select {
			case info := <-ch:
				mu.Lock()
				results[w.id] = info
				mu.Unlock()
			case <-ctx.Done():
			}
		}(w)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Flush waits until every signal ingested so far has been processed by
// its player's worker.
func (e *Engine) Flush(ctx context.Context) error {
	ws := e.workersSnapshot()
	for _, w := range ws {
		ch := make(chan struct{}, 1)
		select {
		case w.ch <- flushMsg{done: ch}:
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// GetDifficulty returns an immutable, self-consistent snapshot of a
// player's state. The snapshot is published atomically by the worker, so
// reads never block and never observe a partially updated state.
func (e *Engine) GetDifficulty(playerID string) (PlayerSnapshot, bool) {
	e.workersMu.Lock()
	w, ok := e.workers[playerID]
	e.workersMu.Unlock()
	if !ok {
		return PlayerSnapshot{}, false
	}
	s := w.snap.Load()
	if s == nil {
		return PlayerSnapshot{}, false
	}
	return *s, true
}

// Players returns the IDs of all known players.
func (e *Engine) Players() []string {
	e.workersMu.Lock()
	defer e.workersMu.Unlock()
	out := make([]string, 0, len(e.workers))
	for id := range e.workers {
		out = append(out, id)
	}
	return out
}

// Close stops all worker goroutines and closes the store.
func (e *Engine) Close() error {
	e.workersMu.Lock()
	for _, w := range e.workers {
		close(w.quit)
	}
	e.workersMu.Unlock()
	e.workerWg.Wait()
	if e.store != nil {
		return e.store.Close()
	}
	return nil
}

func (e *Engine) config() *Config {
	return e.cfg.Load()
}

func (e *Engine) workersSnapshot() []*playerWorker {
	e.workersMu.Lock()
	defer e.workersMu.Unlock()
	out := make([]*playerWorker, 0, len(e.workers))
	for _, w := range e.workers {
		out = append(out, w)
	}
	return out
}

func (e *Engine) getOrCreateWorker(id string) *playerWorker {
	e.workersMu.Lock()
	defer e.workersMu.Unlock()
	if w, ok := e.workers[id]; ok {
		return w
	}
	w := newPlayerWorker(e, id, e.cfg.Load())
	e.workers[id] = w
	e.workerWg.Add(1)
	go w.run()
	return w
}

// recover rebuilds engine state from the store. Committed snapshots are
// restored directly; WAL entries are replayed through the same apply path
// used for live ingest, which makes recovery idempotent and keeps
// in-flight (not-yet-ticked) signals distinguishable from committed
// state. Workers are created and fully restored before their goroutines
// start, so recovery never races with the event loop.
func (e *Engine) recover() error {
	if e.store == nil {
		return nil
	}
	snapshots, wal, err := e.store.Load()
	if err != nil {
		return err
	}
	cfg := e.cfg.Load()

	e.workersMu.Lock()
	for id, data := range snapshots {
		w := newPlayerWorker(e, id, cfg)
		w.restore(data)
		e.workers[id] = w
	}
	for id, sigs := range wal {
		w := e.workers[id]
		if w == nil {
			w = newPlayerWorker(e, id, cfg)
			e.workers[id] = w
		}
		for _, sig := range sigs {
			w.apply(sig, cfg)
		}
	}
	for _, w := range e.workers {
		w.publish()
	}
	for _, w := range e.workers {
		e.workerWg.Add(1)
		go w.run()
	}
	e.workersMu.Unlock()
	return nil
}
