package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
	"github.com/qubeintegrations/qube-cli/internal/uuid"
)

const testRelayURL = "https://qubesync.example/relay/rl_test"

// withDeviceDir points os.UserConfigDir, where the device id lives, at a temp directory on
// every platform, so no test touches the real one.
func withDeviceDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CONFIG_HOME", "AppData"} {
		setenv(t, key, dir)
	}
	return dir
}

// fastListen shortens the forwarding timeouts and the reconnect backoff for one test.
func fastListen(t *testing.T) {
	t.Helper()
	dial, header, min, max := forwardDialTimeout, forwardHeaderTimeout, listenBackoffMin, listenBackoffMax
	forwardDialTimeout, forwardHeaderTimeout = 500*time.Millisecond, 300*time.Millisecond
	listenBackoffMin, listenBackoffMax = 10*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() {
		forwardDialTimeout, forwardHeaderTimeout, listenBackoffMin, listenBackoffMax = dial, header, min, max
	})
}

// captureStderr runs fn with os.Stderr replaced by a pipe and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan []byte)
	go func() {
		data, _ := io.ReadAll(r)
		done <- data
	}()
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	return string(<-done)
}

// fakeRelay stands in for QuBe Sync's relay routes for app1: the relay is rl_test, and a
// lookup finds it. Each poll takes the next of polls; once they run out, a poll is answered
// 409 relay_taken_over, which ends `qube listen` after its deliveries in flight are reported.
type fakeRelay struct {
	mu       sync.Mutex
	create   http.HandlerFunc // overrides the create answer
	lookup   http.HandlerFunc // overrides the lookup answer
	grant    map[string]interface{}
	polls    []http.HandlerFunc
	creates  []map[string]string
	lookups  []string // device ids looked up
	sessions []string
	results  map[string]json.RawMessage
	stops    []string
	forgets  int
	polled   chan struct{} // a poll arrived
}

func newFakeRelay(polls ...http.HandlerFunc) *fakeRelay {
	return &fakeRelay{
		grant:   map[string]interface{}{"relay": map[string]interface{}{"id": "rl_test", "url": testRelayURL, "app_id": "app1"}},
		polls:   polls,
		results: map[string]json.RawMessage{},
		polled:  make(chan struct{}, 100),
	}
}

func (f *fakeRelay) handlers() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/api/cli/apps/app1/relays": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				f.mu.Lock()
				f.lookups = append(f.lookups, r.URL.Query().Get("device_id"))
				f.mu.Unlock()
				if f.lookup != nil {
					f.lookup(w, r)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{"relay": f.grant["relay"]})
				return
			}
			if f.create != nil {
				f.create(w, r)
				return
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.creates = append(f.creates, body)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(f.grant)
		},
		"/api/cli/apps/app1/relays/rl_test": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "DELETE" {
				f.mu.Lock()
				f.forgets++
				f.mu.Unlock()
				w.WriteHeader(204)
			}
		},
		"/api/cli/apps/app1/relays/rl_test/listen": func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "DELETE" {
				f.mu.Lock()
				f.stops = append(f.stops, r.URL.Query().Get("session"))
				f.mu.Unlock()
				w.WriteHeader(204)
			}
		},
		"/api/cli/apps/app1/relays/rl_test/deliveries": func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			f.sessions = append(f.sessions, r.URL.Query().Get("session"))
			var next http.HandlerFunc
			if len(f.polls) > 0 {
				next, f.polls = f.polls[0], f.polls[1:]
			}
			f.mu.Unlock()
			f.polled <- struct{}{}
			if next == nil {
				next = cliError(409, "relay_taken_over", "Another qube listen took over this URL; only the newest one receives deliveries.")
			}
			next(w, r)
		},
		"/api/cli/apps/app1/relays/rl_test/deliveries/": func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.results[strings.TrimPrefix(r.URL.Path, "/api/cli/apps/app1/relays/rl_test/deliveries/")] = data
			f.mu.Unlock()
			w.WriteHeader(204)
		},
	}
}

func cliError(status int, code, message string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"error": map[string]string{"code": code, "message": message}})
	}
}

// noRoute is how QuBe Sync (Phoenix) answers a route it doesn't have: a 404 without the
// /api/cli envelope.
func noRoute(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(404)
	_, _ = io.WriteString(w, `{"errors":{"detail":"Not Found"}}`)
}

func noContent(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }

