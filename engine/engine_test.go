package engine

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"testing"
)

func testConfig() Config {
	cfg := DefaultConfig()
	cfg.SignalsPerCycle = 4
	cfg.ReorderWindow = 8
	cfg.BurstLimit = 16
	return cfg
}

// 难度应收敛到目标区间附近，且单次不跳变、不发散。
func TestConvergenceSmoothAndBounded(t *testing.T) {
	eng, err := New(testConfig(), "")
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(1))
	var prev float64
	var lastAdj *Adjustment
	for seq := uint64(1); seq <= 2000; seq++ {
		snap, _ := eng.Snapshot("p1")
		prev = snap.Difficulty
		// 玩家水平 7.0：得分随难度升高而降低。
		score := 0.5 + (7.0-prev)*0.2 + (rng.Float64()-0.5)*0.1
		score = math.Min(1, math.Max(0, score))
		res := eng.Ingest(Signal{PlayerID: "p1", Seq: seq, Score: score, Timestamp: int64(seq) * 100})
		if res.Adjustment != nil {
			a := res.Adjustment
			if d := math.Abs(a.New - a.Old); d > testConfig().MaxStep+1e-9 {
				t.Fatalf("cycle %d: jump %v exceeds MaxStep", a.Cycle, d)
			}
			if a.New < testConfig().MinDifficulty || a.New > testConfig().MaxDifficulty {
				t.Fatalf("cycle %d: difficulty %v out of range", a.Cycle, a.New)
			}
			lastAdj = a
		}
	}
	if lastAdj == nil {
		t.Fatal("no adjustment produced")
	}
	// 收敛：最终难度应使期望得分落入目标区间（0.5+(7-d)*0.2 ∈ [0.45,0.55] ⇒ d ∈ [6.75,7.25]）。
	if lastAdj.New < 6.5 || lastAdj.New > 7.5 {
		t.Fatalf("difficulty did not converge near 7.0, got %v", lastAdj.New)
	}
	// 不发散：最后 10 个周期的波动应很小。
	snap, _ := eng.Snapshot("p1")
	if math.Abs(snap.Difficulty-lastAdj.New) > 1.0 {
		t.Fatalf("difficulty diverging: snapshot=%v last=%v", snap.Difficulty, lastAdj.New)
	}
}

