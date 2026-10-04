package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// stateFileName 是持久化文件名。写入采用 临时文件+rename，保证落盘原子性。
const stateFileName = "state.json"

// persistedPlayer 是单玩家状态的可序列化形式。
// Committed 难度（Difficulty/Cycle）与未完成的调节进度
// （CycleCount/BurstSeen/EWMA 等）分别保存，重启后可明确区分。
type persistedPlayer struct {
	Difficulty float64           `json:"difficulty"` // 已提交难度
	Cycle      uint64            `json:"cycle"`      // 已完成的调节周期数
	EWMA       float64           `json:"ewma"`
	EWMAInit   bool              `json:"ewma_init"`
	AppliedSeq uint64            `json:"applied_seq"`
	Pending    map[uint64]Signal `json:"pending,omitempty"` // 未完成的乱序缓冲
	Recent     []uint64          `json:"recent,omitempty"`
	CycleCount uint64            `json:"cycle_count"` // 未完成周期内已计入的信号数
	BurstSeen  uint64            `json:"burst_seen"`
	LastTs     int64             `json:"last_ts"`
	LastTsSet  bool              `json:"last_ts_set"`
}

type persistedState struct {
	Version int                        `json:"version"`
	Players map[string]persistedPlayer `json:"players"`
}

// store 负责难度状态的持久化与重启恢复。
type store struct {
	dir string
}

func newStore(dir string) *store {
	return &store{dir: dir}
}

// saveCommitted 将全部玩家状态原子落盘。
func (s *store) saveCommitted(states map[string]*playerState) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	ps := persistedState{Version: 1, Players: make(map[string]persistedPlayer, len(states))}
	for id, p := range states {
		recent := make([]uint64, 0, len(p.recent))
		for seq := range p.recent {
			recent = append(recent, seq)
		}
		ps.Players[id] = persistedPlayer{
			Difficulty: p.difficulty,
			Cycle:      p.cycle,
			EWMA:       p.ewma,
			EWMAInit:   p.ewmaInit,
			AppliedSeq: p.appliedSeq,
			Pending:    p.pending,
			Recent:     recent,
			CycleCount: p.cycleCount,
			BurstSeen:  p.burstSeen,
			LastTs:     p.lastTs,
			LastTsSet:  p.lastTsSet,
		}
	}
	data, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(s.dir, stateFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, stateFileName))
}

// load 恢复全部玩家状态；恢复出的状态 committed=true，
// 其中 CycleCount>0 的玩家存在未完成的调节周期，与已提交难度可区分。
func (s *store) load() (map[string]*playerState, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, stateFileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return nil, fmt.Errorf("decode %s: %w", stateFileName, err)
	}
	out := make(map[string]*playerState, len(ps.Players))
	for id, pp := range ps.Players {
		p := &playerState{
			difficulty: pp.Difficulty,
			cycle:      pp.Cycle,
			ewma:       pp.EWMA,
			ewmaInit:   pp.EWMAInit,
			appliedSeq: pp.AppliedSeq,
			pending:    pp.Pending,
			recent:     make(map[uint64]bool, len(pp.Recent)),
			cycleCount: pp.CycleCount,
			burstSeen:  pp.BurstSeen,
			lastTs:     pp.LastTs,
			lastTsSet:  pp.LastTsSet,
			committed:  true,
		}
		if p.pending == nil {
			p.pending = make(map[uint64]Signal)
		}
		for _, seq := range pp.Recent {
			p.recent[seq] = true
		}
		out[id] = p
	}
	return out, nil
}
