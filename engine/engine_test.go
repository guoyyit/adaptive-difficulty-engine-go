package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
)

// testConfig returns a configuration with a large tick size so tests can
// drive adjustment periods explicitly via Tick.
func testConfig() Config {
	c := DefaultConfig()
	c.TickSize = 1_000_000
	c.BurstLimit = 1_000_000
	return c
}

func newTestEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func ingest(t *testing.T, e *Engine, sig Signal) Result {
	t.Helper()
	r, err := e.Ingest(context.Background(), sig)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func tickPlayer(t *testing.T, e *Engine, player string) TickInfo {
	t.Helper()
	m, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	info, ok := m[player]
	if !ok {
		t.Fatalf("player %q missing from tick results", player)
	}
	return info
}

func TestBasicAdjustment(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Poor performance (0.1) should trend difficulty downward.
	prev := cfg.DefaultDifficulty
	for k := 0; k < 6; k++ {
		for i := 0; i < 10; i++ {
			seq := int64(k*10 + i + 1)
			ingest(t, e, Signal{ID: fmt.Sprintf("low-%d-%d", k, i), PlayerID: "p", Seq: seq, Value: 0.1})
		}
		info := tickPlayer(t, e, "p")
		if info.NewDifficulty > prev+1e-9 {
			t.Fatalf("poor performance should lower difficulty: %.3f -> %.3f", prev, info.NewDifficulty)
		}
		prev = info.NewDifficulty
	}
	low := prev
	if low >= cfg.DefaultDifficulty {
		t.Fatalf("poor phase should end below default: low=%.3f", low)
	}

	// Strong performance (0.9) should pull EWMA up and eventually trend
	// difficulty upward. The EWMA lags by a few ticks, so only the tail
	// needs to be non-decreasing.
	strongStart := prev
	tailNonDec := 0
	for k := 0; k < 10; k++ {
		for i := 0; i < 10; i++ {
			seq := int64(60 + k*10 + i + 1)
			ingest(t, e, Signal{ID: fmt.Sprintf("high-%d-%d", k, i), PlayerID: "p", Seq: seq, Value: 0.9})
		}
		info := tickPlayer(t, e, "p")
		if info.NewDifficulty >= info.OldDifficulty-1e-9 {
			tailNonDec++
		} else {
			tailNonDec = 0
		}
		prev = info.NewDifficulty
	}
	if prev <= strongStart {
		t.Fatalf("strong phase should end above its start: start=%.3f end=%.3f", strongStart, prev)
	}
	if tailNonDec < 3 {
		t.Fatalf("strong phase should end with non-decreasing difficulty, tail=%d", tailNonDec)
	}
	if prev <= low {
		t.Fatalf("strong phase should end above low point: low=%.3f high=%.3f", low, prev)
	}

	// In-band performance (0.5) should eventually pin difficulty via the
	// deadband: the last few ticks must produce no change.
	zeroTicks := 0
	for k := 0; k < 8; k++ {
		for i := 0; i < 10; i++ {
			seq := int64(160 + k*10 + i + 1)
			ingest(t, e, Signal{ID: fmt.Sprintf("mid-%d-%d", k, i), PlayerID: "p", Seq: seq, Value: 0.5})
		}
		info := tickPlayer(t, e, "p")
		if info.NewDifficulty < cfg.MinDifficulty || info.NewDifficulty > cfg.MaxDifficulty {
			t.Fatalf("difficulty out of range: %.3f", info.NewDifficulty)
		}
		if info.NewDifficulty == info.OldDifficulty {
			zeroTicks++
		} else {
			zeroTicks = 0
		}
	}
	if zeroTicks < 3 {
		t.Fatalf("in-band performance should stabilize difficulty, zeroTicks=%d", zeroTicks)
	}
}

func TestSingleOutlierNoJump(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Steady in-band signals, then one extreme outlier.
	for i := 0; i < 8; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("s-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.5})
	}
	ingest(t, e, Signal{ID: "outlier", PlayerID: "p", Seq: 9, Value: 0.0})
	info := tickPlayer(t, e, "p")
	change := math.Abs(info.NewDifficulty - info.OldDifficulty)
	if change > cfg.MaxStep+1e-9 {
		t.Fatalf("single outlier caused jump of %.3f (max step %.3f)", change, cfg.MaxStep)
	}
	if info.NewDifficulty < cfg.MinDifficulty || info.NewDifficulty > cfg.MaxDifficulty {
		t.Fatalf("difficulty out of range: %.3f", info.NewDifficulty)
	}
}

func TestDifficultyAlwaysClamped(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Extreme losing streak: difficulty must never leave [min,max].
	for i := 0; i < 200; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("l-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.0})
		tickPlayer(t, e, "p")
		snap, _ := e.GetDifficulty("p")
		if snap.Difficulty < cfg.MinDifficulty || snap.Difficulty > cfg.MaxDifficulty {
			t.Fatalf("difficulty out of range at step %d: %.3f", i, snap.Difficulty)
		}
	}
	// Extreme winning streak back up.
	for i := 0; i < 200; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("w-%d", i), PlayerID: "p", Seq: int64(i + 201), Value: 1.0})
		tickPlayer(t, e, "p")
		snap, _ := e.GetDifficulty("p")
		if snap.Difficulty < cfg.MinDifficulty || snap.Difficulty > cfg.MaxDifficulty {
			t.Fatalf("difficulty out of range at step %d: %.3f", i, snap.Difficulty)
		}
	}
}

