# Adaptive Difficulty Engine

A deterministic, concurrency-safe engine that maintains a per-player
difficulty level and adjusts it in fixed periods based on recent
performance. Difficulty changes are smooth, bounded, and converge to a
target performance interval; a single anomalous signal cannot cause a
jump, and the level never leaves the configured legal range.

The engine is written in pure Go (standard library only), persists
committed state and in-flight signals separately, and reproduces
bit-identical difficulty trajectories for the same signal sequence
regardless of ingestion concurrency.

## Adjustment model

Each player owns two pieces of state:

- `difficulty` — the current challenge level, always in
  `[MinDifficulty, MaxDifficulty]`.
- `ewma` — an exponentially weighted moving average of recent performance
  signals (each in `[0,1]`, higher is better).

A period (`Tick`) works as follows for a player with pending signals:

1. Signals are ordered by `Seq` (per-player monotonic sequence) and
   deduplicated by signal `ID`. Out-of-order signals are held in a
   reorder buffer and applied contiguously.
2. If more than `BurstLimit` signals are pending, the excess are dropped
   as throttled (burst degradation).
3. The period average of the first `BurstLimit` signals updates the EWMA:
   `ewma += EwmaAlpha * (avg - ewma)`.
4. A deadband controller adjusts difficulty toward the target interval
   `[TargetPerfLo, TargetPerfHi]`:
   - `ewma < TargetPerfLo` (underperforming) -> difficulty decreases.
   - `ewma > TargetPerfHi` (overperforming) -> difficulty increases.
   - inside the interval -> no change.
5. The per-period step is clamped to `[-MaxStep, MaxStep]` and the final
   difficulty is clamped to `[MinDifficulty, MaxDifficulty]`.

With no pending signals the player is idle; after `IdleTicks` idle
periods the difficulty relaxes slowly toward `DefaultDifficulty` (by at
most `IdleRelax` per period), so a returning player is not stuck at an
extreme.

The EWMA + deadband + clamped step gives: smooth changes (EWMA smoothing),
no jumps (step clamp), no oscillation (deadband), and no divergence
(range clamp).

## Signal handling

Every `Ingest` returns a `Result` whose `Status` is one of:

| Status       | Meaning                                                        |
| ------------ | -------------------------------------------------------------- |
| `applied`    | signal was accepted and applied to the player's pending set   |
| `duplicate`  | signal `ID` was already seen (applied, buffered, or throttled)|
| `expired`    | signal arrived too late (seq passed or outside reorder window)|
| `invalid`    | signal failed shape validation (bad id, seq, or value)        |
| `throttled`  | signal was dropped by the burst policy at the tick boundary   |

- **Out-of-order**: signals are buffered and applied in `Seq` order.
- **Duplicate**: a signal whose `ID` was already processed is rejected
  without affecting state.
- **Late**: a signal with `Seq <= lastSeq` (already applied or in a
  passed gap) or `Seq > lastSeq + LateWindow` (beyond the reorder
  window) is expired and does not affect difficulty.
- **Invalid**: empty id/player, non-positive seq, or value outside
  `[0,1]` (including NaN) is rejected before any state change.
- **Burst**: at a tick, at most `BurstLimit` pending signals are
  incorporated; the rest are throttled (and still deduplicated).

## Configuration

All parameters are in `engine.Config`:

| Field              | Default | Description                                            |
| ------------------ | ------- | ------------------------------------------------------ |
| `MinDifficulty`    | 0.1     | legal lower bound                                      |
| `MaxDifficulty`    | 5.0     | legal upper bound                                      |
| `DefaultDifficulty`| 1.0     | starting difficulty and idle-relax target              |
| `TargetPerfLo`     | 0.4     | lower bound of the target performance interval         |
| `TargetPerfHi`     | 0.6     | upper bound of the target performance interval         |
| `EwmaAlpha`        | 0.2     | EWMA smoothing factor per period                       |
| `AdjustGain`       | 0.15    | adjustment gain toward the target interval             |
| `MaxStep`          | 0.3     | maximum difficulty change per period                   |
| `TickSize`         | 8       | auto-tick after this many pending signals              |
| `LateWindow`       | 64      | reorder window in sequence numbers                     |
| `BurstLimit`       | 20      | max signals incorporated per period                   |
| `IdleTicks`        | 12      | idle periods before relaxation starts                  |
| `IdleRelax`        | 0.02    | max relaxation step per idle period                    |

`UpdateConfig` validates the new configuration (the new `Version` must be
strictly greater than the current one), persists it, and then atomically
swaps it in. Observers see either the old or the new configuration, never
a partial state, and a failed update leaves the running configuration
untouched.

## Concurrency

- Each player is owned by a dedicated goroutine. Ingestion for one player
  never blocks on another player's adjustment, and a player's adjustment
  never blocks signal ingestion.
- Reads (`GetDifficulty`) go through an `atomic.Pointer` to an immutable
  snapshot published after every state change. Readers always observe a
  complete, self-consistent state and never block.
- `Tick` fans out one adjustment message per player and waits for all;
  each player adjusts at most once per period.
- `Flush` waits until every ingested signal has been processed.

## Determinism

Signals are applied strictly in per-player `Seq` order via the reorder
buffer, and adjustments are driven by period boundaries (not wall
clocks). With a fixed random seed and the same signal sequence, the
per-player difficulty trajectory is identical regardless of ingestion
concurrency. The test `TestDeterminismAcrossConcurrency` verifies this
across 1, 2, 4, 8, and 16 concurrent ingest workers.

Note: the reorder window (`LateWindow`) bounds memory. For bit-identical
reproduction of a sequence where no signal should ever be expired, set
`LateWindow` to at least the maximum sequence number in the workload.

## Persistence and recovery

The `FileStore` persists two kinds of state:

- **Snapshot** (`players/<id>.json`): the committed difficulty, EWMA,
  last applied seq, seen set, and config version. Written atomically
  (temp file, fsync, rename) after each tick.
- **WAL** (`wal/<id>.log`): append-only record of signals ingested but
  not yet committed by a tick.

On recovery:

1. Snapshots restore committed state exactly.
2. WAL entries are replayed through the same `apply` path used for live
   ingest. Signals already in the snapshot's seen set are skipped
   (idempotent); the rest are re-applied as pending (in-flight).

This distinguishes committed difficulty (from snapshots) from
not-yet-committed adjustments (from the WAL). A crash between snapshot
write and WAL truncate is safe: replay skips signals already in the
snapshot's seen set.

The `MemoryStore` is used by default and in tests.

## Verification

```sh
go test ./...          # run all tests
go test -race ./...    # run with the race detector
go run ./cmd/demo      # closed-loop simulation + recovery check
```

Tests cover:

- smooth adjustment, deadband, clamping, and no-jump from outliers
- invalid, duplicate, out-of-order, late, and burst signals
- idle relaxation
- atomic configuration updates
- concurrent ingestion and snapshot consistency
- determinism across concurrency levels
- recovery of committed and in-flight state, config persistence, and
  multi-player isolation

## API overview

```go
eng, _ := engine.NewEngine()
eng.UpdateConfig(cfg)                 // atomic runtime config update
res, _ := eng.Ingest(ctx, signal)     // validate, dedupe, order, apply
results, _ := eng.Tick(ctx)           // one adjustment period per player
eng.Flush(ctx)                        // wait for all ingested signals
snap, ok := eng.GetDifficulty(pid)    // immutable, self-consistent view
eng.Close()                           // stop workers and store
```
