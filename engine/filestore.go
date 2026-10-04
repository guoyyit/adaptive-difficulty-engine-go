package engine

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
)

// FileStore persists committed state as JSON snapshots and in-flight
// signals as an append-only write-ahead log. All commits are atomic
// (write temp, fsync, rename), so a crash never leaves a partially
// written snapshot.
type FileStore struct {
	base string
}

// NewFileStore creates or opens a store rooted at base.
func NewFileStore(base string) (*FileStore, error) {
	s := &FileStore{base: base}
	if err := os.MkdirAll(filepath.Join(base, "players"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(base, "wal"), 0o755); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileStore) playerPath(playerID string) string {
	return filepath.Join(s.base, "players", url.QueryEscape(playerID)+".json")
}

func (s *FileStore) walPath(playerID string) string {
	return filepath.Join(s.base, "wal", url.QueryEscape(playerID)+".log")
}

func (s *FileStore) configPath() string {
	return filepath.Join(s.base, "config.json")
}

// WriteSnapshot atomically writes the committed state for a player.
func (s *FileStore) WriteSnapshot(playerID string, data PlayerData) error {
	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.playerPath(playerID), b)
}

// AppendWAL appends a signal to the player's write-ahead log and fsyncs.
func (s *FileStore) AppendWAL(playerID string, sig Signal) error {
	f, err := os.OpenFile(s.walPath(playerID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(sig)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err := f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}

// TruncateWAL removes a player's WAL after a committed snapshot.
func (s *FileStore) TruncateWAL(playerID string) error {
	err := os.Remove(s.walPath(playerID))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// Load reads all snapshots and WAL entries.
func (s *FileStore) Load() (map[string]PlayerData, map[string][]Signal, error) {
	snapshots := make(map[string]PlayerData)
	wal := make(map[string][]Signal)

	entries, err := os.ReadDir(filepath.Join(s.base, "players"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.base, "players", ent.Name()))
		if err != nil {
			return nil, nil, err
		}
		var data PlayerData
		if err := json.Unmarshal(b, &data); err != nil {
			return nil, nil, err
		}
		snapshots[data.PlayerID] = data
	}

	walEntries, err := os.ReadDir(filepath.Join(s.base, "wal"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, nil, err
	}
	for _, ent := range walEntries {
		if ent.IsDir() {
			continue
		}
		pid, err := unescapePlayer(ent.Name())
		if err != nil {
			return nil, nil, err
		}
		sigs, err := readWAL(filepath.Join(s.base, "wal", ent.Name()))
		if err != nil {
			return nil, nil, err
		}
		if len(sigs) > 0 {
			wal[pid] = sigs
		}
	}
	return snapshots, wal, nil
}

// WriteConfig atomically writes the engine configuration.
func (s *FileStore) WriteConfig(cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.configPath(), b)
}

// LoadConfig reads the persisted configuration, returning nil if absent.
func (s *FileStore) LoadConfig() (*Config, error) {
	b, err := os.ReadFile(s.configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Close syncs the base directory.
func (s *FileStore) Close() error {
	dir, err := os.Open(s.base)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func readWAL(path string) ([]Signal, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var sigs []Signal
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var sig Signal
		if err := json.Unmarshal(line, &sig); err != nil {
			return nil, err
		}
		sigs = append(sigs, sig)
	}
	return sigs, sc.Err()
}

func unescapePlayer(name string) (string, error) {
	const suffix = ".log"
	if len(name) < len(suffix) || name[len(name)-len(suffix):] != suffix {
		return "", errors.New("unexpected wal file name: " + name)
	}
	esc := name[:len(name)-len(suffix)]
	return url.QueryUnescape(esc)
}

// atomicWrite writes b to path via a temp file, fsync and rename, then
// fsyncs the containing directory so the rename itself is durable.
func atomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(b); err != nil {
		cleanup()
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