func TestInvalidSignals(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Create the player with one valid signal first.
	ingest(t, e, Signal{ID: "valid-1", PlayerID: "p", Seq: 1, Value: 0.5})

	cases := []Signal{
		{ID: "", PlayerID: "p", Seq: 2, Value: 0.5},
		{ID: "x", PlayerID: "", Seq: 2, Value: 0.5},
		{ID: "x", PlayerID: "p", Seq: 0, Value: 0.5},
		{ID: "x", PlayerID: "p", Seq: -1, Value: 0.5},
		{ID: "x", PlayerID: "p", Seq: 2, Value: -0.1},
		{ID: "x", PlayerID: "p", Seq: 2, Value: 1.5},
		{ID: "x", PlayerID: "p", Seq: 2, Value: math.NaN()},
	}
	for i, sig := range cases {
		r := ingest(t, e, sig)
		if r.Status != StatusInvalid {
			t.Fatalf("case %d: expected invalid, got %s", i, r.Status)
		}
	}
	snap, ok := e.GetDifficulty("p")
	if !ok {
		t.Fatal("player should exist after valid signal")
	}
	if snap.Pending != 1 {
		t.Fatalf("invalid signals must not accumulate pending: %d", snap.Pending)
	}
}

func TestDuplicateSignalsNotAccumulated(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	sig := Signal{ID: "dup-1", PlayerID: "p", Seq: 1, Value: 0.9}
	r1 := ingest(t, e, sig)
	if r1.Status != StatusApplied {
		t.Fatalf("first ingest: expected applied, got %s", r1.Status)
	}
	r2 := ingest(t, e, sig)
	if r2.Status != StatusDuplicate {
		t.Fatalf("duplicate ingest: expected duplicate, got %s", r2.Status)
	}
	// Same id, different seq: still duplicate.
	r3 := ingest(t, e, Signal{ID: "dup-1", PlayerID: "p", Seq: 5, Value: 0.1})
	if r3.Status != StatusDuplicate {
		t.Fatalf("same id diff seq: expected duplicate, got %s", r3.Status)
	}
	snap, _ := e.GetDifficulty("p")
	if snap.Pending != 1 {
		t.Fatalf("duplicates must not accumulate: pending=%d", snap.Pending)
	}
}

func TestOutOfOrderDelivery(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Deliver seqs in shuffled order; the engine must reorder them.
	order := []int64{4, 1, 3, 2, 6, 5}
	for _, seq := range order {
		r := ingest(t, e, Signal{
			ID: fmt.Sprintf("s-%d", seq), PlayerID: "p", Seq: seq, Value: 0.5,
		})
		if r.Status != StatusApplied {
			t.Fatalf("seq %d: expected applied, got %s", seq, r.Status)
		}
	}
	snap, _ := e.GetDifficulty("p")
	if snap.LastSeq != 6 {
		t.Fatalf("expected lastSeq 6, got %d", snap.LastSeq)
	}
	if snap.Pending != 6 {
		t.Fatalf("expected 6 pending, got %d", snap.Pending)
	}

	// A duplicate of an already-applied signal is still a duplicate.
	r := ingest(t, e, Signal{ID: "s-3", PlayerID: "p", Seq: 3, Value: 0.5})
	if r.Status != StatusDuplicate {
		t.Fatalf("expected duplicate, got %s", r.Status)
	}
}