func deliver(deliveries ...map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"deliveries": deliveries})
	}
}

// parked holds the poll open until the CLI goes away, as the server does with no work.
func parked(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }

func delivery(attemptID, path, body string) map[string]interface{} {
	return map[string]interface{}{
		"attempt_id": attemptID,
		"path":       path,
		"headers":    map[string]string{"content-type": "application/json", "x-qube-signature": "t=1700000000,v1=abc123"},
		"body":       body,
		"summary":    map[string]interface{}{"kind": "request", "id": "8d0f6a52-1111-2222-3333-444455556666", "page_request_id": "8d0f6a52-1111-2222-3333-444455556666", "page": 1, "state": "response_received"},
	}
}

// localApp is the app `qube listen` forwards to: it records what it got and answers with
// answer.
type localApp struct {
	mu      sync.Mutex
	method  string
	uri     string
	headers http.Header
	body    []byte
}

func newLocalApp(t *testing.T, answer http.HandlerFunc) (*httptest.Server, *localApp) {
	t.Helper()
	got := &localApp{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.mu.Lock()
		got.method, got.uri, got.headers, got.body = r.Method, r.URL.RequestURI(), r.Header.Clone(), body
		got.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func forwardTo(t *testing.T, raw string) listenOptions {
	t.Helper()
	opts, err := listenOptionsFrom(raw, "", false)
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

func jsonOf(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var v map[string]interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("not JSON: %q", raw)
	}
	return v
}

// The relay is created for this machine's device id, a 204 is followed by another poll, and
// the delivery goes to --forward-to plus its path with the body and headers byte for byte;
// the local app's status goes back (a 2xx's answer stays here).
func TestListenForwardsADeliveryAndReportsTheAnswer(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	app, got := newLocalApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	body := `{"page_request_id":"r1","note":"café \"quoted\"","n":1.50}`
	relay := newFakeRelay(noContent, deliver(delivery("att_1", "/runs?src=qb", body)))
	c := newAPITest(t, relay.handlers())

	var err error
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { err = c.runListen(forwardTo(t, app.URL+"/webhooks/qube")) })
	})
	if err == nil || !strings.Contains(err.Error(), "another qube listen took over "+testRelayURL) {
		t.Fatalf("err = %v", err)
	}

	deviceID, _ := config.ExistingDeviceID()
	if len(relay.creates) != 1 || relay.creates[0]["device_id"] != deviceID || !uuid.Valid(deviceID) {
		t.Fatalf("creates = %v, device id %q", relay.creates, deviceID)
	}
	if len(relay.sessions) != 3 || !uuid.Valid(relay.sessions[0]) || relay.sessions[1] != relay.sessions[0] || relay.sessions[2] != relay.sessions[0] {
		t.Fatalf("polls' sessions = %v", relay.sessions)
	}

	if got.method != "POST" || got.uri != "/webhooks/qube/runs?src=qb" {
		t.Fatalf("local app got %s %s", got.method, got.uri)
	}
	if string(got.body) != body {
		t.Fatalf("body = %q, want %q", got.body, body)
	}
	if got.headers.Get("X-Qube-Signature") != "t=1700000000,v1=abc123" || got.headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", got.headers)
	}
	if got.headers.Get("Accept-Encoding") != "" {
		t.Fatalf("Accept-Encoding added: %q", got.headers.Get("Accept-Encoding"))
	}

	result := jsonOf(t, relay.results["att_1"])
	want := map[string]interface{}{"status": float64(201), "body": ""}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("result = %s", relay.results["att_1"])
	}

	if !strings.HasPrefix(stdout, "Webhook URL for App (the same every time on this machine):\n  "+testRelayURL+"\nForwarding to "+app.URL+"/webhooks/qube. Ctrl-C to stop.\n") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "  request 8d0f6a52…  response_received  → POST /webhooks/qube/runs?src=qb  201  ") || strings.Contains(stdout, "retry") {
		t.Fatalf("stdout = %q", stdout)
	}
	if strings.Contains(stderr, "reconnect") {
		t.Fatalf("a 204 isn't a lost connection: %q", stderr)
	}
}

