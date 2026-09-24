package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func withConfig(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nested", "credentials.json")
	old, had := os.LookupEnv("QUBE_CONFIG")
	os.Setenv("QUBE_CONFIG", p)
	t.Cleanup(func() {
		if had {
			os.Setenv("QUBE_CONFIG", old)
		} else {
			os.Unsetenv("QUBE_CONFIG")
		}
	})
	return p
}

func TestNormalizeHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                          DefaultHost,
		"dev.qubesync.com":          "https://dev.qubesync.com",
		"https://dev.qubesync.com/": "https://dev.qubesync.com",
		" http://localhost:4002/ ":  "http://localhost:4002",
		"https://qubesync.com//":    "https://qubesync.com",
	} {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	withConfig(t)
	f, err := Load()
	if err != nil || len(f.Sessions) != 0 || f.Version != 1 {
		t.Fatalf("got %+v, %v", f, err)
	}
}

func TestSaveRoundTripAndPermissions(t *testing.T) {
	p := withConfig(t)
	f := &File{Version: 1, CurrentHost: "https://dev.qubesync.com", Sessions: map[string]Session{
		"https://dev.qubesync.com": {Token: "qct_x", Scope: "sandbox", DefaultApp: "app-1"},
	}}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	g, err := Load()
	if err != nil || g.CurrentHost != f.CurrentHost || g.Sessions["https://dev.qubesync.com"].Token != "qct_x" {
		t.Fatalf("round trip: %+v %v", g, err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o, want 0600", info.Mode().Perm())
		}
		dir, _ := os.Stat(filepath.Dir(p))
		if dir.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %o, want 0700", dir.Mode().Perm())
		}
	}
	// no temp file left behind
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("expected only the credentials file, got %d entries", len(entries))
	}
}

func TestResolveHost(t *testing.T) {
	f := &File{Sessions: map[string]Session{}}
	if f.ResolveHost("") != DefaultHost {
		t.Fatal("no sessions -> production")
	}
	if f.ResolveHost("dev.qubesync.com") != "https://dev.qubesync.com" {
		t.Fatal("explicit wins and is normalised")
	}
	f.Sessions["https://dev.qubesync.com"] = Session{}
	if f.ResolveHost("") != "https://dev.qubesync.com" {
		t.Fatal("a single session is the default")
	}
	f.Sessions["https://qubesync.com"] = Session{}
	if f.ResolveHost("") != DefaultHost {
		t.Fatal("two sessions and no current host -> production")
	}
	f.CurrentHost = "https://dev.qubesync.com"
	if f.ResolveHost("") != "https://dev.qubesync.com" {
		t.Fatal("the current host wins over the default")
	}
	f.CurrentHost = "https://gone.example"
	if f.ResolveHost("") != DefaultHost {
		t.Fatal("a current host without a session is ignored")
	}
}

func TestSessionExpired(t *testing.T) {
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if !(Session{ExpiresAt: past}).Expired() || (Session{ExpiresAt: future}).Expired() || (Session{}).Expired() {
		t.Fatal("expiry check")
	}
}
