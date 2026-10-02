package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// withConfig points QUBE_CONFIG at a temp file holding c's config, so `use` can save.
func withConfig(t *testing.T, c *ctx) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "credentials.json")
	setenv(t, "QUBE_CONFIG", p)
	if err := c.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	return p
}

func connectionsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]interface{}{
			{"id": "conn_1", "name": "Riverbend"},
			{"id": "conn_2", "name": "Northwind"},
		}})
	case "POST":
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "conn_3", "name": "Local dev", "type": "simulated"}})
	}
}

func TestAPICallsCarryTheSessionAndApp(t *testing.T) {
	var auth, app string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) {
			auth, app = r.Header.Get("Authorization"), r.Header.Get("X-Qube-App")
			connectionsHandler(w, r)
		},
	})
	setJSON(t, true)
	_ = captureStdout(t, func() { c.connections([]string{"list"}) })
	if auth != "Bearer qct_x" || app != "app1" {
		t.Fatalf("auth = %q, app = %q", auth, app)
	}
}

func TestUseConnectionByNameThenCommandsUseIt(t *testing.T) {
	var got string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections": connectionsHandler,
		"/api/v2/connections/conn_2/queued_requests": func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
		},
		"/api/v2/connections/conn_2/queued_requests/req_1": func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "req_1"}})
		},
	})
	path := withConfig(t, c)
	setJSON(t, true)

	c.connection = "north" // a unique name prefix
	c.use(nil)
	saved, _ := os.ReadFile(path)
	if !strings.Contains(string(saved), `"conn_2"`) {
		t.Fatalf("config = %s", saved)
	}

	c.connection = ""
	_ = captureStdout(t, func() { c.requests([]string{"list"}) })
	if got != "/api/v2/connections/conn_2/queued_requests" {
		t.Fatalf("requests list went to %q", got)
	}
	_ = captureStdout(t, func() { c.requests([]string{"show", "req_1"}) })
	if got != "/api/v2/connections/conn_2/queued_requests/req_1" {
		t.Fatalf("requests show went to %q", got)
	}
	// a connection on the line still wins
	_ = captureStdout(t, func() { c.requests([]string{"list", "conn_2"}) })
	if got != "/api/v2/connections/conn_2/queued_requests" {
		t.Fatalf("requests list conn_2 went to %q", got)
	}
}

func TestResolveConnection(t *testing.T) {
	list := []map[string]interface{}{{"id": "c1", "name": "Riverbend"}, {"id": "c2", "name": "River Oaks"}, {"id": "c3", "name": "Northwind"}}
	for ref, want := range map[string]string{"c1": "c1", "northwind": "c3", "North": "c3", "river oaks": "c2"} {
		if cn, err := resolveConnection(list, ref); err != nil || cn.ID != want {
			t.Errorf("%q: %v %v", ref, cn, err)
		}
	}
	for _, ref := range []string{"River", "nope"} {
		if _, err := resolveConnection(list, ref); err == nil {
			t.Errorf("%q should not resolve", ref)
		}
	}
}

func TestCreateWithUseAndDeleteForgetsTheDefault(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections": connectionsHandler,
		"/api/v2/connections/conn_3": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "DELETE" {
				w.WriteHeader(204)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "conn_3", "name": "Local dev"}})
		},
	})
	path := withConfig(t, c)
	setJSON(t, true)
	c.connections2(t, "create", "--simulated", "--use")
	if saved, _ := os.ReadFile(path); !strings.Contains(string(saved), `"conn_3"`) {
		t.Fatalf("--use didn't save it: %s", saved)
	}
	c.yes = true
	c.connections2(t, "delete", "conn_3")
	if saved, _ := os.ReadFile(path); strings.Contains(string(saved), `"conn_3"`) {
		t.Fatalf("delete kept it as the default: %s", saved)
	}
}

// connections2 runs `connections` with the list resolving to conn_3 as well.
func (c *ctx) connections2(t *testing.T, args ...string) {
	t.Helper()
	_ = captureStdout(t, func() { c.connections(args) })
}

func TestWorkflowRunsTakesTheRunWhenAConnectionIsChosen(t *testing.T) {
	var got string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow_runs/run_1": func(w http.ResponseWriter, r *http.Request) {
			got = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "run_1"}})
		},
	})
	setJSON(t, true)
	c.connection = "conn_1"
	_ = captureStdout(t, func() { c.workflows([]string{"runs", "run_1"}) })
	if got != "/api/v2/connections/conn_1/workflow_runs/run_1" {
		t.Fatalf("went to %q", got)
	}
}

// A read-only session refuses a write before sending it (exit 1), and `env` too; reads go.
func TestReadOnlySessionRefusesWrites(t *testing.T) {
	if host := os.Getenv("QUBE_TEST_READONLY_HOST"); host != "" {
		c := &ctx{
			host: host,
			cfg:  &config.File{Sessions: map[string]config.Session{host: {Token: "qct_x", Access: "read_only"}}},
			bg:   context.Background(),
			yes:  true,
		}
		ui.JSON = true
		args := strings.Split(os.Getenv("QUBE_TEST_READONLY_ARGS"), " ")
		switch args[0] {
		case "connections":
			c.connections(args[1:])
		case "env":
			c.env(args[1:])
		case "qb":
			c.qb(args[1:])
		}
		os.Exit(0)
	}
	for _, args := range []string{
		"connections update conn_1 --name Riverbend",
		"connections delete conn_1",
		"env --write .env",
		"qb customers create conn_1 --name Northwind",
	} {
		var sent []string
		c := newAPITest(t, map[string]http.HandlerFunc{
			"/api/v2/": func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					sent = append(sent, r.Method+" "+r.URL.Path)
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "conn_1", "name": "Riverbend"}})
			},
			"/api/v2/openapi.json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(qbSpec)) },
			"/api/cli/apps/app1/credentials": func(w http.ResponseWriter, r *http.Request) {
				sent = append(sent, "credentials")
			},
		})
		cmd := exec.Command(os.Args[0], "-test.run=^TestReadOnlySessionRefusesWrites$")
		cmd.Env = append(os.Environ(), "QUBE_TEST_READONLY_HOST="+c.host, "QUBE_TEST_READONLY_ARGS="+args, "QUBE_CACHE_DIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != ui.ExitFailure {
			t.Errorf("%s: want exit %d, got %v\n%s", args, ui.ExitFailure, err, out)
		}
		if !strings.Contains(string(out), "this session is read-only") {
			t.Errorf("%s: output = %s", args, out)
		}
		if len(sent) > 0 {
			t.Errorf("%s: sent %v", args, sent)
		}
	}
}

func TestReadOnlySessionReads(t *testing.T) {
	var got string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) {
			got = r.Method
			connectionsHandler(w, r)
		},
	})
	s := c.cfg.Sessions[c.host]
	s.Access = "read_only"
	c.cfg.Sessions[c.host] = s
	setJSON(t, true)
	_ = captureStdout(t, func() { c.connections([]string{"list"}) })
	if got != "GET" {
		t.Fatalf("a read-only session should still read; server saw %q", got)
	}
}
