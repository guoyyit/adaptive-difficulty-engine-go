package engine

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
)

// scenario is a reproducible signal workload. Batches are delivered in
// order; within a batch, signals are shuffled with a seeded RNG so the
// same scenario produces the same arrival order on every run.
type scenario struct {
	name    string
	players int
	batches [][]Signal
}

func buildScenario(seed int64, players, perPlayer int) scenario {
	rng := rand.New(rand.NewSource(seed))
	cfg := testConfig()
	_ = cfg
	batches := make([][]Signal, 0, 4)

	// Batch 1: seqs 1..perPlayer, shuffled.
	var b1 []Signal
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("p-%02d", p)
		for i := 1; i <= perPlayer; i++ {
			b1 = append(b1, Signal{
				ID: fmt.Sprintf("%s-s-%d", pid, i), PlayerID: pid,
				Seq: int64(i), Value: rng.Float64(),
			})
		}
	}
	rng.Shuffle(len(b1), func(i, j int) { b1[i], b1[j] = b1[j], b1[i] })
	batches = append(batches, b1)

	// Batch 2: late signals (seq 1, fresh id) for every player.
	var b2 []Signal
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("p-%02d", p)
		b2 = append(b2, Signal{
			ID: fmt.Sprintf("%s-late", pid), PlayerID: pid,
			Seq: 1, Value: rng.Float64(),
		})
	}
	rng.Shuffle(len(b2), func(i, j int) { b2[i], b2[j] = b2[j], b2[i] })
	batches = append(batches, b2)

	// Batch 3: duplicates of batch-1 signals (same id, different seq).
	var b3 []Signal
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("p-%02d", p)
		orig := b1[p*perPlayer+1] // deterministic pick per player
		b3 = append(b3, Signal{
			ID: orig.ID, PlayerID: pid,
			Seq: int64(perPlayer + 50), Value: rng.Float64(),
		})
	}
	rng.Shuffle(len(b3), func(i, j int) { b3[i], b3[j] = b3[j], b3[i] })
	batches = append(batches, b3)

	// Batch 4: seqs perPlayer+1 .. perPlayer+10, shuffled.
	var b4 []Signal
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("p-%02d", p)
		for i := perPlayer + 1; i <= perPlayer+10; i++ {
			b4 = append(b4, Signal{
				ID: fmt.Sprintf("%s-s-%d", pid, i), PlayerID: pid,
				Seq: int64(i), Value: rng.Float64(),
			})
		}
	}
	rng.Shuffle(len(b4), func(i, j int) { b4[i], b4[j] = b4[j], b4[i] })
	batches = append(batches, b4)

	return scenario{name: fmt.Sprintf("seed-%d", seed), players: players, batches: batches}
}

// runScenario ingests every batch with the given concurrency, then ticks
// the engine ticks times, recording each player's difficulty after every
// tick.
func runScenario(t *testing.T, sc scenario, workers, ticks int) map[string][]float64 {
	t.Helper()
	// Use a reorder window large enough that no in-flight signal is ever
	// expired: determinism requires every signal to apply in seq order
	// regardless of arrival interleaving.
	cfg := testConfig()
	cfg.LateWindow = 1_000_000
	e, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for batchIdx, batch := range sc.batches {
		if workers <= 1 {
			for _, sig := range batch {
				if _, err := e.Ingest(ctx, sig); err != nil {
					t.Fatalf("batch %d: %v", batchIdx, err)
				}
			}
		} else {
			var wg sync.WaitGroup
			chunk := (len(batch) + workers - 1) / workers
			for w := 0; w < workers; w++ {
				start := w * chunk
				if start >= len(batch) {
					break
				}
				end := start + chunk
				if end > len(batch) {
					end = len(batch)
				}
				wg.Add(1)
				go func(sigs []Signal) {
					defer wg.Done()
					for _, sig := range sigs {
						if _, err := e.Ingest(ctx, sig); err != nil {
							t.Errorf("ingest: %v", err)
						}
					}
				}(batch[start:end])
			}
			wg.Wait()
		}
		if err := e.Flush(ctx); err != nil {
			t.Fatalf("batch %d flush: %v", batchIdx, err)
		}
	}

	traj := make(map[string][]float64, sc.players)
	for i := 0; i < ticks; i++ {
		results, err := e.Tick(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for pid, info := range results {
			traj[pid] = append(traj[pid], info.NewDifficulty)
		}
	}
	return traj
}

func trajectoriesEqual(a, b map[string][]float64) bool {
	if len(a) != len(b) {
		return false
	}
	for pid, ta := range a {
		tb, ok := b[pid]
		if !ok || len(ta) != len(tb) {
			return false
		}
		for i := range ta {
			if ta[i] != tb[i] {
				return false
			}
		}
	}
	return true
}

// TestDeterminismAcrossConcurrency is the key reproducibility property:
// with a fixed seed and the same signal sequence, the per-player
// difficulty trajectory is identical regardless of ingestion concurrency.
func TestDeterminismAcrossConcurrency(t *testing.T) {
	sc := buildScenario(20261004, 12, 40)
	baseline := runScenario(t, sc, 1, 8)

	for _, workers := range []int{2, 4, 8, 16} {
		got := runScenario(t, sc, workers, 8)
		if !trajectoriesEqual(baseline, got) {
			for pid := range baseline {
				t.Errorf("workers=%d player=%s trajectory mismatch\n baseline=%v\n got=     %v",
					workers, pid, baseline[pid], got[pid])
			}
		}
	}
}

// TestDeterminismReplay runs the same scenario twice serially and
// confirms bit-identical trajectories (guards against accidental
// nondeterminism in the reorder/gap logic).
func TestDeterminismReplay(t *testing.T) {
	sc := buildScenario(99, 8, 30)
	a := runScenario(t, sc, 1, 6)
	b := runScenario(t, sc, 1, 6)
	if !trajectoriesEqual(a, b) {
		t.Fatal("replayed scenario produced different trajectories")
	}
}
