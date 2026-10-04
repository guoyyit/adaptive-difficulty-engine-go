// Package engine 实现按玩家维度的动态难度调节引擎。
package engine

// Signal 是一条玩家表现信号。
type Signal struct {
	PlayerID  string  // 玩家标识
	Seq       uint64  // 玩家维度单调递增序号，用于去重与乱序重排
	Score     float64 // 表现得分，合法范围 [0,1]
	Timestamp int64   // 信号产生时间（逻辑时钟，毫秒）
}

// IngestStatus 表示信号摄入的处理结果类别，各类别可区分。
type IngestStatus int

const (
	StatusAccepted     IngestStatus = iota // 已接受并计入
	StatusBuffered                         // 乱序到达，已缓存等待前序信号
	StatusDuplicate                        // 重复信号，未重复累计
	StatusExpired                          // 迟到/已过期信号，不再影响难度
	StatusOutOfRange                       // 越界信号，已按策略降级
	StatusBurstDropped                     // 突发流量降级丢弃
)

// IngestResult 是单次信号摄入的结果。
type IngestResult struct {
	Status IngestStatus
	Detail string
	// Adjustment 非空表示本次摄入恰好完成一个调节周期并产出了结果。
	Adjustment *Adjustment
}

// Adjustment 是一次调节周期的产出结果。
type Adjustment struct {
	PlayerID    string
	Cycle       uint64  // 调节周期号，同一玩家同一周期只产生一个结果
	Old         float64 // 调节前难度
	New         float64 // 调节后难度
	SignalsUsed int     // 本周期计入的信号数
}

// Snapshot 是对外可见的某玩家自洽完整难度状态。
type Snapshot struct {
	PlayerID   string
	Difficulty float64 // 当前难度，恒在合法范围内
	Cycle      uint64  // 已完成的调节周期数
	AppliedSeq uint64  // 已生效的最大信号序号
	Committed  bool    // 是否已持久化（重启后可与未完成状态区分）
}
