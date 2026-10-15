package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// withConfigDir points os.UserConfigDir at a temp directory on every platform: HOME (macOS,
// and Linux without XDG_CONFIG_HOME), XDG_CONFIG_HOME (Linux) and AppData (Windows).
func withConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		key := key
		old, had := os.LookupEnv(key)
		os.Setenv(key, dir)
		t.Cleanup(func() {
			if had {
				os.Setenv(key, old)
			} else {
				os.Unsetenv(key)
			}
		})
	}
	return dir
}

// The device id is made once, kept outside QUBE_CONFIG's file, and read back unchanged.
func TestDeviceIDIsMadeOnceAndSharedAcrossConfigs(t *testing.T) {
	dir := withConfigDir(t)
	withConfig(t)
	if id, err := ExistingDeviceID(); err != nil || id != "" {
		t.Fatalf("before first use: %q %v", id, err)
	}
	id, err := DeviceID()
	if err != nil || len(id) != 36 {
		t.Fatalf("DeviceID() = %q, %v", id, err)
	}
	p, _ := DeviceIDPath()
	if rel, err := filepath.Rel(dir, p); err != nil || strings.HasPrefix(rel, "..") || filepath.Base(p) != "device-id" {
		t.Fatalf("device id at %s, outside %s", p, dir)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(p); info.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %o", info.Mode().Perm())
		}
	}
	// another config on the same machine gets the same id
	withConfig(t)
	again, err := DeviceID()
	if err != nil || again != id {
		t.Fatalf("second DeviceID() = %q, %v; want %q", again, err, id)
	}
	if existing, _ := ExistingDeviceID(); existing != id {
		t.Fatalf("ExistingDeviceID() = %q", existing)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("a temp file was left behind: %v", entries)
	}
}

// A device-id file that doesn't hold a UUID is replaced by a new one.
func TestDeviceIDReplacesAnUnreadableFile(t *testing.T) {
	withConfigDir(t)
	p, _ := DeviceIDPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := DeviceID()
	if err != nil || len(id) != 36 {
		t.Fatalf("DeviceID() = %q, %v", id, err)
	}
	if data, _ := os.ReadFile(p); strings.TrimSpace(string(data)) != id {
		t.Fatalf("file holds %q", data)
	}
}

// The act-as overlay lives under the host's session, survives a round trip, and an expired
// grant or approval code reads as expired.
func TestActAsOverlayRoundTrip(t *testing.T) {
	p := withConfig(t)
	budget := 25
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	f := &File{Version: 1, Sessions: map[string]Session{
		"https://qubesync.com": {
			Token: "qct_staff", Actor: "agent", AgentName: "Claude Code", Permissions: []string{"admin.read", "org.act"},
			ActingAs: &ActAs{Token: "qct_grant", OrganizationID: "org-1", Organization: "Acme Corp", Scope: "sandbox", Access: "configure",
				WriteBudget: &budget, ExpiresAt: future, Apps: []App{{ID: "app-1", Name: "Acme Sandbox", Sandbox: true}},
				DefaultConnections: map[string]Connection{"app-1": {ID: "conn-1"}}},
			PendingActAs: &PendingActAs{DeviceCode: "qcd_x", UserCode: "ABCD-EFGH", ExpiresAt: past, OrganizationID: "org-2"},
		},
	}}
	if err := f.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	for _, key := range []string{`"acting_as"`, `"pending_act_as"`, `"agent_name": "Claude Code"`, `"permissions"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("config lacks %s:\n%s", key, raw)
		}
	}
	g, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	s := g.Sessions["https://qubesync.com"]
	if !s.Agent() || !s.Staff() || s.ActingAs == nil || s.ActingAs.Token != "qct_grant" || *s.ActingAs.WriteBudget != 25 ||
		s.ActingAs.Apps[0].Name != "Acme Sandbox" || s.ActingAs.DefaultConnections["app-1"].ID != "conn-1" {
		t.Fatalf("round trip: %+v %+v", s, s.ActingAs)
	}
	if s.ActingAs.Expired() || !s.PendingActAs.Expired() || s.ActingAs.ReadOnly() {
		t.Fatal("expiry or access read wrong")
	}
	// an ordinary session writes none of the staff keys
	plain := &File{Version: 1, Sessions: map[string]Session{"https://qubesync.com": {Token: "qct_x"}}}
	if err := plain.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(p)
	for _, key := range []string{"acting_as", "pending_act_as", "actor", "permissions"} {
		if strings.Contains(string(raw), key) {
			t.Errorf("an ordinary session's config has %s:\n%s", key, raw)
		}
	}
}
