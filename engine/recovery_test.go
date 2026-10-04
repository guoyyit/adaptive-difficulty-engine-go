package engine

import (
	"context"
	"fmt"
	"testing"
)

func newFileEngine(t *testing.T) (*Engine, *FileStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	e, err := NewEngine(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	return e, store
}

// TestRecoveryCommittedState: a tick commits a snapshot; after close and
// reopen the committed difficulty and ewma are restored exactly.
func TestRecoveryCommittedState(t *testing.T) {
	e, store := newFileEngine(t)
	ctx := context.Background()
	cfg := testConfig()
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("s-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.1})
	}
	before, err := e.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := NewEngine(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	snap, ok := e2.GetDifficulty("p")
	if !ok {
		t.Fatal("player not restored")
	}
	if snap.Difficulty != before["p"].NewDifficulty {
		t.Fatalf("committed difficulty mismatch: before=%.6f after=%.6f", before["p"].NewDifficulty, snap.Difficulty)
	}
	if snap.Ewma != before["p"].Ewma {
		t.Fatalf("committed ewma mismatch: before=%.6f after=%.6f", before["p"].Ewma, snap.Ewma)
	}
	if snap.Pending != 0 {
		t.Fatalf("committed state should have no pending, got %d", snap.Pending)
	}
}

// TestRecoveryInFlightSignals: signals ingested but not yet ticked are
// held in the WAL; after reopen they are re-applied as pending (in-flight)
// and the committed difficulty is unchanged.
func TestRecoveryInFlightSignals(t *testing.T) {
	e, store := newFileEngine(t)
	ctx := context.Background()
	cfg := testConfig()
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// Commit one tick first.
	for i := 0; i < 10; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("s-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.1})
	}
	if _, err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	committed, _ := e.GetDifficulty("p")

	// Ingest more signals without ticking: these are in-flight.
	for i := 10; i < 15; i++ {
		ingest(t, e, Signal{ID: fmt.Sprintf("s-%d", i), PlayerID: "p", Seq: int64(i + 1), Value: 0.9})
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := NewEngine(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()

	snap, ok := e2.GetDifficulty("p")
	if !ok {
		t.Fatal("player not restored")
	}
	if snap.Difficulty != committed.Difficulty {
		t.Fatalf("committed difficulty changed across recovery: %.6f -> %.6f", committed.Difficulty, snap.Difficulty)
	}
	if snap.Pending != 5 {
		t.Fatalf("in-flight signals should be pending after recovery, got %d", snap.Pending)
	}

	// The in-flight signals are not double-counted: a duplicate is
	// rejected, and a subsequent tick applies them exactly once.
	r := ingest(t, e2, Signal{ID: "s-10", PlayerID: "p", Seq: 11, Value: 0.9})
	if r.Status != StatusDuplicate {
		t.Fatalf("recovered signal should be duplicate, got %s", r.Status)
	}
	after, err := e2.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after["p"].Applied != 5 {
		t.Fatalf("in-flight signals should apply once, got applied=%d", after["p"].Applied)
	}
}

// TestRecoveryConfigPersisted: a runtime config update survives restart.
func TestRecoveryConfigPersisted(t *testing.T) {
	e, store := newFileEngine(t)
	cfg := testConfig()
	cfg.AdjustGain = 0.42
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := NewEngine(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if e2.Config().AdjustGain != 0.42 {
		t.Fatalf("config not persisted: AdjustGain=%.3f", e2.Config().AdjustGain)
	}
	if e2.Config().Version != cfg.Version {
		t.Fatalf("config version not persisted: %d", e2.Config().Version)
	}
}

// TestRecoveryMultiplePlayers: committed state for many players is
// restored independently, with no cross-contamination.
func TestRecoveryMultiplePlayers(t *testing.T) {
	e, store := newFileEngine(t)
	ctx := context.Background()
	cfg := testConfig()
	if err := e.UpdateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	const players = 20
	for p := 0; p < players; p++ {
		pid := fmt.Sprintf("p-%02d", p)
		val := 0.1
		if p%2 == 0 {
			val = 0.9
		}
		for i := 0; i < 10; i++ {
			ingest(t, e, Signal{ID: fmt.Sprintf("%s-s-%d", pid, i), PlayerID: pid, Seq: int64(i + 1), Value: float64(val)})
		}
	}
	if _, err := e.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	before := make(map[string]float64)
	for _, pid := range e.Players() {
		snap, _ := e.GetDifficulty(pid)
		before[pid] = snap.Difficulty
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e2, err := NewEngine(WithStore(store))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if len(e2.Players()) != players {
		t.Fatalf("expected %d players, got %d", players, len(e2.Players()))
	}
	for _, pid := range e2.Players() {
		snap, ok := e2.GetDifficulty(pid)
		if !ok {
			t.Fatalf("player %s missing", pid)
		}
		if snap.Difficulty != before[pid] {
			t.Fatalf("player %s: difficulty mismatch before=%.6f after=%.6f", pid, before[pid], snap.Difficulty)
		}
		if snap.Pending != 0 {
			t.Fatalf("player %s: expected no pending, got %d", pid, snap.Pending)
		}
	}
}