// A non-2xx answer is reported with the first 1,000 bytes of its body, a redirect with its
// location and never followed, and the line says QuBe will retry.
func TestListenReportsAFailedAnswerAndARedirect(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	answer := strings.Repeat("é", 800) // 1,600 bytes
	app, _ := newLocalApp(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			t.Errorf("the redirect was followed")
		}
		if strings.HasSuffix(r.URL.Path, "/moved") {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		w.WriteHeader(500)
		_, _ = w.Write([]byte(answer))
	})
	relay := newFakeRelay(deliver(delivery("att_1", "", "{}"), delivery("att_2", "/moved", "{}")))
	c := newAPITest(t, relay.handlers())

	var stdout string
	_ = captureStderr(t, func() {
		stdout = captureStdout(t, func() { _ = c.runListen(forwardTo(t, app.URL)) })
	})
	if r := jsonOf(t, relay.results["att_1"]); !reflect.DeepEqual(r, map[string]interface{}{"status": float64(500), "body": answer[:1000]}) {
		t.Fatalf("att_1 result = %v", r)
	}
	r := jsonOf(t, relay.results["att_2"])
	if r["status"] != float64(302) || r["location"] != "/login" {
		t.Fatalf("att_2 result = %v", r)
	}
	if !strings.Contains(stdout, "→ POST /  500  ") || !strings.Contains(stdout, "(QuBe will retry)") || !strings.Contains(stdout, "302 to /login, not followed") {
		t.Fatalf("stdout = %q", stdout)
	}
}

// A local app that doesn't answer in time, or isn't there, is reported as an error result.
func TestListenReportsALocalAppThatTimesOutOrRefuses(t *testing.T) {
	withDeviceDir(t)
	fastListen(t)
	setJSON(t, false)
	release := make(chan struct{})
	slow, _ := newLocalApp(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
	})
	defer close(release)
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	for name, tc := range map[string]struct{ target, kind string }{
		"times out": {slow.URL, "timeout"},
		"refuses":   {gone.URL, "connect"},
	} {
		relay := newFakeRelay(deliver(delivery("att_1", "/hook", "{}")))
		c := newAPITest(t, relay.handlers())
		var stdout string
		_ = captureStderr(t, func() {
			stdout = captureStdout(t, func() { _ = c.runListen(forwardTo(t, tc.target)) })
		})
		var result struct {
			Status *int `json:"status"`
			Error  struct {
				Kind   string `json:"kind"`
				Detail string `json:"detail"`
			} `json:"error"`
		}
		if err := json.Unmarshal(relay.results["att_1"], &result); err != nil {
			t.Fatalf("%s: result %q: %v", name, relay.results["att_1"], err)
		}
		if result.Status != nil || result.Error.Kind != tc.kind || result.Error.Detail == "" || len([]rune(result.Error.Detail)) > 200 {
			t.Fatalf("%s: result = %s", name, relay.results["att_1"])
		}
		// Go's error text, which names the local URL
		if !strings.Contains(result.Error.Detail, tc.target+"/hook") {
			t.Fatalf("%s: detail = %q", name, result.Error.Detail)
		}
		if !strings.Contains(stdout, "(QuBe will retry)") {
			t.Fatalf("%s: stdout = %q", name, stdout)
		}
	}
}

// --json: the relay first, then one object per delivery, each on its own line.
func TestListenJSONLines(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, true)
	app, _ := newLocalApp(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	relay := newFakeRelay(deliver(delivery("att_1", "/webhooks/qube", "{}")))
	c := newAPITest(t, relay.handlers())

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { _ = c.runListen(forwardTo(t, app.URL)) })
	})
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout = %q", stdout)
	}
	start := jsonOf(t, []byte(lines[0]))
	if start["relay"].(map[string]interface{})["url"] != testRelayURL || start["forward_to"] != app.URL || start["app"].(map[string]interface{})["name"] != "App" {
		t.Fatalf("first line = %s", lines[0])
	}
	line := jsonOf(t, []byte(lines[1]))
	var keys []string
	for k := range line {
		keys = append(keys, k)
	}
	if len(line) != 5 || line["path"] != "/webhooks/qube" || line["status"] != float64(204) {
		t.Fatalf("delivery line = %s (keys %v)", lines[1], keys)
	}
	if _, err := time.Parse(time.RFC3339, line["at"].(string)); err != nil {
		t.Fatalf("at = %v", line["at"])
	}
	if _, ok := line["ms"].(float64); !ok {
		t.Fatalf("ms = %v", line["ms"])
	}
	if s := line["summary"].(map[string]interface{}); s["kind"] != "request" || s["state"] != "response_received" {
		t.Fatalf("summary = %v", s)
	}
	if stderr != "" {
		t.Fatalf("notes under --json: %q", stderr)
	}
}

