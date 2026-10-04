package engine

import (
	"math"
	"sort"
	"sync/atomic"
)

// playerMsg is implemented by messages sent to a player worker.
type playerMsg interface {
	playerMsg()
}

type ingestMsg struct {
	sig  Signal
	done chan<- Result
}

type tickMsg struct {
	done chan<- TickInfo
}

type flushMsg struct {
	done chan<- struct{}
}

func (ingestMsg) playerMsg() {}
func (tickMsg) playerMsg()   {}
func (flushMsg) playerMsg()  {}

// playerWorker owns all mutable state for one player. All state access
// happens on the worker goroutine; external readers use the immutable
// snapshot published after every state change.
type playerWorker struct {
	id  string
	eng *Engine
	ch  chan playerMsg
	quit chan struct{}

	snap atomic.Pointer[PlayerSnapshot]

	difficulty float64
	ewma       float64 // -1 when uninitialized
	lastSeq    int64
	buf        map[int64]*Signal
	seen       map[string]struct{}
	pending    []*Signal
	idleTicks  int
}

func newPlayerWorker(eng *Engine, id string, cfg *Config) *playerWorker {
	w := &playerWorker{
		id:         id,
		eng:        eng,
		ch:         make(chan playerMsg, 4096),
		quit:       make(chan struct{}),
		ewma:       -1,
		difficulty: cfg.DefaultDifficulty,
		buf:        make(map[int64]*Signal),
		seen:       make(map[string]struct{}),
	}
	w.publish()
	return w
}

func (w *playerWorker) run() {
	defer w.eng.workerWg.Done()
	for {
		select {
		case msg := <-w.ch:
			switch m := msg.(type) {
			case ingestMsg:
				cfg := w.eng.config()
				r := w.apply(m.sig, cfg)
				var t *TickInfo
				if r.Status == StatusApplied && len(w.pending) >= cfg.TickSize {
					info := w.tick(cfg)
					t = &info
				}
				r.Tick = t
				w.publish()
				m.done <- r
			case tickMsg:
				cfg := w.eng.config()
				info := w.tick(cfg)
				w.publish()
				m.done <- info
			case flushMsg:
				m.done <- struct{}{}
			}
		case <-w.quit:
			return
		}
	}
}

// apply processes a single signal without persisting or ticking. It is
// shared by the ingest path and WAL replay, making recovery idempotent.
// Signals are ordered per player by Seq: out-of-order signals are held in
// a small reorder buffer, duplicates are dropped, and signals older than
// the reorder window are expired.
func (w *playerWorker) apply(sig Signal, cfg *Config) Result {
	res := Result{PlayerID: w.id, Seq: sig.Seq}

	if sig.ID == "" || sig.PlayerID == "" {
		res.Status = StatusInvalid
		res.Reason = "signal id and player id must be non-empty"
		return w.finish(res)
	}
	if sig.Seq <= 0 {
		res.Status = StatusInvalid
		res.Reason = "seq must be positive"
		return w.finish(res)
	}
	if math.IsNaN(sig.Value) || sig.Value < 0 || sig.Value > 1 {
		res.Status = StatusInvalid
		res.Reason = "value must be a number in [0,1]"
		return w.finish(res)
	}
	if sig.PlayerID != w.id {
		res.Status = StatusInvalid
		res.Reason = "signal player id does not match worker"
		return w.finish(res)
	}
	if _, dup := w.seen[sig.ID]; dup {
		res.Status = StatusDuplicate
		res.Reason = "signal id already processed"
		return w.finish(res)
	}
	if sig.Seq <= w.lastSeq {
		// Already applied, or belongs to a gap that was skipped.
		res.Status = StatusExpired
		res.Reason = "seq already passed"
		return w.finish(res)
	}
	if sig.Seq > w.lastSeq+cfg.LateWindow {
		res.Status = StatusExpired
		res.Reason = "seq outside reorder window"
		return w.finish(res)
	}

	w.seen[sig.ID] = struct{}{}
	s := sig
	w.buf[s.Seq] = &s
	w.drain(cfg)

	res.Status = StatusApplied
	return w.finish(res)
}

// drain moves contiguous buffered signals into the pending list. Signals
// are only ever applied in strict seq order, so the result is independent
// of arrival order. The reorder buffer is bounded by LateWindow (see
// apply), which keeps memory usage bounded without making skip decisions
// that could depend on timing.
func (w *playerWorker) drain(cfg *Config) {
	for {
		if s, ok := w.buf[w.lastSeq+1]; ok {
			delete(w.buf, s.Seq)
			w.pending = append(w.pending, s)
			w.lastSeq = s.Seq
			continue
		}
		return
	}
}