func TestLateSignalExpired(t *testing.T) {
	cfg := testConfig()
	cfg.LateWindow = 4
	e := newTestEngine(t, cfg)

	// Advance lastSeq to 10 by delivering 1..10.
	for i := int64(1); i <= 10; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("s-%d", i), PlayerID: "p", Seq: i, Value: 0.5})
	}
	// A signal far behind the reorder window is expired.
	r := ingest(t, e, Signal{ID: "late", PlayerID: "p", Seq: 1, Value: 0.9})
	if r.Status != StatusExpired {
		t.Fatalf("expected expired, got %s", r.Status)
	}
	// A signal just behind the frontier is also expired (already passed).
	r = ingest(t, e, Signal{ID: "late-2", PlayerID: "p", Seq: 5, Value: 0.9})
	if r.Status != StatusExpired {
		t.Fatalf("expected expired, got %s", r.Status)
	}
}

func TestBurstThrottle(t *testing.T) {
	cfg := testConfig()
	cfg.BurstLimit = 5
	e := newTestEngine(t, cfg)

	for i := 0; i < 12; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("b-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.9})
	}
	info := tickPlayer(t, e, "p")
	if info.Applied != 5 {
		t.Fatalf("expected 5 applied, got %d", info.Applied)
	}
	if info.Throttled != 7 {
		t.Fatalf("expected 7 throttled, got %d", info.Throttled)
	}
	// Throttled signals must not affect difficulty: only the first 5 (all
	// 0.9) count, so ewma is exactly 0.9.
	if math.Abs(info.Ewma-0.9) > 1e-9 {
		t.Fatalf("throttled signals leaked into ewma: %.3f", info.Ewma)
	}
}

func TestIdleRelax(t *testing.T) {
	cfg := testConfig()
	cfg.IdleTicks = 2
	cfg.IdleRelax = 0.1
	e := newTestEngine(t, cfg)

	// Push difficulty down with poor performance.
	for i := 0; i < 10; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("l-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.0})
	}
	info := tickPlayer(t, e, "p")
	low := info.NewDifficulty
	if low >= cfg.DefaultDifficulty {
		t.Fatalf("expected difficulty below default, got %.3f", low)
	}

	// Many idle ticks: difficulty should relax back toward default but
	// never overshoot it or leave the legal range.
	prev := low
	for i := 0; i < 30; i++ {
		info := tickPlayer(t, e, "p")
		if !info.Idle {
			t.Fatalf("tick %d: expected idle", i)
		}
		if info.NewDifficulty < cfg.MinDifficulty || info.NewDifficulty > cfg.MaxDifficulty {
			t.Fatalf("difficulty out of range: %.3f", info.NewDifficulty)
		}
		if info.NewDifficulty < prev-1e-9 {
			t.Fatalf("idle relax should not lower difficulty: %.3f -> %.3f", prev, info.NewDifficulty)
		}
		prev = info.NewDifficulty
	}
	if math.Abs(prev-cfg.DefaultDifficulty) > 1e-9 {
		t.Fatalf("difficulty should relax to default %.3f, got %.3f", cfg.DefaultDifficulty, prev)
	}
}

func TestConfigUpdateAtomic(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Invalid update is rejected and leaves the current config untouched.
	bad := cfg
	bad.Version = cfg.Version + 1
	bad.EwmaAlpha = 2.0
	if err := e.UpdateConfig(bad); err == nil {
		t.Fatal("expected invalid config to be rejected")
	}
	if e.Config().EwmaAlpha != cfg.EwmaAlpha {
		t.Fatal("invalid update changed the config")
	}

	// Valid update applies atomically with a new version.
	good := cfg
	good.Version = cfg.Version + 1
	good.AdjustGain = 0.5
	if err := e.UpdateConfig(good); err != nil {
		t.Fatal(err)
	}
	if e.Config().AdjustGain != 0.5 {
		t.Fatal("config update did not apply")
	}
	if e.Config().Version != good.Version {
		t.Fatal("config version did not advance")
	}

	// Version must strictly increase.
	if err := e.UpdateConfig(good); err != ErrConfigVersion {
		t.Fatalf("expected ErrConfigVersion, got %v", err)
	}
}