// A newer qube listen for the same URL ends this one, with a message saying so.
func TestListenStopsWhenTakenOver(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay() // the first poll is answered 409
	c := newAPITest(t, relay.handlers())
	var err error
	_ = captureStdout(t, func() { err = c.runListen(forwardTo(t, "http://localhost:3000")) })
	if err == nil || !strings.Contains(err.Error(), "another qube listen took over") || !strings.Contains(err.Error(), "only the newest one receives deliveries") {
		t.Fatalf("err = %v", err)
	}
	if len(relay.stops) != 0 {
		t.Fatalf("a session taken over from has nothing to stop: %v", relay.stops)
	}
}

// Ctrl-C tells the server the session stopped, then ends with context.Canceled (exit 130).
func TestListenCtrlCStopsTheSession(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay(parked)
	c := newAPITest(t, relay.handlers())
	bg, cancel := context.WithCancel(context.Background())
	c.bg = bg
	go func() {
		<-relay.polled
		cancel()
	}()
	var err error
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { err = c.runListen(forwardTo(t, "http://localhost:3000")) })
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if len(relay.stops) != 1 || relay.stops[0] != relay.sessions[0] {
		t.Fatalf("stops = %v, sessions = %v", relay.stops, relay.sessions)
	}
	if !strings.Contains(stderr, "Stopped.") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// A failed poll backs off and polls again, with a note; the delivery after it still goes.
func TestListenReconnectsAfterAFailedPoll(t *testing.T) {
	withDeviceDir(t)
	fastListen(t)
	setJSON(t, false)
	app, got := newLocalApp(t, func(w http.ResponseWriter, r *http.Request) {})
	relay := newFakeRelay(cliError(503, "unavailable", "deploying"), deliver(delivery("att_1", "", "{}")))
	c := newAPITest(t, relay.handlers())
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { _ = c.runListen(forwardTo(t, app.URL)) })
	})
	if !strings.Contains(stderr, "Lost the connection to "+c.host) || !strings.Contains(stderr, "reconnecting in") || !strings.Contains(stderr, "Reconnected.") {
		t.Fatalf("stderr = %q", stderr)
	}
	if got.method != "POST" || !strings.Contains(stdout, "→ POST /  200  ") {
		t.Fatalf("local app got %q; stdout = %q", got.method, stdout)
	}
	if len(relay.sessions) != 3 || relay.sessions[1] != relay.sessions[0] {
		t.Fatalf("sessions = %v", relay.sessions)
	}
}

// A URL forgotten meanwhile (or taken over by another machine) ends listening, saying so.
func TestListenStopsWhenTheURLIsGone(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay(cliError(404, "not_found", "no such qube listen URL for this app"))
	c := newAPITest(t, relay.handlers())
	var err error
	_ = captureStdout(t, func() { err = c.runListen(forwardTo(t, "http://localhost:3000")) })
	if err == nil || !strings.Contains(err.Error(), "no such qube listen URL for this app") || !strings.Contains(err.Error(), "Run qube listen again") {
		t.Fatalf("err = %v", err)
	}
}

// When the create answer names a relay this machine's new one took over, listen says so.
func TestListenSaysWhatItTookOver(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay()
	relay.grant["replaced"] = map[string]interface{}{"id": "rl_old", "url": "https://qubesync.example/relay/rl_old", "last_polled_at": "2026-09-01T12:00:00Z"}
	c := newAPITest(t, relay.handlers())
	stderr := captureStderr(t, func() {
		_ = captureStdout(t, func() { _ = c.runListen(forwardTo(t, "http://localhost:3000")) })
	})
	when := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Local().Format("2006-01-02 15:04 MST")
	if !strings.Contains(stderr, "This machine's new URL took over https://qubesync.example/relay/rl_old (last polled "+when+")") || !strings.Contains(stderr, "That URL no longer works") {
		t.Fatalf("stderr = %q", stderr)
	}

	relay.grant["replaced"] = map[string]interface{}{"id": "rl_old", "url": "https://qubesync.example/relay/rl_old", "last_polled_at": nil}
	stderr = captureStderr(t, func() {
		_ = captureStdout(t, func() { _ = c.runListen(forwardTo(t, "http://localhost:3000")) })
	})
	if !strings.Contains(stderr, "(never polled)") {
		t.Fatalf("stderr = %q", stderr)
	}
}

