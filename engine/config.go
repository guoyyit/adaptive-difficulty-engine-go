package engine

import "fmt"

// Config 是调节参数与目标区间配置，支持运行时原子更新。
type Config struct {
	MinDifficulty     float64 // 难度合法下界
	MaxDifficulty     float64 // 难度合法上界
	InitialDifficulty float64 // 新玩家初始难度
	TargetLow         float64 // 目标表现区间下界（得分越高于区间，难度越上调）
	TargetHigh        float64 // 目标表现区间上界
	SmoothingAlpha    float64 // EWMA 平滑系数 (0,1]，越小越平滑
	MaxStep           float64 // 单周期难度最大变化量（防跳变）
	Gain              float64 // 比例调节增益
	SignalsPerCycle   uint64  // 每个调节周期所需信号数
	ReorderWindow     uint64  // 乱序重排缓冲窗口大小（按序号跨度计）
	BurstLimit        uint64  // 单周期突发信号上限，超出部分降级丢弃
	IdleGapMs         int64   // 相邻信号时间间隔超过该值视为长期无信号，EWMA 向中性衰减
}

// DefaultConfig 返回一套可用的默认配置。
func DefaultConfig() Config {
	return Config{
		MinDifficulty:     0.0,
		MaxDifficulty:     10.0,
		InitialDifficulty: 5.0,
		TargetLow:         0.45,
		TargetHigh:        0.55,
		SmoothingAlpha:    0.3,
		MaxStep:           0.5,
		Gain:              2.0,
		SignalsPerCycle:   4,
		ReorderWindow:     8,
		BurstLimit:        16,
		IdleGapMs:         60_000,
	}
}

// Validate 校验配置合法性。
func (c Config) Validate() error {
	if !(c.MinDifficulty < c.MaxDifficulty) {
		return fmt.Errorf("require MinDifficulty < MaxDifficulty, got [%v,%v]", c.MinDifficulty, c.MaxDifficulty)
	}
	if c.InitialDifficulty < c.MinDifficulty || c.InitialDifficulty > c.MaxDifficulty {
		return fmt.Errorf("InitialDifficulty %v out of [%v,%v]", c.InitialDifficulty, c.MinDifficulty, c.MaxDifficulty)
	}
	if !(0 <= c.TargetLow && c.TargetLow <= c.TargetHigh && c.TargetHigh <= 1) {
		return fmt.Errorf("require 0 <= TargetLow <= TargetHigh <= 1, got [%v,%v]", c.TargetLow, c.TargetHigh)
	}
	if c.SmoothingAlpha <= 0 || c.SmoothingAlpha > 1 {
		return fmt.Errorf("SmoothingAlpha must be in (0,1], got %v", c.SmoothingAlpha)
	}
	if c.MaxStep <= 0 {
		return fmt.Errorf("MaxStep must be > 0, got %v", c.MaxStep)
	}
	if c.Gain <= 0 {
		return fmt.Errorf("Gain must be > 0, got %v", c.Gain)
	}
	if c.SignalsPerCycle == 0 {
		return fmt.Errorf("SignalsPerCycle must be >= 1")
	}
	if c.ReorderWindow == 0 {
		return fmt.Errorf("ReorderWindow must be >= 1")
	}
	if c.BurstLimit == 0 {
		return fmt.Errorf("BurstLimit must be >= 1")
	}
	if c.IdleGapMs < 0 {
		return fmt.Errorf("IdleGapMs must be >= 0")
	}
	return nil
}