// 乱序信号先缓冲，前序到达后按序生效，最终状态与顺序到达一致。
func TestOutOfOrderReordering(t *testing.T) {
	mk := func() *Engine {
		e, err := New(testConfig(), "")
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	ordered, shuffled := mk(), mk()
	sigs := []Signal{
		{PlayerID: "p", Seq: 1, Score: 0.9, Timestamp: 100},
		{PlayerID: "p", Seq: 2, Score: 0.8, Timestamp: 200},
		{PlayerID: "p", Seq: 3, Score: 0.7, Timestamp: 300},
		{PlayerID: "p", Seq: 4, Score: 0.6, Timestamp: 400},
	}
	for _, s := range sigs {
		ordered.Ingest(s)
	}
	// 乱序：3,1,4,2
	res := shuffled.Ingest(sigs[2])
	if res.Status != StatusBuffered {
		t.Fatalf("seq3 first: want Buffered, got %v", res.Status)
	}
	shuffled.Ingest(sigs[0])
	res = shuffled.Ingest(sigs[3])
	if res.Status != StatusBuffered {
		t.Fatalf("seq4 before seq2: want Buffered, got %v", res.Status)
	}
	res = shuffled.Ingest(sigs[1])
	if res.Status != StatusAccepted {
		t.Fatalf("seq2: want Accepted, got %v", res.Status)
	}
	s1, _ := ordered.Snapshot("p")
	s2, _ := shuffled.Snapshot("p")
	if s1 != s2 {
		t.Fatalf("reordered state mismatch: ordered=%+v shuffled=%+v", s1, s2)
	}
}

// 重复信号不得被重复累计。
func TestDuplicateNotCounted(t *testing.T) {
	eng, _ := New(testConfig(), "")
	for seq := uint64(1); seq <= 4; seq++ {
		eng.Ingest(Signal{PlayerID: "p", Seq: seq, Score: 1.0, Timestamp: int64(seq)})
	}
	snapBefore, _ := eng.Snapshot("p")
	res := eng.Ingest(Signal{PlayerID: "p", Seq: 4, Score: 1.0, Timestamp: 4})
	if res.Status != StatusDuplicate {
		t.Fatalf("want Duplicate, got %v", res.Status)
	}
	snapAfter, _ := eng.Snapshot("p")
	if snapBefore != snapAfter {
		t.Fatalf("duplicate changed state: %+v -> %+v", snapBefore, snapAfter)
	}
	// 缓冲中的重复也算重复。
	res = eng.Ingest(Signal{PlayerID: "p", Seq: 7, Score: 0.5, Timestamp: 7})
	if res.Status != StatusBuffered {
		t.Fatalf("want Buffered, got %v", res.Status)
	}
	res = eng.Ingest(Signal{PlayerID: "p", Seq: 7, Score: 0.5, Timestamp: 7})
	if res.Status != StatusDuplicate {
		t.Fatalf("buffered dup: want Duplicate, got %v", res.Status)
	}
}

// 迟到/过期信号不再影响难度；序号空洞按策略跳过。
func TestExpiredAndGapSkip(t *testing.T) {
	eng, _ := New(testConfig(), "")
	for seq := uint64(1); seq <= 20; seq++ {
		eng.Ingest(Signal{PlayerID: "p", Seq: seq, Score: 0.5, Timestamp: int64(seq)})
	}
	snapBefore, _ := eng.Snapshot("p")
	res := eng.Ingest(Signal{PlayerID: "p", Seq: 1, Score: 1.0, Timestamp: 1})
	if res.Status != StatusExpired {
		t.Fatalf("want Expired, got %v", res.Status)
	}
	snapAfter, _ := eng.Snapshot("p")
	if snapBefore != snapAfter {
		t.Fatal("expired signal changed state")
	}
	// 空洞跳过：序号远超水位线+窗口时，缺失序号视为丢失。
	res = eng.Ingest(Signal{PlayerID: "p", Seq: 100, Score: 0.5, Timestamp: 100})
	if res.Status != StatusAccepted {
		t.Fatalf("gap skip: want Accepted, got %v", res.Status)
	}
	snap, _ := eng.Snapshot("p")
	if snap.AppliedSeq != 100 {
		t.Fatalf("watermark should advance to 100, got %d", snap.AppliedSeq)
	}
}

// 越界信号按截断策略降级，结果可区分，难度不越界。
func TestOutOfRangeClamped(t *testing.T) {
	eng, _ := New(testConfig(), "")
	res := eng.Ingest(Signal{PlayerID: "p", Seq: 1, Score: 1.7, Timestamp: 1})
	if res.Status != StatusOutOfRange {
		t.Fatalf("want OutOfRange, got %v", res.Status)
	}
	res = eng.Ingest(Signal{PlayerID: "p", Seq: 2, Score: math.NaN(), Timestamp: 2})
	if res.Status != StatusOutOfRange {
		t.Fatalf("NaN: want OutOfRange, got %v", res.Status)
	}
	snap, _ := eng.Snapshot("p")
	if snap.Difficulty < testConfig().MinDifficulty || snap.Difficulty > testConfig().MaxDifficulty {
		t.Fatalf("difficulty out of legal range: %v", snap.Difficulty)
	}
}

// 突发信号超过单周期上限时被降级丢弃。
func TestBurstDegraded(t *testing.T) {
	cfg := testConfig()
	cfg.SignalsPerCycle = 100 // 周期不结束，burst 计数不清零
	cfg.BurstLimit = 4
	eng, _ := New(cfg, "")
	dropped := 0
	for seq := uint64(1); seq <= 10; seq++ {
		res := eng.Ingest(Signal{PlayerID: "p", Seq: seq, Score: 1.0, Timestamp: int64(seq)})
		if res.Status == StatusBurstDropped {
			dropped++
		}
	}
	if dropped != 6 {
		t.Fatalf("want 6 burst-dropped, got %d", dropped)
	}
}

// 长期无信号后 EWMA 向中性衰减（白盒验证）。
func TestIdleDecay(t *testing.T) {
	cfg := testConfig()
	cfg.IdleGapMs = 100
	p := newPlayerState(cfg)
	p.ingest(Signal{PlayerID: "p", Seq: 1, Score: 1.0, Timestamp: 0}, cfg)
	if p.ewma != 1.0 {
		t.Fatalf("ewma should be 1.0, got %v", p.ewma)
	}
	p.ingest(Signal{PlayerID: "p", Seq: 2, Score: 0.0, Timestamp: 1000}, cfg)
	// 衰减后 ewma = 0.5+(1.0-0.5)/2 = 0.75，再与 0.0 做 EWMA。
	want := cfg.SmoothingAlpha*0.0 + (1-cfg.SmoothingAlpha)*0.75
	if math.Abs(p.ewma-want) > 1e-12 {
		t.Fatalf("idle decay wrong: got %v want %v", p.ewma, want)
	}
}

// 同一玩家同一调节周期只产生一个结果。
func TestOneAdjustmentPerCycle(t *testing.T) {
	eng, _ := New(testConfig(), "")
	cycles := map[uint64]int{}
	for seq := uint64(1); seq <= 40; seq++ {
		res := eng.Ingest(Signal{PlayerID: "p", Seq: seq, Score: 0.8, Timestamp: int64(seq)})
		if res.Adjustment != nil {
			cycles[res.Adjustment.Cycle]++
		}
	}
	if len(cycles) != 10 {
		t.Fatalf("want 10 cycles, got %d", len(cycles))
	}
	for c, n := range cycles {
		if n != 1 {
			t.Fatalf("cycle %d produced %d results", c, n)
		}
	}
}

// 并发摄入：不同并发度下逐次难度结果完全一致。
func TestDeterministicAcrossConcurrency(t *testing.T) {
	const players = 16
	const perPlayer = 200

	gen := func() [][]Signal {
		rng := rand.New(rand.NewSource(99))
		out := make([][]Signal, players)
		for i := range out {
			id := fmt.Sprintf("player-%d", i)
			for seq := uint64(1); seq <= perPlayer; seq++ {
				out[i] = append(out[i], Signal{
					PlayerID:  id,
					Seq:       seq,
					Score:     rng.Float64(),
					Timestamp: int64(seq) * 100,
				})
			}
		}
		return out
	}

	run := func(concurrency bool) map[string][]float64 {
		eng, _ := New(testConfig(), "")
		sigs := gen()
		var mu sync.Mutex
		results := map[string][]float64{}
		var wg sync.WaitGroup
		for i := range sigs {
			list := sigs[i]
			wg.Add(1)
			go func() {
				defer wg.Done()
				for _, s := range list {
					res := eng.Ingest(s)
					if res.Adjustment != nil {
						mu.Lock()
						results[s.PlayerID] = append(results[s.PlayerID], res.Adjustment.New)
						mu.Unlock()
					}
				}
			}()
			if !concurrency {
				wg.Wait()
			}
		}
		wg.Wait()
		return results
	}

	seq := run(false)
	par := run(true)
	if len(seq) != players || len(par) != players {
		t.Fatalf("missing players: seq=%d par=%d", len(seq), len(par))
	}
	for id := range seq {
		if len(seq[id]) != len(par[id]) {
			t.Fatalf("%s: adjustment count differs %d vs %d", id, len(seq[id]), len(par[id]))
		}
		for i := range seq[id] {
			if seq[id][i] != par[id][i] {
				t.Fatalf("%s adj %d: sequential=%v parallel=%v", id, i, seq[id][i], par[id][i])
			}
		}
	}
}

// 大量玩家并发推进时，读取到的快照必须是自洽完整状态。
func TestConcurrentReadWriteConsistent(t *testing.T) {
	eng, _ := New(testConfig(), "")
	var wg sync.WaitGroup
	for w := 0; w < 32; w++ {
		id := fmt.Sprintf("p-%d", w)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := uint64(1); seq <= 100; seq++ {
				eng.Ingest(Signal{PlayerID: id, Seq: seq, Score: 0.6, Timestamp: int64(seq)})
			}
		}()
	}
	for r := 0; r < 8; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				snap, ok := eng.Snapshot(fmt.Sprintf("p-%d", i%32))
				if ok {
					if snap.Difficulty < testConfig().MinDifficulty || snap.Difficulty > testConfig().MaxDifficulty {
						t.Errorf("snapshot out of range: %v", snap.Difficulty)
					}
				}
			}
		}()
	}
	wg.Wait()
}

