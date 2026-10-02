package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

func TestParseAnywhereFlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	data := fs.String("data", "", "")
	pub := fs.Bool("publish", false, "")
	parseAnywhere(fs, []string{"POST", "/api/v2/x", "--data", `{"a":1}`, "--publish", "extra"})
	if *data != `{"a":1}` || !*pub || strings.Join(fs.Args(), " ") != "POST /api/v2/x extra" {
		t.Fatalf("data=%q publish=%v args=%v", *data, *pub, fs.Args())
	}
}

func TestParseAnywhereEqualsFormAndDoubleDash(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	n := fs.Int("page", 1, "")
	parseAnywhere(fs, []string{"conn-1", "--page=3", "--", "--not-a-flag"})
	if *n != 3 || strings.Join(fs.Args(), " ") != "conn-1 --not-a-flag" {
		t.Fatalf("page=%d args=%v", *n, fs.Args())
	}
}

func TestParseAnywhereStdinDashIsPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	parseAnywhere(fs, []string{"-"})
	if len(fs.Args()) != 1 || fs.Arg(0) != "-" {
		t.Fatalf("args=%v", fs.Args())
	}
}

func TestUpsertEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("RAILS_ENV=development\nexport QUBE_API_KEY=old\n# comment\nQUBE_URL=https://old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := upsertEnv(p, map[string]string{"QUBE_URL": "https://dev.qubesync.com", "QUBE_API_KEY": "sk_new", "QUBE_WEBHOOK_SECRET": "whs"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "RAILS_ENV=development\nQUBE_API_KEY=sk_new\n# comment\nQUBE_URL=https://dev.qubesync.com\nQUBE_WEBHOOK_SECRET=whs\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("existing mode must be kept, got %o", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("a temp file was left behind: %v", entries)
	}
}

func TestUpsertEnvCreatesPrivateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := upsertEnv(p, map[string]string{"QUBE_URL": "u", "QUBE_API_KEY": "k", "QUBE_WEBHOOK_SECRET": "s"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "QUBE_URL=u\nQUBE_API_KEY=k\nQUBE_WEBHOOK_SECRET=s\n" {
		t.Fatalf("got %q", got)
	}
	if info, _ := os.Stat(p); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o", info.Mode().Perm())
	}
}

