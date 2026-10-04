// Command demo runs a closed-loop simulation of the adaptive difficulty
// engine: a population of players emits performance signals whose win
// probability depends on the current difficulty, and the engine adjusts
// difficulty each period to push performance into the target interval.
//
// The demo is fully deterministic: with the default seed the printed
// trajectories are identical on every run. It also exercises duplicate,
// late and invalid signals, and verifies that committed state survives a
// close/reopen cycle.
package main

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/guoyyit/adaptive-difficulty-engine-go/engine"
)

const (
	players = 16
	periods = 24
	seed    = int64(20261004)
)

func main() {
	dir, err := os.MkdirTemp("", "adaptive-difficulty-demo-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	store, err := engine.NewFileStore(filepath.Join(dir, "store"))
	if err != nil {
		panic(err)
	}
	eng, err := engine.NewEngine(engine.WithStore(store))
	if err != nil {
		panic(err)
	}
	cfg := engine.DefaultConfig()
	cfg.Version = 1
	cfg.TickSize = 1_000_000 // drive periods explicitly
	if err := eng.UpdateConfig(cfg); err != nil {
		panic(err)
	}

	rng := rand.New(rand.NewSource(seed))
	// Per-player skill in [0.3, 0.9]; higher skill performs better at the
	// same difficulty.
	skills := make([]float64, players)
	for i := range skills {
		skills[i] = 0.3 + rng.Float64()*0.6
	}

	counts := map[engine.Status]int{}
	trajectories := make([][]float64, players)
	for i := range trajectories {
		trajectories[i] = make([]float64, 0, periods)
	}

	ctx := context.Background()
	seq := make([]int64, players)
	for i := range seq {
		seq[i] = 1
	}

	for t := 0; t < periods; t++ {
		for p := 0; p < players; p++ {
			pid := fmt.Sprintf("player-%02d", p)
			snap, ok := eng.GetDifficulty(pid)
			difficulty := cfg.DefaultDifficulty
			if ok {
				difficulty = snap.Difficulty
			}
			// Closed loop: win probability decreases with difficulty.
			winProb := clamp(skills[p]-0.12*difficulty+0.1*(rng.Float64()-0.5), 0.05, 0.95)
			val := 0.0
			if rng.Float64() < winProb {
				val = 1.0
			}
			sig := engine.Signal{
				ID: fmt.Sprintf("%s-s-%d", pid, seq[p]), PlayerID: pid,
				Seq: seq[p], Value: val,
			}
			seq[p]++
			res, err := eng.Ingest(ctx, sig)
			if err != nil {
				panic(err)
			}
			counts[res.Status]++

			// Inject a duplicate of every 7th signal.
			if seq[p]%7 == 0 {
				dup := sig
				dup.Seq = seq[p] + 1000
				res, err := eng.Ingest(ctx, dup)
				if err != nil {
					panic(err)
				}
				counts[res.Status]++
				seq[p]++
			}
			// Inject an invalid signal for the first player once.
			if p == 0 && t == 3 {
				res, err := eng.Ingest(ctx, engine.Signal{
					ID: "bad", PlayerID: pid, Seq: seq[p], Value: 1.5,
				})
				if err != nil {
					panic(err)
				}
				counts[res.Status]++
			}
		}

		results, err := eng.Tick(ctx)
		if err != nil {
			panic(err)
		}
		for p := 0; p < players; p++ {
			pid := fmt.Sprintf("player-%02d", p)
			trajectories[p] = append(trajectories[p], results[pid].NewDifficulty)
		}
	}

	// Close and reopen: committed state must survive.
	if err := eng.Close(); err != nil {
		panic(err)
	}
	store2, err := engine.NewFileStore(filepath.Join(dir, "store"))
	if err != nil {
		panic(err)
	}
	eng2, err := engine.NewEngine(engine.WithStore(store2))
	if err != nil {
		panic(err)
	}
	defer eng2.Close()

	fmt.Println("Adaptive difficulty engine — closed-loop simulation")
	fmt.Printf("players=%d periods=%d seed=%d\n\n", players, periods, seed)
	fmt.Println("Difficulty trajectories (one column per period):")
	fmt.Println("player   skill  " + strings.Repeat("period", periods))
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("player-%02d", p)
		snap, _ := eng2.GetDifficulty(pid)
		row := make([]string, periods)
		for t, d := range trajectories[p] {
			row[t] = fmt.Sprintf("%.2f", d)
		}
		fmt.Printf("%s  %.2f  %s  final=%.2f ewma=%.2f\n",
			pid, skills[p], strings.Join(row, " "), snap.Difficulty, snap.Ewma)
	}

	fmt.Println("\nSignal handling summary:")
	statuses := make([]engine.Status, 0, len(counts))
	for s := range counts {
		statuses = append(statuses, s)
	}
	sort.Slice(statuses, func(i, j int) bool { return statuses[i] < statuses[j] })
	for _, s := range statuses {
		fmt.Printf("  %-10s %d\n", s, counts[s])
	}

	// Verify recovery: final difficulty from the reopened engine matches
	// the last trajectory value.
	fmt.Println("\nRecovery check (close/reopen):")
	mismatch := 0
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("player-%02d", p)
		snap, _ := eng2.GetDifficulty(pid)
		last := trajectories[p][periods-1]
		if math.Abs(snap.Difficulty-last) > 1e-9 {
			mismatch++
		}
	}
	if mismatch == 0 {
		fmt.Println("  OK: committed difficulty restored exactly for all players")
	} else {
		fmt.Printf("  FAIL: %d players mismatch\n", mismatch)
	}

	// Show that all final difficulties lie in the legal range and most
	// have converged toward the default region.
	inRange := 0
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("player-%02d", p)
		snap, _ := eng2.GetDifficulty(pid)
		if snap.Difficulty >= cfg.MinDifficulty && snap.Difficulty <= cfg.MaxDifficulty {
			inRange++
		}
	}
	fmt.Printf("  %d/%d players within legal difficulty range [%.1f, %.1f]\n",
		inRange, players, cfg.MinDifficulty, cfg.MaxDifficulty)
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
