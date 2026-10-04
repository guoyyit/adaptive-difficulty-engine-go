package engine

import (
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
)

// numShards 是分片数量，不同分片的玩家互不阻塞。
const numShards = 64

type shard struct {
	mu      sync.Mutex
	players map[string]*playerState
}

// Engine 是动态难度调节引擎，支持并发摄入与读取。
//
// 并发模型：按玩家 ID 哈希到固定分片，每个分片一把互斥锁。
// 同一玩家的信号在分片锁内串行处理，保证其状态机确定性；
// 不同分片完全并行，单个玩家的调节只持有其分片锁，临界区为纯内存计算，
// 不会阻塞其他玩家的信号摄入，也不会拖垮整体吞吐。
type Engine struct {
	shards [numShards]shard
	cfg    atomic.Value // 存放 Config，整体原子替换，杜绝部分生效
	store  *store       // nil 表示不持久化
}

// New 创建引擎。dir 非空时启用持久化并从该目录恢复上次已提交的状态。
func New(cfg Config, dir string) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	e := &Engine{}
	for i := range e.shards {
		e.shards[i].players = make(map[string]*playerState)
	}
	e.cfg.Store(cfg)
	if dir != "" {
		e.store = newStore(dir)
		snaps, err := e.store.load()
		if err != nil {
			return nil, fmt.Errorf("restore state: %w", err)
		}
		for id, ps := range snaps {
			sh := e.shardOf(id)
			sh.players[id] = ps
		}
	}
	return e, nil
}

func (e *Engine) shardOf(playerID string) *shard {
	h := fnv.New64a()
	h.Write([]byte(playerID))
	return &e.shards[h.Sum64()%numShards]
}

// Ingest 摄入一条表现信号，返回可区分的处理结果；
// 若本次摄入完成了一个调节周期，结果中携带该周期唯一的调节产出。
func (e *Engine) Ingest(sig Signal) IngestResult {
	cfg := e.cfg.Load().(Config)
	sh := e.shardOf(sig.PlayerID)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	p, ok := sh.players[sig.PlayerID]
	if !ok {
		p = newPlayerState(cfg)
		sh.players[sig.PlayerID] = p
	}
	cycleBefore := p.cycle
	res := p.ingest(sig, cfg)
	if p.cycle > cycleBefore && p.hasAdjustment {
		adj := p.lastAdjustment
		adj.PlayerID = sig.PlayerID
		res.Adjustment = &adj
	}
	return res
}

// Snapshot 读取某玩家当前自洽的难度状态（在分片锁内整体拷贝）。
func (e *Engine) Snapshot(playerID string) (Snapshot, bool) {
	sh := e.shardOf(playerID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	p, ok := sh.players[playerID]
	if !ok {
		return Snapshot{}, false
	}
	snap := p.snapshot()
	snap.PlayerID = playerID
	return snap, true
}

// UpdateConfig 运行时原子更新配置：先整体校验，再一次原子替换，
// 更新过程中不会出现部分生效，失败时旧配置完整保留（无回退中间态）。
func (e *Engine) UpdateConfig(cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	e.cfg.Store(cfg)
	return nil
}

// Config 返回当前生效配置。
func (e *Engine) Config() Config {
	return e.cfg.Load().(Config)
}

// Close 将当前所有玩家状态落盘并释放资源。
func (e *Engine) Close() error {
	if e.store == nil {
		return nil
	}
	states := make(map[string]*playerState)
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		for id, p := range sh.players {
			states[id] = p
		}
		sh.mu.Unlock()
	}
	if err := e.store.saveCommitted(states); err != nil {
		return err
	}
	// 落盘成功后统一标记为已提交。
	for i := range e.shards {
		sh := &e.shards[i]
		sh.mu.Lock()
		for _, p := range sh.players {
			p.committed = true
		}
		sh.mu.Unlock()
	}
	return nil
}