// --forget deletes this machine's URL; alone, that is all it does.
func TestListenForget(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay()
	c := newAPITest(t, relay.handlers())
	c.yes = true

	// a machine that never listened has nothing to forget, and nothing is sent
	opts, err := listenOptionsFrom("", "", true)
	if err != nil {
		t.Fatal(err)
	}
	out := captureStdout(t, func() { err = c.runListen(opts) })
	if err != nil || !strings.Contains(out, "nothing to forget") || len(relay.lookups) != 0 || len(relay.creates) != 0 {
		t.Fatalf("err = %v, out = %q, lookups = %v, creates = %v", err, out, relay.lookups, relay.creates)
	}

	// the URL is looked up, never created
	deviceID, _ := config.DeviceID()
	out = captureStdout(t, func() { err = c.runListen(opts) })
	if err != nil || relay.forgets != 1 || len(relay.lookups) != 1 || relay.lookups[0] != deviceID || len(relay.creates) != 0 {
		t.Fatalf("err = %v, forgets = %d, lookups = %v, creates = %v", err, relay.forgets, relay.lookups, relay.creates)
	}
	if !strings.Contains(out, "Forgot "+testRelayURL) || len(relay.sessions) != 0 {
		t.Fatalf("out = %q, polls = %v", out, relay.sessions)
	}

	// with --forward-to, it listens on a new URL afterwards
	opts, _ = listenOptionsFrom("http://localhost:3000", "", true)
	_ = captureStderr(t, func() {
		_ = captureStdout(t, func() { err = c.runListen(opts) })
	})
	if relay.forgets != 2 || len(relay.lookups) != 2 || len(relay.creates) != 1 || len(relay.sessions) != 1 {
		t.Fatalf("forgets = %d, lookups = %d, creates = %d, polls = %d", relay.forgets, len(relay.lookups), len(relay.creates), len(relay.sessions))
	}
}

// forgetAlone runs `qube listen --forget` with c, returning stdout and stderr.
func forgetAlone(t *testing.T, c *ctx) (string, string, error) {
	t.Helper()
	opts, err := listenOptionsFrom("", "", true)
	if err != nil {
		t.Fatal(err)
	}
	var out string
	stderr := captureStderr(t, func() {
		out = captureStdout(t, func() { err = c.runListen(opts) })
	})
	return out, stderr, err
}

// A machine that has listened, but not for this app, has nothing to forget, and no URL is
// made for it.
func TestListenForgetWithoutAURL(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay()
	relay.lookup = cliError(404, "relay_not_found", "This machine has no qube listen URL for this app.")
	c := newAPITest(t, relay.handlers())
	c.yes = true
	if _, err := config.DeviceID(); err != nil {
		t.Fatal(err)
	}

	out, _, err := forgetAlone(t, c)
	if err != nil || !strings.Contains(out, "This machine has no qube listen URL for App; nothing to forget.") || len(relay.lookups) != 1 || len(relay.creates) != 0 || relay.forgets != 0 {
		t.Fatalf("err = %v, out = %q, lookups = %v, creates = %v, forgets = %d", err, out, relay.lookups, relay.creates, relay.forgets)
	}

	setJSON(t, true)
	out, _, err = forgetAlone(t, c)
	if err != nil || strings.TrimSpace(out) != `{"forgotten":null}` {
		t.Fatalf("err = %v, out = %q", err, out)
	}
}

// With all of the user's URLs for the app on other machines, forgetting here takes none of
// them over: the create, which for this machine would take over the least recently used one,
// is never sent.
func TestListenForgetTakesNoURLOver(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay()
	relay.lookup = cliError(404, "relay_not_found", "This machine has no qube listen URL for this app.")
	relay.grant["replaced"] = map[string]interface{}{"id": "rl_old", "url": "https://qubesync.example/relay/rl_old", "last_polled_at": "2026-09-01T12:00:00Z"}
	c := newAPITest(t, relay.handlers())
	c.yes = true
	if _, err := config.DeviceID(); err != nil {
		t.Fatal(err)
	}

	out, stderr, err := forgetAlone(t, c)
	if err != nil || !strings.Contains(out, "nothing to forget") || len(relay.creates) != 0 || relay.forgets != 0 || strings.Contains(stderr, "took over") {
		t.Fatalf("err = %v, out = %q, stderr = %q, creates = %v, forgets = %d", err, out, stderr, relay.creates, relay.forgets)
	}
}