// 配置更新：非法配置被拒绝且旧配置完整保留，合法更新整体生效。
func TestConfigUpdateAtomic(t *testing.T) {
	eng, _ := New(testConfig(), "")
	old := eng.Config()
	bad := old
	bad.TargetLow, bad.TargetHigh = 0.9, 0.1
	if err := eng.UpdateConfig(bad); err == nil {
		t.Fatal("invalid config accepted")
	}
	if eng.Config() != old {
		t.Fatal("failed update changed config")
	}
	nu := old
	nu.MaxStep = 0.25
	nu.TargetLow, nu.TargetHigh = 0.4, 0.6
	if err := eng.UpdateConfig(nu); err != nil {
		t.Fatal(err)
	}
	if eng.Config() != nu {
		t.Fatal("update not fully applied")
	}
}

// 重启恢复：已提交难度与未完成的调节进度都被还原且可区分。
func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig()
	eng, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	// 4 条信号完成 1 个周期，再发 2 条形成未完成周期。
	for seq := uint64(1); seq <= 6; seq++ {
		eng.Ingest(Signal{PlayerID: "p", Seq: seq, Score: 0.9, Timestamp: int64(seq)})
	}
	before, _ := eng.Snapshot("p")
	if before.Cycle != 1 {
		t.Fatalf("want 1 committed cycle, got %d", before.Cycle)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}

	eng2, err := New(cfg, dir)
	if err != nil {
		t.Fatal(err)
	}
	after, ok := eng2.Snapshot("p")
	if !ok {
		t.Fatal("player state not recovered")
	}
	if !after.Committed {
		t.Fatal("recovered state should be marked committed")
	}
	if after.Difficulty != before.Difficulty || after.Cycle != before.Cycle || after.AppliedSeq != before.AppliedSeq {
		t.Fatalf("recovered state mismatch: before=%+v after=%+v", before, after)
	}
	// 未完成的调节进度（2/4）继续推进：再发 2 条应完成第 2 周期。
	res := eng2.Ingest(Signal{PlayerID: "p", Seq: 7, Score: 0.9, Timestamp: 7})
	if res.Adjustment != nil {
		t.Fatal("cycle should not complete after 1 more signal")
	}
	res = eng2.Ingest(Signal{PlayerID: "p", Seq: 8, Score: 0.9, Timestamp: 8})
	if res.Adjustment == nil || res.Adjustment.Cycle != 2 {
		t.Fatalf("pending cycle not resumed correctly: %+v", res.Adjustment)
	}
}

// 不同玩家状态互不污染。
func TestPlayerIsolation(t *testing.T) {
	eng, _ := New(testConfig(), "")
	for seq := uint64(1); seq <= 8; seq++ {
		eng.Ingest(Signal{PlayerID: "a", Seq: seq, Score: 1.0, Timestamp: int64(seq)})
		eng.Ingest(Signal{PlayerID: "b", Seq: seq, Score: 0.0, Timestamp: int64(seq)})
	}
	sa, _ := eng.Snapshot("a")
	sb, _ := eng.Snapshot("b")
	if sa.Difficulty == sb.Difficulty {
		t.Fatal("player states contaminated each other")
	}
	if sa.Difficulty <= testConfig().InitialDifficulty {
		t.Fatal("high performer difficulty should increase")
	}
	if sb.Difficulty >= testConfig().InitialDifficulty {
		t.Fatal("low performer difficulty should decrease")
	}
}
