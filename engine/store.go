package engine

import "sync"

// Store is the persistence boundary. A player's committed state
// (difficulty, ewma, seen set) is written via WriteSnapshot; signals that
// have been ingested but not yet committed by a tick are appended to the
// WAL. On recovery, snapshots represent committed difficulty while WAL
// entries represent in-flight adjustments that must be replayed.
type Store interface {
	WriteSnapshot(playerID string, data PlayerData) error
	AppendWAL(playerID string, sig Signal) error
	TruncateWAL(playerID string) error
	Load() (snapshots map[string]PlayerData, wal map[string][]Signal, err error)
	WriteConfig(cfg Config) error
	LoadConfig() (*Config, error)
	Close() error
}

// MemoryStore is an in-memory Store used by default and in tests.
type MemoryStore struct {
	mu        sync.Mutex
	snapshots map[string]PlayerData
	wal       map[string][]Signal
	cfg       *Config
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		snapshots: make(map[string]PlayerData),
		wal:       make(map[string][]Signal),
	}
}

func (s *MemoryStore) WriteSnapshot(playerID string, data PlayerData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := data
	cp.Seen = append([]string(nil), data.Seen...)
	s.snapshots[playerID] = cp
	return nil
}

func (s *MemoryStore) AppendWAL(playerID string, sig Signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wal[playerID] = append(s.wal[playerID], sig)
	return nil
}

func (s *MemoryStore) TruncateWAL(playerID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.wal, playerID)
	return nil
}

func (s *MemoryStore) Load() (map[string]PlayerData, map[string][]Signal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	snaps := make(map[string]PlayerData, len(s.snapshots))
	for k, v := range s.snapshots {
		cp := v
		cp.Seen = append([]string(nil), v.Seen...)
		snaps[k] = cp
	}
	wal := make(map[string][]Signal, len(s.wal))
	for k, v := range s.wal {
		cp := append([]Signal(nil), v...)
		wal[k] = cp
	}
	return snaps, wal, nil
}

func (s *MemoryStore) WriteConfig(cfg Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cfg
	s.cfg = &c
	return nil
}

func (s *MemoryStore) LoadConfig() (*Config, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg == nil {
		return nil, nil
	}
	c := *s.cfg
	return &c, nil
}

func (s *MemoryStore) Close() error { return nil }