// A server from before the lookup (Phoenix's own 404) is asked with the create instead, as
// before; one whose 404 has the envelope (an unknown app) is not.
func TestListenForgetOnAServerWithoutTheLookup(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	relay := newFakeRelay()
	relay.lookup = noRoute
	c := newAPITest(t, relay.handlers())
	c.yes = true
	deviceID, _ := config.DeviceID()

	out, _, err := forgetAlone(t, c)
	if err != nil || !strings.Contains(out, "Forgot "+testRelayURL) || relay.forgets != 1 || len(relay.creates) != 1 || relay.creates[0]["device_id"] != deviceID {
		t.Fatalf("err = %v, out = %q, forgets = %d, creates = %v", err, out, relay.forgets, relay.creates)
	}

	// the create took over another machine's URL: forgetting says so
	relay.grant["replaced"] = map[string]interface{}{"id": "rl_old", "url": "https://qubesync.example/relay/rl_old", "last_polled_at": nil}
	_, stderr, err := forgetAlone(t, c)
	if err != nil || !strings.Contains(stderr, "took over https://qubesync.example/relay/rl_old (never polled)") {
		t.Fatalf("err = %v, stderr = %q", err, stderr)
	}

	// all of the user's URLs listening, none of them this machine's
	relay.create = cliError(422, "too_many_relays", "All 10 of your qube listen URLs for this app are in use.")
	out, _, err = forgetAlone(t, c)
	if err != nil || !strings.Contains(out, "nothing to forget") || relay.forgets != 2 {
		t.Fatalf("err = %v, out = %q, forgets = %d", err, out, relay.forgets)
	}

	relay.create = nil
	relay.lookup = cliError(404, "not_found", "no such app in this organization")
	creates := len(relay.creates)
	if _, _, err = forgetAlone(t, c); err == nil || err.Error() != "no such app in this organization" || len(relay.creates) != creates {
		t.Fatalf("err = %v, creates = %d", err, len(relay.creates))
	}
}