func TestPushBodyOmitsAbsentNameAndDescription(t *testing.T) {
	key, body, err := pushBody([]byte(`{"key":"k1","version":1,"initial":"a","states":{}}`), true, "first")
	if err != nil || key != "k1" {
		t.Fatalf("%v %v", key, err)
	}
	if _, has := body["name"]; has {
		t.Fatal("name must be omitted when the chart has none")
	}
	if body["publish"] != true || body["notes"] != "first" {
		t.Fatalf("body = %v", body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "null") {
		t.Fatalf("no nulls may be sent: %s", raw)
	}

	_, body, _ = pushBody([]byte(`{"key":"k2","name":"Named","description":"d"}`), false, "")
	if body["name"] != "Named" || body["description"] != "d" || body["publish"] != nil {
		t.Fatalf("body = %v", body)
	}
	if _, _, err := pushBody([]byte(`{"version":1}`), false, ""); err == nil {
		t.Fatal("a chart without a key is refused")
	}
	if _, _, err := pushBody([]byte(`nope`), false, ""); err == nil {
		t.Fatal("non-JSON is refused")
	}
}

func TestApplyFaultFlags(t *testing.T) {
	f := map[string]interface{}{"next_response_error": "xml_error", "latency_ms": float64(50)}
	if err := applyFaultFlags(f, "closed", "3100", 0); err != nil {
		t.Fatal(err)
	}
	if f["connection_error"] != "qb_closed" || f["next_response_error"] != nil || f["latency_ms"] != nil {
		t.Fatalf("faults = %v", f)
	}
	if ns, _ := f["next_status"].(map[string]interface{}); ns["code"] != 3100 {
		t.Fatalf("next_status = %v", f["next_status"])
	}
	if err := applyFaultFlags(f, "ok", "ok", -1); err != nil || len(f) != 0 {
		t.Fatalf("clearing: %v %v", err, f)
	}
	if applyFaultFlags(f, "meteor", "", -1) == nil || applyFaultFlags(f, "", "soon", -1) == nil {
		t.Fatal("bad values are rejected")
	}
}

func TestSameHost(t *testing.T) {
	if err := sameHost("https://dev.qubesync.com", "https://dev.qubesync.com/api"); err != nil {
		t.Fatal(err)
	}
	if err := sameHost("https://evil.example", "https://dev.qubesync.com/api"); err == nil {
		t.Fatal("a key for another host must be refused")
	}
	if err := sameHost("https://x", ""); err != nil {
		t.Fatal("no api_base_url means no check")
	}
}

type fakePoller struct {
	answers []interface{} // *api.TokenGrant or error, in order
	calls   int
}

func (f *fakePoller) PollToken(string) (*api.TokenGrant, error) {
	a := f.answers[f.calls]
	f.calls++
	if g, ok := a.(*api.TokenGrant); ok {
		return g, nil
	}
	return nil, a.(error)
}

type never struct{}

func (never) Done() <-chan struct{} { return nil }

func TestPollUntilApprovedRetriesTransientErrors(t *testing.T) {
	grant := &api.TokenGrant{Token: "qct_ok"}
	p := &fakePoller{answers: []interface{}{api.ErrPending, &api.Error{Status: 502}, errors.New("dial tcp: connection refused"), api.ErrPending, grant}}
	// a plain error is not retryable, so make it a *url.Error-like net failure via api.Retryable rules:
	p.answers[2] = &api.Error{Status: 503}
	got, err := pollUntilApproved(never{}, p, "dc", time.Millisecond, time.Second)
	if err != nil || got.Token != "qct_ok" || p.calls != 5 {
		t.Fatalf("got %v err %v calls %d", got, err, p.calls)
	}
}

func TestPollUntilApprovedStopsOnDenial(t *testing.T) {
	p := &fakePoller{answers: []interface{}{api.ErrPending, &api.Error{Status: 403, Code: "access_denied", Message: "denied"}}}
	_, err := pollUntilApproved(never{}, p, "dc", time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "denied") || p.calls != 2 {
		t.Fatalf("err %v calls %d", err, p.calls)
	}
}

func TestPollUntilApprovedGivesUpAfterFiveFailures(t *testing.T) {
	var answers []interface{}
	for i := 0; i < 6; i++ {
		answers = append(answers, &api.Error{Status: 500})
	}
	p := &fakePoller{answers: answers}
	_, err := pollUntilApproved(never{}, p, "dc", time.Millisecond, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "after 5 attempts") || p.calls != 5 {
		t.Fatalf("err %v calls %d", err, p.calls)
	}
}

func TestPollUntilApprovedExpires(t *testing.T) {
	p := &fakePoller{answers: []interface{}{api.ErrPending, api.ErrPending, api.ErrPending, api.ErrPending, api.ErrPending, api.ErrPending}}
	_, err := pollUntilApproved(never{}, p, "dc", 2*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err %v", err)
	}
}

func TestParseTimeout(t *testing.T) {
	if parseTimeout("") != api.DefaultTimeout || parseTimeout("30") != 30*time.Second || parseTimeout("2m") != 2*time.Minute {
		t.Fatal("timeout parsing")
	}
}

func TestDiscardConflict(t *testing.T) {
	cases := []struct{ id, code, want string }{
		{"req_1", "in_flight", `The Web Connector already has req_1, so it can't be discarded: it will be answered or time out.`},
		{"req_1", "already_finished", `req_1 has already ended, so there is nothing to discard.`},
		{"req_1", "something_else", ""},
	}
	for _, tc := range cases {
		if got := discardConflict(tc.id, tc.code); got != tc.want {
			t.Fatalf("discardConflict(%q, %q) = %q, want %q", tc.id, tc.code, got, tc.want)
		}
	}
}

func TestSyncSummary(t *testing.T) {
	cases := []struct {
		name string
		data map[string]interface{}
		want string
	}{
		{"nothing to do", map[string]interface{}{"result": "nothing_to_do"},
			"Nothing was waiting: everything queued had already been answered."},
		{"connection error", map[string]interface{}{"result": "connection_error"},
			"The session failed with the connection fault that is set. The request it was sending is retried next session; while the fault lasts, QuBe spaces sessions out, up to 30 minutes apart."},
		{"postponed", map[string]interface{}{"result": "postponed", "retry_in": float64(12)},
			"QuickBooks couldn't be reached on the last tries, so QuBe asked the Web Connector to wait 12s. Run `qube simulator sync conn-1` again to go ahead, as clicking Update Selected again would."},
		{"answered", map[string]interface{}{"result": "ok", "answered": float64(3), "errors": float64(1)},
			"Session done: 3 answered, 1 failed."},
	}
	for _, tc := range cases {
		if got := syncSummary(tc.data, "conn-1"); got != tc.want {
			t.Fatalf("%s:\n got  %q\n want %q", tc.name, got, tc.want)
		}
	}
}

// newAPITest starts a fake server standing in for both /api/cli (app resolution,
// credentials) and /api/v2: it serves /api/cli/apps and /api/cli/apps/app1/credentials for
// one sandbox app, "app1", registers the given handlers alongside them, and returns a *ctx
// wired up to reach it. The server is closed on test cleanup.
func newAPITest(t *testing.T, handlers map[string]http.HandlerFunc) *ctx {
	t.Helper()
	return newAPITestApp(t, true, handlers)
}

// newAPITestApp is newAPITest with the app a sandbox one or a production one.
func newAPITestApp(t *testing.T, sandbox bool, handlers map[string]http.HandlerFunc) *ctx {
	t.Helper()
	var srv *httptest.Server
	app := map[string]interface{}{"id": "app1", "name": "App", "sandbox": sandbox}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/cli/apps", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data":  []map[string]interface{}{app},
			"scope": "all",
		})
	})
	credentials := func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"data": map[string]interface{}{
				"app":          app,
				"api_key":      "sk_test",
				"api_base_url": srv.URL,
			},
		})
	}
	if _, own := handlers["/api/cli/apps/app1/credentials"]; !own {
		mux.HandleFunc("/api/cli/apps/app1/credentials", credentials)
	}
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &ctx{
		host: srv.URL,
		cfg:  &config.File{Sessions: map[string]config.Session{srv.URL: {Token: "qct_x"}}},
		bg:   context.Background(),
	}
}

