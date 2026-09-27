package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- connections

func TestConnectionsListIsV2(t *testing.T) {
	var gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]interface{}{}})
		},
	})
	setJSON(t, false)
	captureStdout(t, func() { c.connections([]string{"list"}) })
	if gotPath != "/api/v2/connections" {
		t.Fatalf("path = %s", gotPath)
	}
}

func TestConnectionsShow(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id": "conn_1", "name": "Acme", "type": "web_connector",
					"links": map[string]interface{}{"ui": "https://dash/conn_1", "onboarding": "https://onboard/conn_1"},
				},
			})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.connections([]string{"show", "conn_1"}) })
	if gotMethod != "GET" || gotPath != "/api/v2/connections/conn_1" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "conn_1") || !strings.Contains(out, "Acme") || !strings.Contains(out, "dashboard") {
		t.Fatalf("output = %q", out)
	}
}

func TestConnectionsUpdateSendsFlatBody(t *testing.T) {
	var gotMethod string
	var gotBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1": func(w http.ResponseWriter, r *http.Request) {
			gotMethod = r.Method
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"id": "conn_1", "name": "X"},
			})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.connections([]string{"update", "conn_1", "--name", "X"}) })
	if gotMethod != "PUT" {
		t.Fatalf("method = %s", gotMethod)
	}
	if len(gotBody) != 1 || gotBody["name"] != "X" {
		t.Fatalf("body = %v", gotBody)
	}
	if !strings.Contains(out, "Updated conn_1 (X)") {
		t.Fatalf("output = %q", out)
	}
}

func TestConnectionsDeleteHitsEndpoints(t *testing.T) {
	var methods []string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1": func(w http.ResponseWriter, r *http.Request) {
			methods = append(methods, r.Method)
			switch r.Method {
			case "GET":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{"id": "conn_1", "name": "Acme"},
				})
			case "DELETE":
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.connections([]string{"delete", "conn_1", "--yes"}) })
	if len(methods) != 2 || methods[0] != "GET" || methods[1] != "DELETE" {
		t.Fatalf("methods = %v", methods)
	}
	if !strings.Contains(out, "Removed connection conn_1.") {
		t.Fatalf("output = %q", out)
	}
}

func TestConnectionsQWCWritesFile(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/qwc": func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "POST" {
				t.Fatalf("method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"qwc": "<QBWCXML>hello</QBWCXML>"})
		},
	})
	setJSON(t, false)
	tmp := filepath.Join(t.TempDir(), "acme.qwc")
	out := captureStdout(t, func() { c.connections([]string{"qwc", "conn_1", "--output", tmp}) })
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "<QBWCXML>hello</QBWCXML>" {
		t.Fatalf("file contents = %q", data)
	}
	if !strings.Contains(out, "Wrote "+tmp) {
		t.Fatalf("output = %q", out)
	}
}

func TestConnectionsOnboardingURL(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/onboarding_url": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{
					"id":                          "conn_1",
					"onboarding_token_expires_at": "2026-01-01T00:00:00Z",
					"links":                       map[string]interface{}{"onboarding": "https://onboard/xyz"},
				},
			})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.connections([]string{"onboarding-url", "conn_1"}) })
	if gotMethod != "POST" || gotPath != "/api/v2/connections/conn_1/onboarding_url" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "https://onboard/xyz") || !strings.Contains(out, "2026-01-01T00:00:00Z") {
		t.Fatalf("output = %q", out)
	}
}

// TestConnectionsPasswordStdin drives the --stdin branch by swapping os.Stdin for a pipe.
func TestConnectionsPasswordStdin(t *testing.T) {
	var gotBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/password": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"id": "conn_1", "password": "secret"},
			})
		},
	})
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	go func() {
		_, _ = w.Write([]byte("hunter2\n"))
		_ = w.Close()
	}()
	setJSON(t, false)
	out := captureStdout(t, func() { c.connections([]string{"password", "conn_1", "--stdin"}) })
	if gotBody["password"] != "hunter2" {
		t.Fatalf("body = %v", gotBody)
	}
	if !strings.Contains(out, "Web Connector password for conn_1: secret") {
		t.Fatalf("output = %q", out)
	}
}