// tick runs one adjustment period for the player. With no pending signals
// the player slowly relaxes toward the default difficulty; otherwise the
// EWMA of recent performance drives a clamped, deadbanded adjustment.
func (w *playerWorker) tick(cfg *Config) TickInfo {
	info := TickInfo{
		OldDifficulty: w.difficulty,
		Ewma:          w.ewma,
	}

	if len(w.pending) == 0 {
		w.idleTicks++
		info.Idle = true
		if w.idleTicks > cfg.IdleTicks {
			step := cfg.DefaultDifficulty - w.difficulty
			if step > cfg.IdleRelax {
				step = cfg.IdleRelax
			} else if step < -cfg.IdleRelax {
				step = -cfg.IdleRelax
			}
			w.difficulty = clamp(w.difficulty+step, cfg.MinDifficulty, cfg.MaxDifficulty)
		}
		info.NewDifficulty = w.difficulty
		w.persist(cfg, &info)
		return info
	}

	w.idleTicks = 0
	applied := w.pending
	throttled := 0
	if len(applied) > cfg.BurstLimit {
		throttled = len(applied) - cfg.BurstLimit
		applied = applied[:cfg.BurstLimit]
	}
	info.Applied = len(applied)
	info.Throttled = throttled

	sum := 0.0
	for _, s := range applied {
		sum += s.Value
	}
	avg := sum / float64(len(applied))
	if w.ewma < 0 {
		w.ewma = avg
	} else {
		w.ewma += cfg.EwmaAlpha * (avg - w.ewma)
	}
	info.Ewma = w.ewma

	target := (cfg.TargetPerfLo + cfg.TargetPerfHi) / 2
	delta := 0.0
	if w.ewma < cfg.TargetPerfLo {
		delta = cfg.AdjustGain * (w.ewma - target) // performance too low -> easier
	} else if w.ewma > cfg.TargetPerfHi {
		delta = cfg.AdjustGain * (w.ewma - target) // performance too high -> harder
	}
	delta = clamp(delta, -cfg.MaxStep, cfg.MaxStep)
	w.difficulty = clamp(w.difficulty+delta, cfg.MinDifficulty, cfg.MaxDifficulty)

	w.pending = w.pending[:0]
	info.Ticked = true
	info.NewDifficulty = w.difficulty
	w.persist(cfg, &info)
	return info
}

// persist writes the committed snapshot and clears the WAL. A crash
// between the two is safe: recovery replays the WAL and skips signals
// already present in the snapshot's seen set.
func (w *playerWorker) persist(cfg *Config, info *TickInfo) {
	if w.eng.store == nil {
		return
	}
	data := PlayerData{
		PlayerID:      w.id,
		Difficulty:    w.difficulty,
		Ewma:          w.ewma,
		LastSeq:       w.lastSeq,
		Seen:          w.sortedSeen(),
		IdleTicks:     w.idleTicks,
		ConfigVersion: cfg.Version,
	}
	if err := w.eng.store.WriteSnapshot(w.id, data); err != nil {
		info.Err = err
		return
	}
	if err := w.eng.store.TruncateWAL(w.id); err != nil {
		info.Err = err
	}
}

func (w *playerWorker) sortedSeen() []string {
	out := make([]string, 0, len(w.seen))
	for id := range w.seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (w *playerWorker) publish() {
	cfg := w.eng.config()
	w.snap.Store(&PlayerSnapshot{
		PlayerID:      w.id,
		Difficulty:    w.difficulty,
		Ewma:          w.ewma,
		LastSeq:       w.lastSeq,
		Pending:       len(w.pending),
		ConfigVersion: cfg.Version,
		IdleTicks:     w.idleTicks,
	})
}

func (w *playerWorker) snapshotData(cfg *Config) PlayerData {
	return PlayerData{
		PlayerID:      w.id,
		Difficulty:    w.difficulty,
		Ewma:          w.ewma,
		LastSeq:       w.lastSeq,
		Seen:          w.sortedSeen(),
		IdleTicks:     w.idleTicks,
		ConfigVersion: cfg.Version,
	}
}

func (w *playerWorker) restore(data PlayerData) {
	w.difficulty = data.Difficulty
	w.ewma = data.Ewma
	w.lastSeq = data.LastSeq
	w.idleTicks = data.IdleTicks
	w.seen = make(map[string]struct{}, len(data.Seen))
	for _, id := range data.Seen {
		w.seen[id] = struct{}{}
	}
	w.buf = make(map[int64]*Signal)
	w.pending = nil
}

// finish stamps the current difficulty onto a result.
func (w *playerWorker) finish(res Result) Result {
	res.Difficulty = w.difficulty
	return res
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
