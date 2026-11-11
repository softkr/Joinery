// Package store persists saved connections. Metadata goes to a 0600 JSON file
// in the user config dir; passwords go to the OS keychain, never to disk.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/zalando/go-keyring"

	"dbcat/internal/db"
)

const keyringService = "dbcat"

type Store struct {
	mu    sync.Mutex
	path  string
	conns []db.Config
}

// Open loads (or creates) the store at <user config dir>/dbcat/connections.json.
func Open() (*Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(dir, "dbcat", "connections.json"))
}

func OpenAt(path string) (*Store, error) {
	s := &Store{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.conns); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

func (s *Store) List() []db.Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]db.Config(nil), s.conns...)
}

// Get returns the config with its password filled in from the keychain.
func (s *Store) Get(id string) (db.Config, bool) {
	s.mu.Lock()
	var cfg db.Config
	found := false
	for _, c := range s.conns {
		if c.ID == id {
			cfg, found = c, true
			break
		}
	}
	s.mu.Unlock()
	if !found {
		return cfg, false
	}
	if cfg.Kind != db.SQLite {
		if pw, err := keyring.Get(keyringService, cfg.ID); err == nil {
			cfg.Password = pw
		}
		if cfg.SSH != nil {
			ssh := *cfg.SSH // do not mutate the shared copy held by the store
			if pw, err := keyring.Get(keyringService, sshKey(cfg.ID)); err == nil {
				ssh.Password = pw
			}
			cfg.SSH = &ssh
		}
	}
	return cfg, true
}

// Save inserts or updates a connection. An empty ID creates a new one.
// For existing connections an empty password keeps the stored one.
func (s *Store) Save(cfg db.Config) (db.Config, error) {
	if cfg.ID == "" {
		id, err := newID()
		if err != nil {
			return cfg, err
		}
		cfg.ID = id
	}
	if cfg.Kind != db.SQLite && cfg.Password != "" {
		if err := keyring.Set(keyringService, cfg.ID, cfg.Password); err != nil {
			return cfg, fmt.Errorf("키체인에 비밀번호를 저장하지 못했습니다: %w", err)
		}
	}
	if cfg.Kind != db.SQLite && cfg.SSH != nil && cfg.SSH.Password != "" {
		if err := keyring.Set(keyringService, sshKey(cfg.ID), cfg.SSH.Password); err != nil {
			return cfg, fmt.Errorf("키체인에 SSH 비밀번호를 저장하지 못했습니다: %w", err)
		}
	}
	if cfg.SSH == nil {
		_ = keyring.Delete(keyringService, sshKey(cfg.ID)) // tunnel turned off: drop the secret
	}
	// Secrets now live in the keychain; do not keep them in memory with the list.
	stored := cfg
	stored.Password = ""
	if cfg.SSH != nil {
		ssh := *cfg.SSH
		ssh.Password = ""
		stored.SSH = &ssh
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i, c := range s.conns {
		if c.ID == cfg.ID {
			s.conns[i], replaced = stored, true
			break
		}
	}
	if !replaced {
		s.conns = append(s.conns, stored)
	}
	return cfg, s.flush()
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.conns[:0]
	for _, c := range s.conns {
		if c.ID != id {
			out = append(out, c)
		}
	}
	s.conns = out
	_ = keyring.Delete(keyringService, id) // missing entries are fine
	_ = keyring.Delete(keyringService, sshKey(id))
	return s.flush()
}

func (s *Store) flush() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.conns, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// sshKey is the keychain account holding a connection's SSH password/passphrase.
func sshKey(id string) string { return id + "/ssh" }

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
