package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// skillHost stands in for QuBe Sync's /skills route: it serves body for qube-sync-audit (a 404
// for any other name) and notes every request's Authorization header.
type skillHost struct {
	mu     sync.Mutex
	body   string
	status int
	auth   []string
	paths  []string
}

func serveSkills(t *testing.T, h *skillHost) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.auth = append(h.auth, r.Header.Get("Authorization"))
		h.paths = append(h.paths, r.URL.Path)
		body, status := h.body, h.status
		h.mu.Unlock()
		if r.URL.Path != "/skills/qube-sync-audit/SKILL.md" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/markdown")
		if status != 0 {
			w.WriteHeader(status)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *skillHost) set(body string) {
	h.mu.Lock()
	h.body = body
	h.mu.Unlock()
}

// noLoginCtx is a ctx with no session at all for host, as on a machine that never ran qube login.
func noLoginCtx(host string) *ctx {
	return &ctx{host: host, cfg: &config.File{Sessions: map[string]config.Session{}}, bg: context.Background()}
}

// Written, then current (nothing written), then updated when the served skill changes; with no
// login, and no token sent.
func TestSkillsInstallWritesUpdatesAndKeepsCurrent(t *testing.T) {
	h := &skillHost{body: "---\nname: qube-sync-audit\n---\n# Audit v1\n"}
	c := noLoginCtx(serveSkills(t, h))
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", "skills", "qube-sync-audit", "SKILL.md")
	setJSON(t, false)

	out := captureStdout(t, func() { c.skills([]string{"install", "--dir", dir}) })
	if out != "Wrote "+path+" (new).\n" {
		t.Fatalf("first install: %q", out)
	}
	if data, _ := os.ReadFile(path); string(data) != h.body {
		t.Fatalf("file = %q", data)
	}

	info, _ := os.Stat(path)
	out = captureStdout(t, func() { c.skills([]string{"install", "qube-sync-audit", "--dir", dir}) })
	if out != path+" is already current; nothing written.\n" {
		t.Fatalf("second install: %q", out)
	}
	if again, _ := os.Stat(path); !again.ModTime().Equal(info.ModTime()) {
		t.Error("a current file was rewritten")
	}

	h.set("---\nname: qube-sync-audit\n---\n# Audit v2\n")
	setJSON(t, true)
	out = captureStdout(t, func() { c.skills([]string{"install", "--dir", dir}) })
	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["path"] != path || got["status"] != "updated" || len(got) != 2 {
		t.Fatalf("--json: %v %q", err, out)
	}
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), "Audit v2") {
		t.Fatalf("file not updated: %q", data)
	}

	for i, a := range h.auth {
		if a != "" {
			t.Errorf("request %d carried Authorization %q", i, a)
		}
	}
	if len(h.paths) != 3 || h.paths[0] != "/skills/qube-sync-audit/SKILL.md" {
		t.Errorf("paths = %v", h.paths)
	}
}

// DIR defaults to the current directory.
func TestSkillsInstallDefaultsToTheCurrentDirectory(t *testing.T) {
	h := &skillHost{body: "# Audit\n"}
	c := noLoginCtx(serveSkills(t, h))
	dir := t.TempDir()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	setJSON(t, true)
	out := captureStdout(t, func() { c.skills([]string{"install"}) })
	// The path is the platform's own: backslashes on Windows, escaped in JSON.
	want, _ := json.Marshal(filepath.Join(".claude", "skills", "qube-sync-audit", "SKILL.md"))
	if !strings.Contains(out, `"path": `+string(want)) || !strings.Contains(out, `"status": "written"`) {
		t.Fatalf("out = %q", out)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, ".claude", "skills", "qube-sync-audit", "SKILL.md")); string(data) != "# Audit\n" {
		t.Fatalf("file = %q", data)
	}
}

