# adaptive-difficulty-engine-go

按玩家维度的动态难度调节引擎：根据玩家近期表现自动调整挑战强度，使难度稳定收敛到预设目标区间。纯本地运行，固定随机种子下结果完全可复现。

## 调节模型

每个玩家独立维护一个状态机，互不污染：

1. **信号摄入**：表现信号 `(PlayerID, Seq, Score, Timestamp)` 到达后，先经过去重 / 乱序重排 / 过期过滤 / 越界降级 / 突发限流（见下），有效信号才计入。
2. **平滑**：有效得分进入 EWMA（指数加权移动平均），系数为 `SmoothingAlpha`。单次异常表现只影响一个指数衰减的权重，不会造成跳变。
3. **周期调节**：每累计 `SignalsPerCycle` 条有效信号完成一个调节周期，产出**唯一**一条 `Adjustment`：
   - `ewma` 落在目标区间 `[TargetLow, TargetHigh]` 内 → 死区，不调节；
   - 高于区间 → 按 `Gain * (ewma - TargetHigh)` 上调难度；低于区间 → 对称下调；
   - 单周期步长被截断到 `[-MaxStep, MaxStep]`（比例控制 + 步长截断保证收敛、不震荡、不发散）；
   - 结果始终夹取在 `[MinDifficulty, MaxDifficulty]` 内，任何情况下难度不越界。
4. **长期无信号**：相邻信号时间间隔超过 `IdleGapMs` 时，EWMA 先向中性值 0.5 衰减一半再计入，避免陈旧表现支配当前难度。

## 信号健壮性

| 情况 | 处理策略 | 返回状态 |
|---|---|---|
| 正常生效 | 计入 EWMA 与周期计数 | `StatusAccepted` |
| 乱序（序号超前但在窗口内） | 进入重排缓冲，前序到达后按序生效 | `StatusBuffered` |
| 序号空洞超过 `ReorderWindow` | 缺失序号视为丢失，推进水位线 | `StatusAccepted`（Detail 注明） |
| 重复（已生效或已缓冲） | 不重复累计 | `StatusDuplicate` |
| 迟到 / 已过期（水位线以下） | 不再影响难度 | `StatusExpired` |
| 得分越界（含 NaN） | 截断到 `[0,1]` 后计入 | `StatusOutOfRange` |
| 单周期突发超过 `BurstLimit` | 丢弃但推进水位线，不阻塞后续 | `StatusBurstDropped` |

## 并发模型

- 玩家按 ID 哈希到 64 个分片，每分片一把互斥锁；同一玩家串行、不同玩家并行。
- 调节是纯内存计算，临界区极短，单个玩家的调节不会阻塞其他玩家的信号摄入。
- `Snapshot` 在分片锁内整体拷贝，对外可见的永远是某个自洽的完整状态。
- 配置存放在 `atomic.Value` 中，`UpdateConfig` 先整体校验再一次原子替换：不存在部分生效，校验失败时旧配置完整保留。
- **确定性**：每个玩家的状态只取决于其自身按序生效的信号序列。固定随机种子与相同信号序列下，无论并发度如何，逐次难度结果完全一致（见 `TestDeterministicAcrossConcurrency`）。

## 持久化与重启恢复

`New(cfg, dir)` 指定目录后启用持久化：

- `Close()` 将全部玩家状态以 JSON 原子落盘（临时文件 + rename）到 `dir/state.json`。
- 已提交难度（`difficulty`/`cycle`）与未完成的调节进度（`cycle_count`/`burst_seen`/`ewma`/乱序缓冲）分别保存。
- 重启后恢复的状态 `Snapshot.Committed=true`，未完成周期从断点继续推进（见 `TestRestartRecovery`）。

## 配置项

| 字段 | 含义 | 默认 |
|---|---|---|
| `MinDifficulty` / `MaxDifficulty` | 难度合法范围 | 0 / 10 |
| `InitialDifficulty` | 新玩家初始难度 | 5 |
| `TargetLow` / `TargetHigh` | 目标表现区间 | 0.45 / 0.55 |
| `SmoothingAlpha` | EWMA 平滑系数 (0,1] | 0.3 |
| `MaxStep` | 单周期最大步长 | 0.5 |
| `Gain` | 比例调节增益 | 2.0 |
| `SignalsPerCycle` | 每周期所需信号数 | 4 |
| `ReorderWindow` | 乱序重排窗口（序号跨度） | 8 |
| `BurstLimit` | 单周期突发上限 | 16 |
| `IdleGapMs` | 长期无信号判定间隔 | 60000 |

## 验证方法

```bash
go build ./...        # 编译
go test ./...         # 单元测试：乱序/重复/迟到/突发/越界/并发/确定性/重启恢复
go test -race ./...   # 竞态检测
go run ./cmd/demo     # 固定种子演示：难度收敛 + 重启恢复，输出可复现
```

演示程序使用固定种子 `42` 生成信号，两次运行输出完全一致。
