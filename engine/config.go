package engine

import (
	"errors"
	"math"
)

// Config controls the adjustment model. It is treated as an immutable
// value: updates are validated and then atomically swapped, so no reader
// can ever observe a partially applied configuration.
type Config struct {
	Version           int64   `json:"version"`
	MinDifficulty     float64 `json:"min_difficulty"`
	MaxDifficulty     float64 `json:"max_difficulty"`
	DefaultDifficulty float64 `json:"default_difficulty"`
	TargetPerfLo      float64 `json:"target_perf_lo"`
	TargetPerfHi      float64 `json:"target_perf_hi"`
	EwmaAlpha         float64 `json:"ewma_alpha"`
	AdjustGain        float64 `json:"adjust_gain"`
	MaxStep           float64 `json:"max_step"`
	TickSize          int     `json:"tick_size"`
	LateWindow        int64   `json:"late_window"`
	BurstLimit        int     `json:"burst_limit"`
	IdleTicks         int     `json:"idle_ticks"`
	IdleRelax         float64 `json:"idle_relax"`
}

// DefaultConfig returns the factory configuration.
func DefaultConfig() Config {
	return Config{
		Version:           1,
		MinDifficulty:     0.1,
		MaxDifficulty:     5.0,
		DefaultDifficulty: 1.0,
		TargetPerfLo:      0.4,
		TargetPerfHi:      0.6,
		EwmaAlpha:         0.2,
		AdjustGain:        0.15,
		MaxStep:           0.3,
		TickSize:          8,
		LateWindow:        64,
		BurstLimit:        20,
		IdleTicks:         12,
		IdleRelax:         0.02,
	}
}

// Validate checks the configuration for consistency.
func (c Config) Validate() error {
	if c.MinDifficulty <= 0 || c.MaxDifficulty <= 0 {
		return errors.New("difficulty bounds must be positive")
	}
	if c.MinDifficulty >= c.MaxDifficulty {
		return errors.New("min difficulty must be less than max")
	}
	if c.DefaultDifficulty < c.MinDifficulty || c.DefaultDifficulty > c.MaxDifficulty {
		return errors.New("default difficulty must lie within [min,max]")
	}
	if math.IsNaN(c.TargetPerfLo) || math.IsNaN(c.TargetPerfHi) {
		return errors.New("target performance bounds must be numbers")
	}
	if c.TargetPerfLo < 0 || c.TargetPerfHi > 1 || c.TargetPerfLo >= c.TargetPerfHi {
		return errors.New("target performance interval must be within [0,1] with lo < hi")
	}
	if c.EwmaAlpha <= 0 || c.EwmaAlpha > 1 {
		return errors.New("ewma alpha must be in (0,1]")
	}
	if c.AdjustGain <= 0 {
		return errors.New("adjust gain must be positive")
	}
	if c.MaxStep <= 0 {
		return errors.New("max step must be positive")
	}
	if c.TickSize <= 0 {
		return errors.New("tick size must be positive")
	}
	if c.LateWindow <= 0 {
		return errors.New("late window must be positive")
	}
	if c.BurstLimit <= 0 {
		return errors.New("burst limit must be positive")
	}
	if c.IdleTicks < 0 {
		return errors.New("idle ticks must be non-negative")
	}
	if c.IdleRelax < 0 {
		return errors.New("idle relax must be non-negative")
	}
	return nil
}