// A session holding a token and an act-as grant sends neither: the skills are public, and
// qube skills doesn't enter act-as.
func TestSkillsInstallSendsNoCredentials(t *testing.T) {
	h := &skillHost{body: "# Audit\n"}
	host := serveSkills(t, h)
	c := noLoginCtx(host)
	c.cfg.Sessions[host] = config.Session{Token: "qct_x", ActingAs: &config.ActAs{Token: "qct_grant", Organization: "Acme Corp"}}
	setJSON(t, true)
	_ = captureStdout(t, func() { c.skills([]string{"install", "--dir", t.TempDir()}) })
	if len(h.auth) != 1 || h.auth[0] != "" {
		t.Fatalf("Authorization = %q", h.auth)
	}
	if usesActAs([]string{"skills", "install"}) {
		t.Error("usesActAs(skills install) = true")
	}
}

// The refusals end the process, so each runs in a child against a config with no session.
func TestSkillsInstallRefusals(t *testing.T) {
	if host := os.Getenv("QUBE_TEST_SKILLS_HOST"); host != "" {
		cfg, err := config.Load()
		if err != nil || len(cfg.Sessions) != 0 {
			panic(fmt.Sprintf("the child should have no session: %v %v", err, cfg))
		}
		c := &ctx{host: host, cfg: cfg, bg: context.Background()}
		c.skills(strings.Split(os.Getenv("QUBE_TEST_SKILLS_ARGS"), " "))
		os.Exit(0)
	}
	h := &skillHost{body: "# Audit\n"}
	host := serveSkills(t, h)
	withConfig(t, noLoginCtx(host))
	runChild := func(t *testing.T, args string) (int, string) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestSkillsInstallRefusals$")
		cmd.Env = append(os.Environ(), "QUBE_TEST_SKILLS_HOST="+host, "QUBE_TEST_SKILLS_ARGS="+args)
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), string(out)
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0, string(out)
	}
	big := strings.Repeat("x", api.MaxSkillBytes+1)
	for _, tc := range []struct {
		name     string
		args     string
		body     string
		status   int
		wantCode int
		wantErr  string
		wantGET  bool
	}{
		{"an unknown skill", "install other-skill", "", 0, ui.ExitFailure, `has no skill named "other-skill" (GET /skills/other-skill/SKILL.md is a 404)`, true},
		{"a server error", "install", "oops", http.StatusInternalServerError, ui.ExitFailure, "answered HTTP 500", true},
		{"an oversize body", "install", big, 0, ui.ExitFailure, "more than 1 MB", true},
		{"a name with a slash", "install ../evil", "", 0, ui.ExitUsage, "lower-case letters, digits and dashes", false},
		{"an upper-case name", "install Qube", "", 0, ui.ExitUsage, "lower-case letters", false},
		{"an underscore", "install qube_sync", "", 0, ui.ExitUsage, "lower-case letters", false},
		{"two names", "install a b", "", 0, ui.ExitUsage, "usage: qube skills install", false},
		{"no subcommand", "list", "", 0, ui.ExitUsage, "usage: qube skills install", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.mu.Lock()
			h.body, h.status, h.paths = tc.body, tc.status, nil
			h.mu.Unlock()
			dir := t.TempDir()
			code, out := runChild(t, tc.args+" --dir "+dir)
			if code != tc.wantCode || !strings.Contains(out, tc.wantErr) {
				t.Errorf("exit %d (want %d):\n%s", code, tc.wantCode, out)
			}
			if got := len(h.paths) > 0; got != tc.wantGET {
				t.Errorf("requests: %v", h.paths)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("wrote %v", entries)
			}
		})
	}
}

func TestSkillsIsInCompletionAndHelp(t *testing.T) {
	if !contains(commandTree[""], "skills") || !contains(commandTree["skills"], "install") || !contains(commandTree["skills"], "--dir") {
		t.Errorf("completion: %v %v", commandTree[""], commandTree["skills"])
	}
	setenv(t, "QUBE_CONFIG", t.TempDir()+"/credentials.json")
	help := captureStdout(t, usage)
	if !strings.Contains(help, "skills install [NAME] [--dir DIR]") {
		t.Errorf("qube help lacks skills install:\n%s", help)
	}
}