// ---------------------------------------------------------------- requests

func TestRequestsListQuery(t *testing.T) {
	var gotQuery url.Values
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/queued_requests": func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]interface{}{}})
		},
	})
	setJSON(t, false)
	captureStdout(t, func() {
		c.requests([]string{"list", "conn_1", "--state", "error", "--search", "acme", "--page-size", "5"})
	})
	if gotQuery.Get("state") != "error" || gotQuery.Get("search_phrase") != "acme" || gotQuery.Get("page_size") != "5" {
		t.Fatalf("query = %v", gotQuery)
	}
}

func TestRequestsShow(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/queued_requests/req_1": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "req_1"}})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.requests([]string{"show", "conn_1", "req_1"}) })
	if gotMethod != "GET" || gotPath != "/api/v2/connections/conn_1/queued_requests/req_1" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, `"req_1"`) {
		t.Fatalf("output = %q", out)
	}
}

func TestRequestsPages(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/queued_requests/req_1/pages": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{map[string]interface{}{"page": float64(1)}}})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.requests([]string{"pages", "conn_1", "req_1"}) })
	if gotMethod != "GET" || gotPath != "/api/v2/connections/conn_1/queued_requests/req_1/pages" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, `"page"`) {
		t.Fatalf("output = %q", out)
	}
}

// ---------------------------------------------------------------- simulator

func TestSimulatorShowResetSync(t *testing.T) {
	var methods, paths []string
	record := func(w http.ResponseWriter, r *http.Request, body map[string]interface{}) {
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.Path)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": body})
	}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/simulator": func(w http.ResponseWriter, r *http.Request) {
			record(w, r, map[string]interface{}{"company": map[string]interface{}{}, "faults": map[string]interface{}{}})
		},
		"/api/v2/connections/conn_1/simulator/reset": func(w http.ResponseWriter, r *http.Request) {
			record(w, r, map[string]interface{}{"company": map[string]interface{}{}, "faults": map[string]interface{}{}})
		},
		"/api/v2/connections/conn_1/simulator/sync": func(w http.ResponseWriter, r *http.Request) {
			record(w, r, map[string]interface{}{"result": "nothing_to_do"})
		},
	})
	setJSON(t, false)
	captureStdout(t, func() { c.simulator([]string{"show", "conn_1"}) })
	captureStdout(t, func() { c.simulator([]string{"reset", "conn_1"}) })
	captureStdout(t, func() { c.simulator([]string{"sync", "conn_1"}) })

	want := []struct{ method, path string }{
		{"GET", "/api/v2/connections/conn_1/simulator"},
		{"POST", "/api/v2/connections/conn_1/simulator/reset"},
		{"POST", "/api/v2/connections/conn_1/simulator/sync"},
	}
	for i, w := range want {
		if methods[i] != w.method || paths[i] != w.path {
			t.Fatalf("call %d: got %s %s, want %s %s", i, methods[i], paths[i], w.method, w.path)
		}
	}
}

func TestSimulatorFaults(t *testing.T) {
	var calls []string
	var putBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/simulator": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method)
			switch r.Method {
			case "GET":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{"faults": map[string]interface{}{}},
				})
			case "PUT":
				_ = json.NewDecoder(r.Body).Decode(&putBody)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"data": map[string]interface{}{"faults": map[string]interface{}{"connection_error": "qb_closed"}},
				})
			default:
				t.Fatalf("unexpected method %s", r.Method)
			}
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.simulator([]string{"faults", "conn_1", "--qb", "closed"}) })
	if len(calls) != 2 || calls[0] != "GET" || calls[1] != "PUT" {
		t.Fatalf("calls = %v", calls)
	}
	faults, _ := putBody["faults"].(map[string]interface{})
	if faults["connection_error"] != "qb_closed" {
		t.Fatalf("PUT body = %v", putBody)
	}
	if !strings.Contains(out, "Faults now:") {
		t.Fatalf("output = %q", out)
	}
}

// ---------------------------------------------------------------- workflows

