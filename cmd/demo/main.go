// demo 以固定随机种子生成信号序列，演示难度收敛与重启恢复，输出可复现。
package main

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"

	"github.com/guoyyit/adaptive-difficulty-engine-go/engine"
)

func main() {
	dir := filepath.Join(os.TempDir(), "ade-demo")
	os.RemoveAll(dir) // 保证每次运行从干净状态开始，输出可复现

	cfg := engine.DefaultConfig()
	eng, err := engine.New(cfg, dir)
	must(err)

	rng := rand.New(rand.NewSource(42))
	players := []string{"alice", "bob", "carol"}
	seqs := map[string]uint64{}

	// 生成 300 条信号：玩家水平固定，难度应收敛到使表现落入目标区间的位置。
	for i := 0; i < 300; i++ {
		id := players[rng.Intn(len(players))]
		seqs[id]++
		snap, _ := eng.Snapshot(id)
		// 模拟玩家表现：难度越高于其水平，得分越低。
		skill := 6.0
		score := 0.5 + (skill-snap.Difficulty)*0.1 + (rng.Float64()-0.5)*0.2
		res := eng.Ingest(engine.Signal{PlayerID: id, Seq: seqs[id], Score: score, Timestamp: int64(i) * 1000})
		if res.Adjustment != nil {
			a := res.Adjustment
			fmt.Printf("player=%s cycle=%d difficulty %.4f -> %.4f\n", a.PlayerID, a.Cycle, a.Old, a.New)
		}
	}

	fmt.Println("--- before close ---")
	for _, id := range players {
		snap, _ := eng.Snapshot(id)
		fmt.Printf("player=%s difficulty=%.4f cycle=%d committed=%v\n", id, snap.Difficulty, snap.Cycle, snap.Committed)
	}
	must(eng.Close())

	// 重启恢复：已提交难度与未完成的调节进度都被还原。
	eng2, err := engine.New(cfg, dir)
	must(err)
	fmt.Println("--- after restart ---")
	for _, id := range players {
		snap, _ := eng2.Snapshot(id)
		fmt.Printf("player=%s difficulty=%.4f cycle=%d committed=%v\n", id, snap.Difficulty, snap.Cycle, snap.Committed)
	}
	must(eng2.Close())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