// setJSON sets ui.JSON for the duration of a test and resets it to false afterwards, so
// one test's choice never leaks into the next.
func setJSON(t *testing.T, v bool) {
	t.Helper()
	ui.JSON = v
	t.Cleanup(func() { ui.JSON = false })
}

// TestRequestsDiscardHitsEndpoint drives `requests discard` end to end against a fake
// server standing in for both /api/cli (app resolution, credentials) and /api/v2, to
// check the command reaches POST /api/v2/connections/<connection>/queued_requests/<id>/discard.
func TestRequestsDiscardHitsEndpoint(t *testing.T) {
	var gotMethod, gotPath string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/queued_requests/req_1/discard": func(w http.ResponseWriter, r *http.Request) {
			gotMethod, gotPath = r.Method, r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"data": map[string]interface{}{"id": "req_1", "state": "discarded"},
			})
		},
	})

	setJSON(t, false)
	out := captureStdout(t, func() { c.requests([]string{"discard", "conn_1", "req_1"}) })

	if gotMethod != "POST" || gotPath != "/api/v2/connections/conn_1/queued_requests/req_1/discard" {
		t.Fatalf("server saw method=%s path=%s", gotMethod, gotPath)
	}
	if !strings.Contains(out, `Discarded req_1.`) {
		t.Fatalf("output = %q", out)
	}
}

// captureStdout runs fn with os.Stdout replaced by a pipe and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	fn()
	_ = w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data)
}

// setenv is t.Setenv, which Go 1.16 lacks.
func setenv(t *testing.T, key, value string) {
	old, had := os.LookupEnv(key)
	os.Setenv(key, value)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
}
