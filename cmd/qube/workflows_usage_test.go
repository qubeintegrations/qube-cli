package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// usageServer is the v2 side of `workflows usage --all`: the list of workflows (keys, in
// order), each key's usage in Markdown (/usage.md) and as JSON (/usage), and a record of the
// v2 requests it saw. A key in failing answers both of its usage routes with a 404.
type usageServer struct {
	mu       sync.Mutex
	requests []string
}

func (s *usageServer) saw() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *usageServer) handlers(keys []string, failing map[string]bool) map[string]http.HandlerFunc {
	record := func(r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	}
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": map[string]interface{}{"detail": "Workflow not found"}})
	}
	h := map[string]http.HandlerFunc{
		"/api/v2/workflows": func(w http.ResponseWriter, r *http.Request) {
			record(r)
			list := []map[string]interface{}{}
			for _, k := range keys {
				list = append(list, map[string]interface{}{"key": k, "name": strings.ToUpper(k), "state": "published"})
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": list})
		},
	}
	for _, k := range keys {
		k := k
		h["/api/v2/workflows/"+k+"/usage.md"] = func(w http.ResponseWriter, r *http.Request) {
			record(r)
			if failing[k] {
				notFound(w)
				return
			}
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# Using " + k + "\n\nDoes " + k + ".\n"))
		}
		h["/api/v2/workflows/"+k+"/usage"] = func(w http.ResponseWriter, r *http.Request) {
			record(r)
			if failing[k] {
				notFound(w)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"workflow": map[string]interface{}{"key": k}}})
		}
	}
	return h
}

// `workflows usage --all` lists the workflows, then prints each one's Markdown in list order
// with a --- line between the documents (and none around them).
func TestWorkflowsUsageAllMarkdown(t *testing.T) {
	s := &usageServer{}
	c := newAPITest(t, s.handlers([]string{"alpha", "beta", "gamma"}, nil))
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"usage", "--all"}) })
	want := "# Using alpha\n\nDoes alpha.\n" +
		"\n---\n\n" +
		"# Using beta\n\nDoes beta.\n" +
		"\n---\n\n" +
		"# Using gamma\n\nDoes gamma.\n"
	if out != want {
		t.Fatalf("output:\n%q\nwant:\n%q", out, want)
	}
	wantRequests := []string{
		"GET /api/v2/workflows",
		"GET /api/v2/workflows/alpha/usage.md",
		"GET /api/v2/workflows/beta/usage.md",
		"GET /api/v2/workflows/gamma/usage.md",
	}
	if got := s.saw(); !reflect.DeepEqual(got, wantRequests) {
		t.Fatalf("requests = %v\nwant %v", got, wantRequests)
	}
}

// A document that doesn't end in a newline still gets a separator line of its own.
func TestWorkflowsUsageAllMarkdownWithoutTrailingNewline(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflows": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []map[string]interface{}{{"key": "a"}, {"key": "b"}}})
		},
		"/api/v2/workflows/a/usage.md": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("# A")) },
		"/api/v2/workflows/b/usage.md": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("# B")) },
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"usage", "--all"}) })
	if want := "# A\n\n---\n\n# B\n"; out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
}

// With --json, --all prints one array of {key, usage}, each usage that workflow's JSON.
func TestWorkflowsUsageAllJSON(t *testing.T) {
	s := &usageServer{}
	c := newAPITest(t, s.handlers([]string{"alpha", "beta"}, nil))
	setJSON(t, true)
	out := captureStdout(t, func() { c.workflows([]string{"usage", "--all"}) })
	var got []struct {
		Key   string                 `json:"key"`
		Usage map[string]interface{} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not one JSON array: %v\n%s", err, out)
	}
	if len(got) != 2 || got[0].Key != "alpha" || got[1].Key != "beta" {
		t.Fatalf("got %+v", got)
	}
	for _, d := range got {
		workflow, _ := d.Usage["workflow"].(map[string]interface{})
		if workflow["key"] != d.Key {
			t.Fatalf("%s's usage = %v", d.Key, d.Usage)
		}
	}
	wantRequests := []string{"GET /api/v2/workflows", "GET /api/v2/workflows/alpha/usage", "GET /api/v2/workflows/beta/usage"}
	if seen := s.saw(); !reflect.DeepEqual(seen, wantRequests) {
		t.Fatalf("requests = %v\nwant %v", seen, wantRequests)
	}
}

