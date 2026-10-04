package engine

// Signal is a single performance event emitted by a player.
// Signals are uniquely identified by ID and ordered per player by Seq.
type Signal struct {
	ID       string  `json:"id"`
	PlayerID string  `json:"player_id"`
	Seq      int64   `json:"seq"`
	Value    float64 `json:"value"` // performance in [0,1], higher is better
}

// Status describes how the engine treated a signal.
type Status int

const (
	StatusApplied   Status = iota // signal was accepted and applied
	StatusDuplicate               // signal ID was already seen
	StatusExpired                 // signal arrived too late (seq outside reorder window)
	StatusInvalid                 // signal failed validation
	StatusThrottled               // signal was dropped by the burst policy
)

func (s Status) String() string {
	switch s {
	case StatusApplied:
		return "applied"
	case StatusDuplicate:
		return "duplicate"
	case StatusExpired:
		return "expired"
	case StatusInvalid:
		return "invalid"
	case StatusThrottled:
		return "throttled"
	default:
		return "unknown"
	}
}

// TickInfo describes the outcome of one adjustment period for one player.
type TickInfo struct {
	Ticked        bool    // whether an adjustment ran (false when idle)
	Idle          bool    // true when no pending signals existed
	OldDifficulty float64
	NewDifficulty float64
	Ewma          float64 // smoothed performance after the tick
	Applied       int     // number of signals incorporated
	Throttled     int     // number of signals dropped by burst policy
	Err           error   `json:"-"`
}

// Result is the outcome of ingesting a single signal.
type Result struct {
	Status     Status
	PlayerID   string
	Seq        int64
	Difficulty float64 // difficulty after processing the signal
	Tick       *TickInfo
	Err        error  `json:"-"`
	Reason     string // human-readable detail for invalid/expired/throttled
}

// PlayerSnapshot is an immutable, self-consistent view of a player's state,
// published atomically so readers never observe partial updates.
type PlayerSnapshot struct {
	PlayerID      string
	Difficulty    float64
	Ewma          float64 // -1 when no signal has been incorporated yet
	LastSeq       int64   // highest contiguous sequence applied
	Pending       int     // signals waiting for the next tick
	ConfigVersion int64
	IdleTicks     int
}

// PlayerData is the persistent form of a player's committed state.
type PlayerData struct {
	PlayerID      string   `json:"player_id"`
	Difficulty    float64 `json:"difficulty"`
	Ewma          float64 `json:"ewma"`
	LastSeq       int64    `json:"last_seq"`
	Seen          []string `json:"seen"`
	IdleTicks     int      `json:"idle_ticks"`
	ConfigVersion int64    `json:"config_version"`
}