func TestWorkflowsValidateValidChart(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflows/validate": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"valid": true, "errors": []interface{}{}, "warnings": []interface{}{}})
		},
	})
	tmp := filepath.Join(t.TempDir(), "chart.json")
	if err := os.WriteFile(tmp, []byte(`{"key":"k1","initial":"a","states":{"a":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"validate", tmp}) })
	if gotMethod != "POST" || gotPath != "/api/v2/workflows/validate" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	def, _ := gotBody["definition"].(map[string]interface{})
	if def["key"] != "k1" {
		t.Fatalf("body = %v", gotBody)
	}
	if !strings.Contains(out, "is valid.") {
		t.Fatalf("output = %q", out)
	}
}

func TestWorkflowsPublish(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflows/k1/publish": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"key": "k1", "published_version": float64(3)},
			})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"publish", "k1", "--notes", "n"}) })
	if gotMethod != "POST" || gotPath != "/api/v2/workflows/k1/publish" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if gotBody["notes"] != "n" {
		t.Fatalf("body = %v", gotBody)
	}
	if !strings.Contains(out, "Published k1 as version 3") {
		t.Fatalf("output = %q", out)
	}
}

func TestWorkflowsInstall(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflow_templates/tmpl1/install": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data":      map[string]interface{}{"key": "k2", "state": "draft"},
				"installed": []string{"k2", "k2_child"},
			})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"install", "tmpl1", "--publish", "--as", "k2"}) })
	if gotMethod != "POST" || gotPath != "/api/v2/workflow_templates/tmpl1/install" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if gotBody["publish"] != true || gotBody["as"] != "k2" {
		t.Fatalf("body = %v", gotBody)
	}
	if !strings.Contains(out, "Installed tmpl1 as k2 (draft).") || !strings.Contains(out, "Also created: k2_child") {
		t.Fatalf("output = %q", out)
	}
}

func TestWorkflowsRunsEventsQuery(t *testing.T) {
	var gotMethod, gotPath string
	var gotQuery url.Values
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow_runs/run_1/events": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			gotQuery = r.URL.Query()
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}, "next_after": nil})
		},
	})
	setJSON(t, false)
	captureStdout(t, func() {
		c.workflows([]string{"runs", "conn_1", "run_1", "--events", "--after", "7.3", "--limit", "50"})
	})
	if gotMethod != "GET" || gotPath != "/api/v2/connections/conn_1/workflow_runs/run_1/events" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if gotQuery.Get("after") != "7.3" || gotQuery.Get("limit") != "50" {
		t.Fatalf("query = %v", gotQuery)
	}
}

func TestWorkflowsCancel(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow_runs/run_1/cancel": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "run_1", "state": "cancelled"}})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"cancel", "conn_1", "run_1", "--reason", "r"}) })
	if gotMethod != "POST" || gotPath != "/api/v2/connections/conn_1/workflow_runs/run_1/cancel" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if gotBody["reason"] != "r" {
		t.Fatalf("body = %v", gotBody)
	}
	if !strings.Contains(out, "Cancelled run run_1 (cancelled).") {
		t.Fatalf("output = %q", out)
	}
}

func TestWorkflowsDeleteRun(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow_runs/run_1": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"delete-run", "conn_1", "run_1", "--yes"}) })
	if gotMethod != "DELETE" || gotPath != "/api/v2/connections/conn_1/workflow_runs/run_1" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "Deleted run run_1.") {
		t.Fatalf("output = %q", out)
	}
}

func TestWorkflowsUsageMarkdown(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflows/k1/usage.md": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# Usage\n\nDoes a thing.\n"))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"usage", "k1"}) })
	if gotMethod != "GET" || gotPath != "/api/v2/workflows/k1/usage.md" {
		t.Fatalf("method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, "# Usage") || !strings.Contains(out, "Does a thing.") {
		t.Fatalf("output = %q", out)
	}
}

func TestRawPathDefaultsToV2(t *testing.T) {
	for in, want := range map[string]string{
		"/connections":             "/api/v2/connections",
		"connections/c1/customers": "/api/v2/connections/c1/customers",
		"/api/v2/workflows":        "/api/v2/workflows",
		"/api/v1/connections":      "/api/v1/connections",
	} {
		if got := rawPath(in); got != want {
			t.Errorf("rawPath(%q) = %q, want %q", in, got, want)
		}
	}
}