// An app with no workflows prints nothing in Markdown and [] under --json, and exits 0.
func TestWorkflowsUsageAllWithNoWorkflows(t *testing.T) {
	s := &usageServer{}
	c := newAPITest(t, s.handlers(nil, nil))
	setJSON(t, false)
	if out := captureStdout(t, func() { c.workflows([]string{"usage", "--all"}) }); out != "" {
		t.Fatalf("Markdown output = %q", out)
	}
	setJSON(t, true)
	out := captureStdout(t, func() { c.workflows([]string{"usage", "--all"}) })
	if strings.TrimSpace(out) != "[]" {
		t.Fatalf("JSON output = %q", out)
	}
}

// KEY and --version each contradict --all: a usage error (exit 2) before anything is sent.
func TestWorkflowsUsageAllRefusesAKeyOrAVersion(t *testing.T) {
	cases := [][]string{
		{"usage", "alpha", "--all"},
		{"usage", "--all", "alpha"},
		{"usage", "--all", "--version", "2"},
		{"usage", "--version=2", "--all"},
		{"usage", "alpha", "--version", "2", "--all"},
		{"usage", "--all", "--version", ""},
	}
	for _, args := range cases {
		s := &usageServer{}
		c := newAPITest(t, s.handlers([]string{"alpha"}, nil))
		exit, stdout, stderr := workflowsInChild(t, c.host, false, args...)
		if exit != ui.ExitUsage {
			t.Errorf("%v: want exit %d, got %d\nstdout: %s\nstderr: %s", args, ui.ExitUsage, exit, stdout, stderr)
		}
		if !strings.Contains(stderr, "--all takes no KEY and no --version") || !strings.Contains(stderr, "usage: qube workflows usage KEY [--version V] | --all") {
			t.Errorf("%v: stderr = %q", args, stderr)
		}
		if stdout != "" || len(s.saw()) > 0 {
			t.Errorf("%v: nothing should be printed or sent; stdout %q, requests %v", args, stdout, s.saw())
		}
	}
}

// Without --all, a missing KEY shows the usage line with the new form.
func TestWorkflowsUsageWithoutAKeyShowsTheUsageLine(t *testing.T) {
	c := newAPITest(t, nil)
	exit, _, stderr := workflowsInChild(t, c.host, false, "usage")
	if exit != ui.ExitUsage || !strings.Contains(stderr, "usage: qube workflows usage KEY [--version V] | --all") {
		t.Fatalf("exit %d, stderr = %q", exit, stderr)
	}
}

// One workflow's usage failing leaves the others printed, names the failed one on stderr and
// exits 1.
func TestWorkflowsUsageAllWithOneFailing(t *testing.T) {
	s := &usageServer{}
	c := newAPITest(t, s.handlers([]string{"alpha", "beta", "gamma"}, map[string]bool{"beta": true}))
	exit, stdout, stderr := workflowsInChild(t, c.host, false, "usage", "--all")
	if exit != ui.ExitFailure {
		t.Fatalf("want exit %d, got %d\nstdout: %s\nstderr: %s", ui.ExitFailure, exit, stdout, stderr)
	}
	want := "# Using alpha\n\nDoes alpha.\n\n---\n\n# Using gamma\n\nDoes gamma.\n"
	if stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	for _, wantErr := range []string{"1 of 3 workflows", "beta: HTTP 404: Workflow not found"} {
		if !strings.Contains(stderr, wantErr) {
			t.Fatalf("stderr lacks %q:\n%s", wantErr, stderr)
		}
	}
	if strings.Contains(stderr, "alpha") || strings.Contains(stderr, "gamma") {
		t.Fatalf("stderr names a workflow that worked:\n%s", stderr)
	}
}

