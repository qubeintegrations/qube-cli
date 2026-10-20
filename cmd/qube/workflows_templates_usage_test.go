package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// templateUsageServer is the v2 side of `workflows templates KEY --usage`: the template's
// contract as Markdown (/usage.md) and as JSON (/usage) for the keys it has, a 404 for any
// other, and a record of the v2 requests it saw.
type templateUsageServer struct {
	mu       sync.Mutex
	requests []string
}

func (s *templateUsageServer) saw() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

func (s *templateUsageServer) handlers(keys ...string) map[string]http.HandlerFunc {
	record := func(r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": map[string]interface{}{"detail": "Not Found"}})
	}
	h := map[string]http.HandlerFunc{}
	// the pattern ends in "/": the mux hands every key's routes to one function
	h["/api/v2/workflow-templates/"] = func(w http.ResponseWriter, r *http.Request) {
		record(r)
		rest := strings.TrimPrefix(r.URL.Path, "/api/v2/workflow-templates/")
		switch {
		case strings.HasSuffix(rest, "/usage.md") && have[strings.TrimSuffix(rest, "/usage.md")]:
			w.Header().Set("Content-Type", "text/markdown")
			_, _ = w.Write([]byte("# Using " + strings.TrimSuffix(rest, "/usage.md") + "\n\nAsks `create_missing / ask_rename`.\n"))
		case strings.HasSuffix(rest, "/usage") && have[strings.TrimSuffix(rest, "/usage")]:
			key := strings.TrimSuffix(rest, "/usage")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"workflow": map[string]interface{}{"key": key},
				"decisions": []map[string]interface{}{
					{"id": "create_missing / ask_rename", "options": []string{"rename", "keep"}},
				},
			}})
		case have[rest]:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"key": rest, "definition": map[string]interface{}{"states": map[string]interface{}{}}}})
		default:
			notFound(w)
		}
	}
	return h
}

// `workflows templates KEY --usage` prints the host's Markdown for the template's contract,
// from the template's own route, with no version asked for.
func TestWorkflowsTemplatesUsageMarkdown(t *testing.T) {
	for _, args := range [][]string{
		{"templates", "add_transaction", "--usage"},
		{"templates", "--usage", "add_transaction"},
	} {
		s := &templateUsageServer{}
		c := newAPITest(t, s.handlers("add_transaction"))
		setJSON(t, false)
		out := captureStdout(t, func() { c.workflows(args) })
		if want := "# Using add_transaction\n\nAsks `create_missing / ask_rename`.\n"; out != want {
			t.Fatalf("%v: output = %q, want %q", args, out, want)
		}
		if got, want := s.saw(), []string{"GET /api/v2/workflow-templates/add_transaction/usage.md"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("%v: requests = %v, want %v", args, got, want)
		}
	}
}

// With --json it prints the contract itself: the answer's "data", the shape `workflows usage
// KEY --json` prints.
func TestWorkflowsTemplatesUsageJSON(t *testing.T) {
	s := &templateUsageServer{}
	c := newAPITest(t, s.handlers("add_transaction"))
	setJSON(t, true)
	out := captureStdout(t, func() { c.workflows([]string{"templates", "add_transaction", "--usage"}) })
	var got struct {
		Workflow  map[string]interface{}   `json:"workflow"`
		Decisions []map[string]interface{} `json:"decisions"`
		Data      interface{}              `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if got.Data != nil || got.Workflow["key"] != "add_transaction" || len(got.Decisions) != 1 || got.Decisions[0]["id"] != "create_missing / ask_rename" {
		t.Fatalf("output is not the unwrapped contract:\n%s", out)
	}
	if got, want := s.saw(), []string{"GET /api/v2/workflow-templates/add_transaction/usage"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// A template the host doesn't have prints the server's message, not its JSON body, and exits 1,
// in either form.
func TestWorkflowsTemplatesUsageOfAnUnknownTemplate(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		s := &templateUsageServer{}
		c := newAPITest(t, s.handlers("add_transaction"))
		exit, stdout, stderr := workflowsInChild(t, c.host, asJSON, "templates", "no_such_template", "--usage")
		if exit != ui.ExitFailure || stdout != "" {
			t.Fatalf("json=%v: exit %d, stdout %q, stderr %q", asJSON, exit, stdout, stderr)
		}
		if !strings.Contains(stderr, "HTTP 404: Not Found") || strings.Contains(stderr, `{"errors"`) {
			t.Fatalf("json=%v: stderr = %q", asJSON, stderr)
		}
	}
}

// --usage is about one template: without a KEY it is a usage error (exit 2) before anything is
// sent.
func TestWorkflowsTemplatesUsageNeedsAKey(t *testing.T) {
	s := &templateUsageServer{}
	c := newAPITest(t, s.handlers("add_transaction"))
	exit, stdout, stderr := workflowsInChild(t, c.host, false, "templates", "--usage")
	if exit != ui.ExitUsage || stdout != "" || !strings.Contains(stderr, "usage: qube workflows templates KEY --usage") {
		t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
	if len(s.saw()) > 0 {
		t.Fatalf("requests = %v", s.saw())
	}
}

// Without --usage, `templates KEY` still prints the chart as before, with the flag anywhere.
func TestWorkflowsTemplatesWithoutUsageStillPrintsTheChart(t *testing.T) {
	s := &templateUsageServer{}
	c := newAPITest(t, s.handlers("add_transaction"))
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"templates", "add_transaction"}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["key"] != "add_transaction" || got["definition"] == nil {
		t.Fatalf("not the template: %v\n%s", err, out)
	}
	if got, want := s.saw(), []string{"GET /api/v2/workflow-templates/add_transaction"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
}

// `workflows usage KEY` prints through the same code: a key the host doesn't have prints the
// server's message too.
func TestWorkflowsUsageOfAnUnknownWorkflowPrintsTheMessage(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/workflows/nope/usage.md": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": map[string]interface{}{"detail": "Not Found"}})
		},
	})
	exit, stdout, stderr := workflowsInChild(t, c.host, false, "usage", "nope")
	if exit != ui.ExitFailure || stdout != "" || !strings.Contains(stderr, "HTTP 404: Not Found") || strings.Contains(stderr, `{"errors"`) {
		t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout, stderr)
	}
}

// Completion knows the flag.
func TestCompletionKnowsWorkflowsTemplatesUsage(t *testing.T) {
	if !contains(commandTree["workflows"], "--usage") {
		t.Fatal("completion's commandTree[workflows] lacks --usage")
	}
}
