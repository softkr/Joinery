package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dbcat/internal/db"
)

func TestSaveListDeleteSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "c.json")
	s, err := OpenAt(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := s.Save(db.Config{Name: "local", Kind: db.SQLite, Database: "/tmp/x.db", Password: "ignored"})
	if err != nil || cfg.ID == "" {
		t.Fatalf("save: %+v %v", cfg, err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("store file must be 0600: %v %v", st, err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "ignored") {
		t.Fatal("password leaked into JSON file")
	}

	// SSH secrets must not reach the JSON file or the in-memory list either.
	ssh, err := s.Save(db.Config{Name: "tun", Kind: db.Postgres, Host: "db", SSLMode: db.SSLVerify,
		SSH: &db.SSHConfig{Host: "bastion", User: "u", Password: "sshsecret"}, Password: "dbsecret"})
	if err != nil {
		t.Logf("keychain unavailable, skipping SSH secret check: %v", err)
	} else {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), "sshsecret") || strings.Contains(string(raw), "dbsecret") {
			t.Fatal("secret leaked into JSON file")
		}
		if l := s.List(); l[len(l)-1].Password != "" || l[len(l)-1].SSH.Password != "" {
			t.Fatal("secret kept in memory with the list")
		}
		if got, _ := s.Get(ssh.ID); got.SSH == nil || got.SSH.Password != "sshsecret" || got.SSLMode != db.SSLVerify {
			t.Fatalf("keychain round trip failed: %+v", got.SSH)
		}
		if err := s.Delete(ssh.ID); err != nil {
			t.Fatal(err)
		}
		if got, ok := s.Get(ssh.ID); ok || got.SSH != nil {
			t.Fatal("deleted connection still readable")
		}
	}

	s2, err := OpenAt(path) // reload from disk
	if err != nil || len(s2.List()) != 1 {
		t.Fatalf("reload: %v %v", s2.List(), err)
	}
	got, ok := s2.Get(cfg.ID)
	if !ok || got.Database != "/tmp/x.db" {
		t.Fatalf("get: %+v %v", got, ok)
	}

	cfg.Name = "renamed"
	if _, err := s2.Save(cfg); err != nil || len(s2.List()) != 1 || s2.List()[0].Name != "renamed" {
		t.Fatalf("update: %v %v", s2.List(), err)
	}
	if err := s2.Delete(cfg.ID); err != nil || len(s2.List()) != 0 {
		t.Fatalf("delete: %v %v", s2.List(), err)
	}
}