func TestConcurrentPlayers(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)
	ctx := context.Background()

	const players = 40
	const perPlayer = 50
	var wg sync.WaitGroup
	for p := 0; p < players; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			pid := fmt.Sprintf("p-%d", p)
			for i := 0; i < perPlayer; i++ {
				val := 0.1
				if p%2 == 0 {
					val = 0.9
				}
				_, err := e.Ingest(ctx, Signal{
					ID: fmt.Sprintf("%s-%d", pid, i), PlayerID: pid,
					Seq: int64(i + 1), Value: float64(val),
				})
				if err != nil {
					t.Error(err)
				}
			}
		}(p)
	}
	wg.Wait()
	if err := e.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	// Every player must exist with a self-consistent snapshot.
	ids := e.Players()
	if len(ids) != players {
		t.Fatalf("expected %d players, got %d", players, len(ids))
	}
	for _, id := range ids {
		snap, ok := e.GetDifficulty(id)
		if !ok {
			t.Fatalf("player %s missing", id)
		}
		if snap.Pending != perPlayer {
			t.Fatalf("player %s: expected %d pending, got %d", id, perPlayer, snap.Pending)
		}
		if snap.LastSeq != perPlayer {
			t.Fatalf("player %s: expected lastSeq %d, got %d", id, perPlayer, snap.LastSeq)
		}
		if snap.Difficulty < cfg.MinDifficulty || snap.Difficulty > cfg.MaxDifficulty {
			t.Fatalf("player %s: difficulty out of range %.3f", id, snap.Difficulty)
		}
	}

	// A global tick adjusts every player exactly once.
	results, err := e.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != players {
		t.Fatalf("expected %d tick results, got %d", players, len(results))
	}
	for _, info := range results {
		if !info.Ticked {
			t.Fatalf("expected ticked, got idle: %+v", info)
		}
		if info.Applied != perPlayer {
			t.Fatalf("expected %d applied, got %d", perPlayer, info.Applied)
		}
	}
}

func TestAutoTick(t *testing.T) {
	cfg := testConfig()
	cfg.TickSize = 5
	e := newTestEngine(t, cfg)

	// Ingest exactly TickSize signals: the last ingest should trigger an
	// automatic adjustment.
	var lastTick *TickInfo
	for i := 0; i < 5; i++ {
		r := ingest(t, e, Signal{ID: fmt.Sprintf("s-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.1})
		if i < 4 && r.Tick != nil {
			t.Fatalf("tick should not fire before TickSize (i=%d)", i)
		}
		if i == 4 {
			if r.Tick == nil {
				t.Fatal("expected auto-tick on the 5th signal")
			}
			lastTick = r.Tick
		}
	}
	if lastTick == nil || !lastTick.Ticked {
		t.Fatal("auto-tick did not run")
	}
	if lastTick.Applied != 5 {
		t.Fatalf("auto-tick should apply 5 signals, got %d", lastTick.Applied)
	}
	// After auto-tick, pending is cleared.
	snap, _ := e.GetDifficulty("p")
	if snap.Pending != 0 {
		t.Fatalf("auto-tick should clear pending, got %d", snap.Pending)
	}
}

func TestUnknownPlayer(t *testing.T) {
	e := newTestEngine(t, testConfig())
	if _, ok := e.GetDifficulty("ghost"); ok {
		t.Fatal("unknown player should not be found")
	}
	if len(e.Players()) != 0 {
		t.Fatal("expected no players")
	}
}

func TestPlayersIsolation(t *testing.T) {
	cfg := testConfig()
	e := newTestEngine(t, cfg)

	// Two players with opposite performance: their states must not mix.
	for i := 0; i < 10; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("a-%d", i), PlayerID: "a", Seq: int64(i + 1), Value: 0.0})
		ingest(t, e, Signal{ID: fmt.Sprintf("b-%d", i), PlayerID: "b", Seq: int64(i + 1), Value: 1.0})
	}
	results, err := e.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a := results["a"].NewDifficulty
	b := results["b"].NewDifficulty
	if a >= b {
		t.Fatalf("underperforming player should have lower difficulty: a=%.3f b=%.3f", a, b)
	}

	// A duplicate of player a's own signal must not be double-counted,
	// and must not affect player b.
	r := ingest(t, e, Signal{ID: "a-0", PlayerID: "a", Seq: 1, Value: 0.0})
	if r.Status != StatusDuplicate {
		t.Fatalf("expected duplicate, got %s", r.Status)
	}
	before, _ := e.GetDifficulty("b")
	if _, err := e.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := e.GetDifficulty("b")
	if after.Difficulty != before.Difficulty {
		t.Fatalf("player a's duplicate affected player b: %.3f -> %.3f", before.Difficulty, after.Difficulty)
	}
}
