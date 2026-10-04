package engine

import "math"

// neutralScore 是长期无信号时 EWMA 的衰减目标（表现区间中性的默认值）。
const neutralScore = 0.5

// playerState 维护单个玩家的难度状态机。
// 所有字段仅由引擎持有的分片锁保护，对外不直接暴露。
//
// 调节模型：
//   - 每个调节周期累计 SignalsPerCycle 条有效信号，期间用 EWMA 平滑得分；
//   - 周期结束时按 ewma 与目标区间的偏差做比例调节，步长被 MaxStep 截断，
//     难度始终被夹在 [MinDifficulty, MaxDifficulty] 内；
//   - 偏差在目标区间内时不调节（死区），保证收敛后不震荡。
type playerState struct {
	difficulty float64
	cycle      uint64

	ewma     float64
	ewmaInit bool

	appliedSeq uint64            // 已连续生效的最大信号序号（水位线）
	pending    map[uint64]Signal // 乱序重排缓冲，键为信号序号
	recent     map[uint64]bool   // 近期已生效序号，用于区分重复与过期

	cycleCount uint64 // 当前周期已计入的信号数
	burstSeen  uint64 // 当前周期已见到的信号数（含被降级丢弃的），用于突发限制

	lastTs    int64
	lastTsSet bool

	lastAdjustment Adjustment
	hasAdjustment  bool

	committed bool // 是否已持久化；重启恢复或落盘成功后为 true
}

func newPlayerState(cfg Config) *playerState {
	return &playerState{
		difficulty: cfg.InitialDifficulty,
		pending:    make(map[uint64]Signal),
		recent:     make(map[uint64]bool),
	}
}

// ingest 在分片锁内处理一条信号，返回可区分的处理结果。
func (p *playerState) ingest(sig Signal, cfg Config) IngestResult {
	// 越界信号：降级为截断后继续计入，结果可区分。
	degraded := false
	if math.IsNaN(sig.Score) || sig.Score < 0 || sig.Score > 1 {
		sig.Score = math.Min(1, math.Max(0, sig.Score))
		degraded = true
	}

	switch {
	case sig.Seq <= p.appliedSeq:
		// 水位线以下：重复或已过期，均不再影响难度。
		if p.recent[sig.Seq] {
			return IngestResult{Status: StatusDuplicate, Detail: "signal already applied"}
		}
		return IngestResult{Status: StatusExpired, Detail: "signal expired (below watermark)"}
	case sig.Seq == p.appliedSeq+1:
		// 恰好是下一条，直接生效并冲刷缓冲。
		counted := p.apply(sig, cfg)
		p.drain(cfg)
		if !counted {
			return IngestResult{Status: StatusBurstDropped, Detail: "burst limit reached in current cycle"}
		}
		if degraded {
			return IngestResult{Status: StatusOutOfRange, Detail: "score clamped into [0,1]"}
		}
		return IngestResult{Status: StatusAccepted}
	default:
		// 乱序到达：序号超前，进入重排缓冲。
		if _, ok := p.pending[sig.Seq]; ok {
			return IngestResult{Status: StatusDuplicate, Detail: "signal already buffered"}
		}
		// 超出重排窗口：认为中间信号已丢失，推进水位线跳过空洞。
		if sig.Seq-p.appliedSeq > cfg.ReorderWindow {
			p.skipGap(sig.Seq - 1)
			counted := p.apply(sig, cfg)
			p.drain(cfg)
			if !counted {
				return IngestResult{Status: StatusBurstDropped, Detail: "burst limit reached in current cycle"}
			}
			if degraded {
				return IngestResult{Status: StatusOutOfRange, Detail: "score clamped into [0,1]"}
			}
			return IngestResult{Status: StatusAccepted, Detail: "gap skipped: missing sequences treated as lost"}
		}
		p.pending[sig.Seq] = sig
		return IngestResult{Status: StatusBuffered, Detail: "out-of-order signal buffered"}
	}
}

// apply 处理一条已到达水位线的信号：推进水位线并按突发策略决定是否计入。
// 返回 false 表示该信号因突发限制被降级丢弃（水位线仍推进，不阻塞后续信号）。
func (p *playerState) apply(sig Signal, cfg Config) bool {
	p.burstSeen++
	counted := p.burstSeen <= cfg.BurstLimit
	if counted {
		p.applyScore(sig, cfg)
	}
	p.appliedSeq = sig.Seq
	p.recent[sig.Seq] = true
	// recent 只保留水位线附近窗口，避免无限增长。
	if uint64(len(p.recent)) > 2*cfg.ReorderWindow {
		for s := range p.recent {
			if s+cfg.ReorderWindow < p.appliedSeq {
				delete(p.recent, s)
			}
		}
	}
	return counted
}

// applyScore 更新 EWMA 与周期状态，并在周期完成时产生一次调节。
func (p *playerState) applyScore(sig Signal, cfg Config) {
	// 长期无信号：EWMA 向中性值衰减一半，避免陈旧表现支配当前难度。
	if p.lastTsSet && p.ewmaInit && sig.Timestamp-p.lastTs > cfg.IdleGapMs {
		p.ewma = neutralScore + (p.ewma-neutralScore)/2
	}
	p.lastTs, p.lastTsSet = sig.Timestamp, true

	if !p.ewmaInit {
		p.ewma, p.ewmaInit = sig.Score, true
	} else {
		p.ewma = cfg.SmoothingAlpha*sig.Score + (1-cfg.SmoothingAlpha)*p.ewma
	}

	p.cycleCount++
	if p.cycleCount >= cfg.SignalsPerCycle {
		p.adjust(cfg)
		p.cycleCount = 0
		p.burstSeen = 0
	}
}

// adjust 执行一次调节周期，产出唯一结果。比例控制 + 死区 + 步长截断 + 范围夹取。
func (p *playerState) adjust(cfg Config) {
	var err float64
	switch {
	case p.ewma > cfg.TargetHigh:
		err = p.ewma - cfg.TargetHigh
	case p.ewma < cfg.TargetLow:
		err = p.ewma - cfg.TargetLow
	default:
		err = 0 // 死区：表现落在目标区间内，不调节
	}
	delta := cfg.Gain * err
	delta = math.Min(cfg.MaxStep, math.Max(-cfg.MaxStep, delta))

	old := p.difficulty
	nd := old + delta
	nd = math.Min(cfg.MaxDifficulty, math.Max(cfg.MinDifficulty, nd))
	p.difficulty = nd
	p.cycle++
	p.lastAdjustment = Adjustment{
		PlayerID:    "",
		Cycle:       p.cycle,
		Old:         old,
		New:         nd,
		SignalsUsed: int(cfg.SignalsPerCycle),
	}
	p.hasAdjustment = true
}

// drain 冲刷重排缓冲中已连续的序号。
func (p *playerState) drain(cfg Config) {
	for {
		sig, ok := p.pending[p.appliedSeq+1]
		if !ok {
			return
		}
		delete(p.pending, sig.Seq)
		p.apply(sig, cfg)
	}
}

// skipGap 将水位线推进到 seq，空洞中的序号视为丢失，缓冲中过期的信号被清除。
func (p *playerState) skipGap(seq uint64) {
	for s := range p.pending {
		if s <= seq {
			delete(p.pending, s)
		}
	}
	p.appliedSeq = seq
}

// snapshot 导出自洽的当前状态。
func (p *playerState) snapshot() Snapshot {
	return Snapshot{
		Difficulty: p.difficulty,
		Cycle:      p.cycle,
		AppliedSeq: p.appliedSeq,
		Committed:  p.committed,
	}
}
