package main

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// connectionWrites records the bodies `connections create` and `update` send, as decoded JSON
// (so a null stays a key whose value is nil).
type connectionWrites struct {
	mu     sync.Mutex
	method []string
	bodies []map[string]interface{}
}

func (w *connectionWrites) record(r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	var body map[string]interface{}
	_ = json.Unmarshal(data, &body)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.method = append(w.method, r.Method+" "+r.URL.Path)
	w.bodies = append(w.bodies, body)
}

func (w *connectionWrites) handlers() map[string]http.HandlerFunc {
	answer := func(rw http.ResponseWriter, r *http.Request) {
		w.record(r)
		_ = json.NewEncoder(rw).Encode(map[string]interface{}{"data": map[string]interface{}{
			"id": "conn_1", "name": "Acme", "type": "realtime",
			"links": map[string]interface{}{"onboarding": "https://qubesync.example/onboarding/x"},
		}})
	}
	return map[string]http.HandlerFunc{"/api/v2/connections": answer, "/api/v2/connections/conn_1": answer}
}

// `connections create --webhook-url` sends it as webhook_url, with the name, and never a
// redirect_url; left out, no webhook_url is sent.
func TestConnectionsCreateSendsTheWebhookURL(t *testing.T) {
	w := &connectionWrites{}
	c := newAPITest(t, w.handlers())
	setJSON(t, false)
	out := captureStdout(t, func() {
		c.connections([]string{"create", "--name", "Acme", "--webhook-url", "https://myapp.example/webhooks/qube/connection/42"})
	})
	if !strings.Contains(out, "Send the customer to onboarding: https://qubesync.example/onboarding/x") {
		t.Fatalf("output = %q", out)
	}
	want := map[string]interface{}{"name": "Acme", "webhook_url": "https://myapp.example/webhooks/qube/connection/42"}
	if !reflect.DeepEqual(w.bodies, []map[string]interface{}{want}) || !reflect.DeepEqual(w.method, []string{"POST /api/v2/connections"}) {
		t.Fatalf("sent %v %v, want %v", w.method, w.bodies, want)
	}

	w = &connectionWrites{}
	c = newAPITest(t, w.handlers())
	_ = captureStdout(t, func() { c.connections([]string{"create", "--name", "Acme"}) })
	if want := []map[string]interface{}{{"name": "Acme"}}; !reflect.DeepEqual(w.bodies, want) {
		t.Fatalf("without the flag, sent %v, want %v", w.bodies, want)
	}
}

// `connections update --webhook-url URL` sets it; an empty value sends null, which stops the
// events; the flag can follow the connection or come before it and can go with --name.
func TestConnectionsUpdateSetsOrClearsTheWebhookURL(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want map[string]interface{}
	}{
		{"set", []string{"update", "conn_1", "--webhook-url", "https://myapp.example/hook"}, map[string]interface{}{"webhook_url": "https://myapp.example/hook"}},
		{"set, flag first", []string{"update", "--webhook-url=https://myapp.example/hook", "conn_1"}, map[string]interface{}{"webhook_url": "https://myapp.example/hook"}},
		{"clear", []string{"update", "conn_1", "--webhook-url", ""}, map[string]interface{}{"webhook_url": nil}},
		{"clear with =", []string{"update", "conn_1", "--webhook-url="}, map[string]interface{}{"webhook_url": nil}},
		{"name and clear", []string{"update", "conn_1", "--name", "Riverbend", "--webhook-url", ""}, map[string]interface{}{"name": "Riverbend", "webhook_url": nil}},
		{"name only", []string{"update", "conn_1", "--name", "Riverbend"}, map[string]interface{}{"name": "Riverbend"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &connectionWrites{}
			c := newAPITest(t, w.handlers())
			setJSON(t, false)
			out := captureStdout(t, func() { c.connections(tc.args) })
			if !strings.Contains(out, "Updated conn_1") {
				t.Fatalf("output = %q", out)
			}
			if !reflect.DeepEqual(w.bodies, []map[string]interface{}{tc.want}) || !reflect.DeepEqual(w.method, []string{"PUT /api/v2/connections/conn_1"}) {
				t.Fatalf("sent %v %v, want %v", w.method, w.bodies, tc.want)
			}
		})
	}
}

// With nothing to change, update is a usage error naming the flags it takes (exit 2) and sends
// nothing.
func TestConnectionsUpdateWithNothingToChange(t *testing.T) {
	if inChild(t, func(c *ctx) { c.connections([]string{"update", "conn_1"}) }) {
		return
	}
	w := &connectionWrites{}
	c := staffCtx(t, true, w.handlers())
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, "usage: qube connections update <connection> [--name N] [--webhook-url U]") || len(w.bodies) > 0 {
		t.Fatalf("exit %d, sent %v:\n%s", code, w.bodies, out)
	}
}

// --redirect-url is gone from create: v2 refuses the field, so the flag is an unknown one
// (exit 2) and nothing is sent.
func TestConnectionsCreateNoLongerTakesRedirectURL(t *testing.T) {
	if inChild(t, func(c *ctx) { c.connections([]string{"create", "--redirect-url", "https://myapp.example/cb"}) }) {
		return
	}
	w := &connectionWrites{}
	c := staffCtx(t, true, w.handlers())
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, "flag provided but not defined: -redirect-url") || len(w.bodies) > 0 {
		t.Fatalf("exit %d, sent %v:\n%s", code, w.bodies, out)
	}
}

// The same for update.
func TestConnectionsUpdateNoLongerTakesRedirectURL(t *testing.T) {
	if inChild(t, func(c *ctx) {
		c.connections([]string{"update", "conn_1", "--redirect-url", "https://myapp.example/cb"})
	}) {
		return
	}
	w := &connectionWrites{}
	c := staffCtx(t, true, w.handlers())
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, "flag provided but not defined: -redirect-url") || len(w.bodies) > 0 {
		t.Fatalf("exit %d, sent %v:\n%s", code, w.bodies, out)
	}
}

// `connections show` lists the webhook_url a connection has, and nothing about a redirect.
func TestConnectionsShowListsTheWebhookURL(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1": func(rw http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(rw).Encode(map[string]interface{}{"data": map[string]interface{}{
				"id": "conn_1", "name": "Acme", "type": "realtime", "webhook_url": "https://myapp.example/hook"}})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.connections([]string{"show", "conn_1"}) })
	if !strings.Contains(out, "webhook_url") || !strings.Contains(out, "https://myapp.example/hook") || strings.Contains(out, "redirect") {
		t.Fatalf("output = %q", out)
	}
}

// Completion offers the flag.
func TestCompletionKnowsConnectionsWebhookURL(t *testing.T) {
	if !contains(commandTree["connections"], "--webhook-url") {
		t.Fatal("completion's commandTree[connections] lacks --webhook-url")
	}
}