// The same under --json: the array holds the workflows that worked.
func TestWorkflowsUsageAllWithOneFailingAsJSON(t *testing.T) {
	s := &usageServer{}
	c := newAPITest(t, s.handlers([]string{"alpha", "beta", "gamma"}, map[string]bool{"alpha": true}))
	exit, stdout, stderr := workflowsInChild(t, c.host, true, "usage", "--all")
	if exit != ui.ExitFailure {
		t.Fatalf("want exit %d, got %d\nstdout: %s\nstderr: %s", ui.ExitFailure, exit, stdout, stderr)
	}
	var got []struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not one JSON array: %v\n%s", err, stdout)
	}
	if len(got) != 2 || got[0].Key != "beta" || got[1].Key != "gamma" {
		t.Fatalf("got %+v", got)
	}
	if !strings.Contains(stderr, "alpha: HTTP 404") || !strings.Contains(stderr, "1 of 3 workflows") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// Every usage failing prints nothing on stdout (Markdown) and still exits 1, naming each.
func TestWorkflowsUsageAllWithEveryOneFailing(t *testing.T) {
	s := &usageServer{}
	c := newAPITest(t, s.handlers([]string{"alpha", "beta"}, map[string]bool{"alpha": true, "beta": true}))
	exit, stdout, stderr := workflowsInChild(t, c.host, false, "usage", "--all")
	if exit != ui.ExitFailure || stdout != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
	if !strings.Contains(stderr, "2 of 2 workflows") || !strings.Contains(stderr, "alpha: HTTP 404") || !strings.Contains(stderr, "beta: HTTP 404") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// If the list fails, the command fails as `workflows list` does (exit 1, the error on stderr)
// and fetches no usage.
func TestWorkflowsUsageAllWhenTheListFails(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflows": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			sent = append(sent, r.URL.Path)
			mu.Unlock()
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": map[string]interface{}{"detail": "Not allowed"}})
		},
		"/api/v2/workflows/": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			sent = append(sent, r.URL.Path)
			mu.Unlock()
		},
	})
	exitAll, stdoutAll, stderrAll := workflowsInChild(t, c.host, false, "usage", "--all")
	exitList, _, stderrList := workflowsInChild(t, c.host, false, "list")
	if exitAll != ui.ExitFailure || exitList != ui.ExitFailure {
		t.Fatalf("exit codes: usage --all %d, list %d", exitAll, exitList)
	}
	if stdoutAll != "" || stderrAll != stderrList || !strings.Contains(stderrAll, "HTTP 403: Not allowed") {
		t.Fatalf("usage --all: stdout %q, stderr %q\nlist: stderr %q", stdoutAll, stderrAll, stderrList)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, p := range sent {
		if p != "/api/v2/workflows" {
			t.Fatalf("a usage was fetched: %v", sent)
		}
	}
}

// workflowsInChild runs `qube workflows args...` in a child of this test binary pointed at
// host (with --json when asJSON), as a refusal or failure exits the process, and returns its
// exit code, stdout and stderr.
func workflowsInChild(t *testing.T, host string, asJSON bool, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestWorkflowsChild$")
	flagJSON := ""
	if asJSON {
		flagJSON = "1"
	}
	cmd.Env = append(os.Environ(), "QUBE_TEST_WORKFLOWS_HOST="+host, "QUBE_TEST_WORKFLOWS_JSON="+flagJSON,
		"QUBE_TEST_WORKFLOWS_ARGS="+strings.Join(args, "\x1f"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, stdout.String(), stderr.String()
}

// TestWorkflowsChild is workflowsInChild's child; on its own it does nothing.
func TestWorkflowsChild(t *testing.T) {
	host := os.Getenv("QUBE_TEST_WORKFLOWS_HOST")
	if host == "" {
		t.Skip("run by workflowsInChild")
	}
	ui.JSON = os.Getenv("QUBE_TEST_WORKFLOWS_JSON") == "1"
	c := &ctx{host: host, cfg: &config.File{Sessions: map[string]config.Session{host: {Token: "qct_x"}}}, bg: context.Background()}
	c.workflows(strings.Split(os.Getenv("QUBE_TEST_WORKFLOWS_ARGS"), "\x1f"))
	os.Exit(0)
}