// --write-env upserts QUBE_WEBHOOK_URL and leaves every other line, QUBE_* ones included.
func TestListenWriteEnv(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	dir := t.TempDir()
	for name, tc := range map[string]struct{ before, after string }{
		"replaces": {
			"RAILS_ENV=development\nexport QUBE_WEBHOOK_URL=https://old.ngrok.app/webhooks\nQUBE_API_KEY=sk_x\n",
			"RAILS_ENV=development\nexport QUBE_WEBHOOK_URL=" + testRelayURL + "\nQUBE_API_KEY=sk_x\n",
		},
		"appends": {
			"QUBE_URL=https://qubesync.com\nQUBE_API_KEY=sk_x\n",
			"QUBE_URL=https://qubesync.com\nQUBE_API_KEY=sk_x\nQUBE_WEBHOOK_URL=" + testRelayURL + "\n",
		},
		"creates": {"", "QUBE_WEBHOOK_URL=" + testRelayURL + "\n"},
	} {
		p := filepath.Join(dir, name+".env")
		if tc.before != "" {
			if err := os.WriteFile(p, []byte(tc.before), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		relay := newFakeRelay()
		c := newAPITest(t, relay.handlers())
		opts, err := listenOptionsFrom("http://localhost:3000", p, false)
		if err != nil {
			t.Fatal(err)
		}
		stderr := captureStderr(t, func() {
			_ = captureStdout(t, func() { _ = c.runListen(opts) })
		})
		if got, _ := os.ReadFile(p); string(got) != tc.after {
			t.Fatalf("%s: got\n%s\nwant\n%s", name, got, tc.after)
		}
		if !strings.Contains(stderr, "Wrote QUBE_WEBHOOK_URL to "+p) {
			t.Fatalf("%s: stderr = %q", name, stderr)
		}
	}
}

// Without --write-env, a .env here that lacks the URL gets the line to add.
func TestListenSuggestsTheEnvLine(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	run := func() string {
		relay := newFakeRelay()
		c := newAPITest(t, relay.handlers())
		return captureStderr(t, func() {
			_ = captureStdout(t, func() { _ = c.runListen(forwardTo(t, "http://localhost:3000")) })
		})
	}
	if stderr := run(); strings.Contains(stderr, "QUBE_WEBHOOK_URL") {
		t.Fatalf("no .env, no hint: %q", stderr)
	}
	if err := os.WriteFile(".env", []byte("QUBE_URL=https://qubesync.com\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stderr := run(); !strings.Contains(stderr, ".env doesn't set QUBE_WEBHOOK_URL") || !strings.Contains(stderr, "\n  QUBE_WEBHOOK_URL="+testRelayURL+"\n") {
		t.Fatalf("stderr = %q", stderr)
	}
	if err := os.WriteFile(".env", []byte("export QUBE_WEBHOOK_URL=\""+testRelayURL+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stderr := run(); strings.Contains(stderr, "QUBE_WEBHOOK_URL") {
		t.Fatalf(".env already has it: %q", stderr)
	}
}

// --forward-to is required (unless --forget comes alone), and must be an http(s) URL.
func TestListenOptions(t *testing.T) {
	for _, tc := range []struct {
		forwardTo, writeEnv string
		forget              bool
		ok                  bool
		want                string
	}{
		{"", "", false, false, ""},
		{"", ".env", true, false, ""},
		{"", "", true, true, ""},
		{"http://localhost:3000/webhooks/qube", "", false, true, "http://localhost:3000/webhooks/qube"},
		{"localhost:3000", "", false, true, "http://localhost:3000"},
		{"https://app.test/hooks#frag", "", false, true, "https://app.test/hooks"},
		{"ftp://localhost/x", "", false, false, ""},
	} {
		opts, err := listenOptionsFrom(tc.forwardTo, tc.writeEnv, tc.forget)
		if (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v", tc, err)
			continue
		}
		if tc.want != "" && opts.forwardTo.String() != tc.want {
			t.Errorf("%+v: forward-to = %s", tc, opts.forwardTo)
		}
	}
	if _, err := listenOptionsFrom("", "", false); err == nil || !strings.Contains(err.Error(), "qube listen needs --forward-to") {
		t.Fatalf("err = %v", err)
	}
}

// `qube listen` without --forward-to is a usage error (exit 2), and sends nothing.
func TestListenWithoutForwardToIsRefused(t *testing.T) {
	if os.Getenv("QUBE_TEST_LISTEN_CHILD") != "" {
		c := &ctx{host: "http://127.0.0.1:1", cfg: &config.File{Sessions: map[string]config.Session{}}, bg: context.Background()}
		c.listen([]string{"--write-env", ".env"})
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestListenWithoutForwardToIsRefused$")
	cmd.Env = append(os.Environ(), "QUBE_TEST_LISTEN_CHILD=1")
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != ui.ExitUsage {
		t.Fatalf("want exit %d, got %v\n%s", ui.ExitUsage, err, out)
	}
	if !strings.Contains(string(out), "qube listen needs --forward-to") {
		t.Fatalf("output = %s", out)
	}
}

// A server from before `qube listen` answers 404 without the CLI's envelope; an unknown app
// is a 404 with it, and its message is shown.
func TestListenOnAServerWithoutRelays(t *testing.T) {
	withDeviceDir(t)
	c := newAPITest(t, nil) // no /relays route: the mux's plain 404
	err := c.runListen(forwardTo(t, "http://localhost:3000"))
	if err == nil || err.Error() != "this QuBe Sync ("+c.host+") doesn't support `qube listen` yet" {
		t.Fatalf("err = %v", err)
	}

	relay := newFakeRelay()
	relay.create = cliError(404, "not_found", "no such app in this organization")
	c = newAPITest(t, relay.handlers())
	if err := c.runListen(forwardTo(t, "http://localhost:3000")); err == nil || err.Error() != "no such app in this organization" {
		t.Fatalf("err = %v", err)
	}
}

// A production app is refused by the server; its message is what the user reads.
func TestListenProductionAppIsRefused(t *testing.T) {
	withDeviceDir(t)
	message := "qube listen works with sandbox apps only. Choose a sandbox app with --app or `qube use`."
	relay := newFakeRelay()
	relay.create = cliError(403, "relay_sandbox_only", message)
	c := newAPITestApp(t, false, relay.handlers())
	if err := c.runListen(forwardTo(t, "http://localhost:3000")); err == nil || err.Error() != message {
		t.Fatalf("err = %v", err)
	}
}

// A late result is noted on the line: QuBe Sync had stopped waiting and retries.
func TestListenNotesAResultReportedTooLate(t *testing.T) {
	withDeviceDir(t)
	setJSON(t, false)
	app, _ := newLocalApp(t, func(w http.ResponseWriter, r *http.Request) {})
	relay := newFakeRelay(deliver(delivery("att_1", "", "{}")))
	handlers := relay.handlers()
	handlers["/api/cli/apps/app1/relays/rl_test/deliveries/"] = cliError(404, "attempt_not_pending", "That delivery isn't waiting for a result")
	c := newAPITest(t, handlers)
	stdout := captureStdout(t, func() {
		_ = captureStderr(t, func() { _ = c.runListen(forwardTo(t, app.URL)) })
	})
	if !strings.Contains(stdout, "  200  ") || !strings.Contains(stdout, "(reported too late; QuBe will retry)") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestForwardTarget(t *testing.T) {
	for _, tc := range []struct{ base, suffix, want string }{
		{"http://localhost:3000", "", "http://localhost:3000"},
		{"http://localhost:3000/webhooks/qube", "", "http://localhost:3000/webhooks/qube"},
		{"http://localhost:3000", "/webhooks/qube", "http://localhost:3000/webhooks/qube"},
		{"http://localhost:3000/", "/webhooks/qube", "http://localhost:3000/webhooks/qube"},
		{"http://localhost:3000/hooks", "/runs?src=qb", "http://localhost:3000/hooks/runs?src=qb"},
		{"http://localhost:3000/hooks?app=1", "/runs?src=qb", "http://localhost:3000/hooks/runs?app=1&src=qb"},
		{"http://localhost:3000/hooks", "?src=qb", "http://localhost:3000/hooks?src=qb"},
		{"http://localhost:3000", "/a%20b/c", "http://localhost:3000/a%20b/c"},
	} {
		base, _ := url.Parse(tc.base)
		if got := (&forwarder{base: base}).target(tc.suffix); got != tc.want {
			t.Errorf("%s + %q = %s, want %s", tc.base, tc.suffix, got, tc.want)
		}
	}
}

func TestSummaryText(t *testing.T) {
	for _, tc := range []struct {
		summary map[string]interface{}
		want    string
	}{
		{map[string]interface{}{"kind": "request", "id": "01JA0000aaaa", "page": float64(1), "state": "response_received"}, "request 01JA0000…  response_received"},
		{map[string]interface{}{"kind": "request", "id": "01JA0000aaaa", "page": float64(3), "state": "response_received"}, "request 01JA0000…  response_received  page 3"},
		{map[string]interface{}{"kind": "workflow_run", "id": "run_12345678", "event": "workflow_run.completed"}, "workflow_run.completed  run run_1234…"},
		{map[string]interface{}{"kind": "notification", "id": "run_1", "event": "workflow_run.notification", "name": "customers_page", "seq": float64(3)}, "workflow_run.notification  customers_page #3"},
		{map[string]interface{}{"kind": "name_index", "id": "conn_123456", "event": "name_index.ready"}, "name_index.ready  connection conn_123…"},
		{map[string]interface{}{"kind": "something_new", "id": "x1", "event": "x.happened"}, "something_new  x.happened  x1"},
		{nil, "delivery"},
	} {
		if got := summaryText(tc.summary); got != tc.want {
			t.Errorf("summaryText(%v) = %q, want %q", tc.summary, got, tc.want)
		}
	}
}

func TestTruncateRunes(t *testing.T) {
	long := strings.Repeat("ü", 250)
	got := truncateRunes(long, 200)
	if n := len([]rune(got)); n != 200 || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d runes: %q", n, got)
	}
	if truncateRunes("short", 200) != "short" {
		t.Fatal("a short one changed")
	}
}

// The result's JSON is one of the two shapes the server takes.
func TestDeliveryResultShapes(t *testing.T) {
	for _, tc := range []struct {
		result api.DeliveryResult
		want   string
	}{
		{api.DeliveryResult{Status: 200}, `{"body":"","status":200}`},
		{api.DeliveryResult{Status: 302, Body: "x", Location: "/login"}, `{"body":"x","location":"/login","status":302}`},
		{api.DeliveryResult{Error: &api.ForwardError{Kind: "connect", Detail: "refused"}}, `{"error":{"kind":"connect","detail":"refused"}}`},
	} {
		got, err := json.Marshal(tc.result)
		if err != nil || string(got) != tc.want {
			t.Errorf("%+v: %s %v, want %s", tc.result, got, err, tc.want)
		}
	}
}
