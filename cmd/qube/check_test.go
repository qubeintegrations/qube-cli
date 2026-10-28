package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

const checkTestSecret = "whsec_Zx9Kq4Lp7Vt2Rm8Nb5Qd1Yc3Wf6"

// ---------------------------------------------------------------- the apps under test

// webhookApp is an app's webhook endpoint, POST /webhooks/qube/<operation>/<record id>, written the
// way the integration kit writes it: verify the signature over the raw body and the timestamp
// first (401), then look the slug up in a fixed map (404), then answer. With no field set it does
// everything right. Each field is one mistake.
type webhookApp struct {
	secret string
	ops    map[string]bool

	skipHMAC    bool          // accepts a bad signature (it still refuses an old timestamp)
	skipAge     bool          // accepts a stale timestamp (it still checks the signature)
	anySlug     bool          // answers 200 to a path none of its routes has
	routeFirst  bool          // looks the route up before it verifies, so an unrouted path is a 404
	redirectAll string        // every request is a 302 to this, before anything else (force-SSL, a login)
	sleepAll    time.Duration // every request waits this long first
	echoSecret  bool          // every answer quotes the webhook secret, as a debug page can

	mu   sync.Mutex
	seen []seenDelivery
}

type seenDelivery struct {
	host      string
	path      string
	signature string
	checkID   string
	header    http.Header
	body      []byte
}

func newWebhookApp() *webhookApp {
	return &webhookApp{secret: checkTestSecret, ops: map[string]bool{"runs": true, "orders": true}}
}

func serveWebhookApp(t *testing.T, a *webhookApp) string {
	t.Helper()
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	return srv.URL + "/webhooks/qube"
}

func (a *webhookApp) deliveries() []seenDelivery {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]seenDelivery(nil), a.seen...)
}

func (a *webhookApp) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	a.mu.Lock()
	a.seen = append(a.seen, seenDelivery{host: r.Host, path: r.URL.Path, signature: r.Header.Get("X-QUBE-Signature"),
		checkID: r.Header.Get("X-QUBE-Check"), header: r.Header.Clone(), body: body})
	a.mu.Unlock()
	if a.sleepAll > 0 {
		time.Sleep(a.sleepAll)
	}
	if a.redirectAll != "" {
		w.Header().Set("Location", a.redirectAll)
		a.reply(w, http.StatusFound)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/webhooks/qube/"), "/")
	routed := len(parts) == 2 && (a.ops[parts[0]] || a.anySlug)
	if a.routeFirst && !routed {
		a.reply(w, http.StatusNotFound)
		return
	}
	if !a.verify(body, r.Header.Get("X-QUBE-Signature")) {
		a.reply(w, http.StatusUnauthorized)
		return
	}
	if !routed {
		a.reply(w, http.StatusNotFound)
		return
	}
	a.reply(w, http.StatusOK)
}

func (a *webhookApp) reply(w http.ResponseWriter, status int) {
	if a.echoSecret {
		w.Header().Set("X-Debug", "QUBE_WEBHOOK_SECRET="+a.secret)
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "/login?token="+a.secret)
		}
		w.WriteHeader(status)
		fmt.Fprint(w, "<pre>ENV QUBE_WEBHOOK_SECRET="+a.secret+"</pre>")
		return
	}
	w.WriteHeader(status)
}

// verify is the kit's verify_webhook!, plus one mistake per field.
func (a *webhookApp) verify(body []byte, header string) bool {
	t, sigs, ok := parseSignature(header)
	if !ok {
		return false
	}
	age := time.Now().Unix() - t
	if age < 0 {
		age = -age
	}
	if !a.skipAge && age > 300 {
		return false
	}
	if a.skipHMAC {
		return true
	}
	want := hmacHex(a.secret, t, body)
	for _, s := range sigs {
		if hmac.Equal([]byte(s), []byte(want)) {
			return true
		}
	}
	return false
}

func parseSignature(header string) (t int64, v1 []string, ok bool) {
	for _, field := range strings.Split(header, ",") {
		kv := strings.SplitN(strings.TrimSpace(field), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "t":
			n, err := strconv.ParseInt(kv[1], 10, 64)
			if err != nil {
				return 0, nil, false
			}
			t, ok = n, true
		case "v1":
			v1 = append(v1, kv[1])
		}
	}
	return t, v1, ok
}

// hmacHex is written out here, apart from the code under test.
func hmacHex(secret string, t int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(t, 10) + "."))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------- running the command

// checkCtx is a fake QuBe Sync whose one app has the test secret; hits counts the times the
// secret was asked for.
func checkCtx(t *testing.T, sandbox bool) (*ctx, *int32) {
	t.Helper()
	var hits int32
	c := newAPITestApp(t, sandbox, map[string]http.HandlerFunc{
		"/api/cli/apps/app1/credentials": func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"app":            map[string]interface{}{"id": "app1", "name": "App", "sandbox": sandbox},
				"api_key":        "sk_test",
				"webhook_secret": checkTestSecret,
			}})
		},
	})
	return c, &hits
}

// withLimits shortens the check's limits for one test.
func withLimits(t *testing.T, request, slow time.Duration) {
	t.Helper()
	oldRequest, oldSlow := checkRequestTimeout, checkSlowAfter
	if request > 0 {
		checkRequestTimeout = request
	}
	if slow > 0 {
		checkSlowAfter = slow
	}
	t.Cleanup(func() { checkRequestTimeout, checkSlowAfter = oldRequest, oldSlow })
}

// runChecks runs `qube check webhooks args...` and reads the report from --json. out and errOut
// are everything the command printed.
func runChecks(t *testing.T, c *ctx, args ...string) (rep webhookCheckReport, ok bool, out, errOut string) {
	t.Helper()
	setJSON(t, true)
	errOut = captureStderr(t, func() {
		out = captureStdout(t, func() { ok = c.checkWebhooks(args) })
	})
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("not the report as JSON: %v\n%s", err, out)
	}
	return rep, ok, out, errOut
}

func resultOf(rep webhookCheckReport, id string) webhookCheckResult {
	for _, r := range rep.Checks {
		if r.Check == id {
			return r
		}
	}
	return webhookCheckResult{}
}

func failedChecks(rep webhookCheckReport) []string {
	var failed []string
	for _, r := range rep.Checks {
		if r.Result == "fail" {
			failed = append(failed, r.Check)
		}
	}
	sort.Strings(failed)
	return failed
}

var allChecks = []string{checkBadSignature, checkStale, checkUnknownSlug}

// ---------------------------------------------------------------- an app that is right

func TestCheckWebhooksAGoodAppPassesEveryCheck(t *testing.T) {
	app := newWebhookApp()
	target := serveWebhookApp(t, app)
	c, hits := checkCtx(t, true)

	rep, ok, _, _ := runChecks(t, c, target)

	if !ok || !rep.OK || rep.Summary != (webhookCheckSummary{Passed: 3}) {
		t.Fatalf("ok=%v summary=%+v checks=%+v", ok, rep.Summary, rep.Checks)
	}
	if rep.URL != target || rep.App.ID != "app1" || !rep.App.Sandbox {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Checks) != len(allChecks) {
		t.Fatalf("%d checks, want %d", len(rep.Checks), len(allChecks))
	}
	for i, id := range allChecks {
		r := rep.Checks[i]
		if r.Check != id || r.Result != "pass" || r.Pass == nil || !*r.Pass || r.Rule == "" || r.Status == 0 || r.ElapsedMS == nil {
			t.Errorf("check %d = %+v", i, r)
		}
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("the secret was fetched %d times", n)
	}

	// one request per check, in order; with no --operation the refusals go to /runs/qube-check-N
	seen := app.deliveries()
	wantPaths := []string{
		"/webhooks/qube/runs/qube-check-1",
		"/webhooks/qube/runs/qube-check-2",
		"/webhooks/qube/qube-check-unknown/1",
	}
	if len(seen) != 3 {
		t.Fatalf("the app saw %d requests, want 3", len(seen))
	}
	now := time.Now().Unix()
	for i, d := range seen {
		if d.path != wantPaths[i] || d.checkID != allChecks[i] {
			t.Errorf("request %d: %s (%s), want %s (%s)", i+1, d.path, d.checkID, wantPaths[i], allChecks[i])
		}
		if d.header.Get("Content-Type") != "application/json" || !strings.HasPrefix(d.header.Get("User-Agent"), "qube-cli/") {
			t.Errorf("request %d headers = %v", i+1, d.header)
		}
		if !regexp.MustCompile(`^t=\d+,v1=[0-9a-f]{64}$`).MatchString(d.signature) {
			t.Errorf("request %d: signature header %q", i+1, d.signature)
		}
		if !json.Valid(d.body) {
			t.Errorf("request %d: body isn't JSON: %s", i+1, d.body)
		}
		ts, sigs, _ := parseSignature(d.signature)
		valid := len(sigs) == 1 && sigs[0] == hmacHex(checkTestSecret, ts, d.body)
		age := now - ts
		switch i {
		case 0: // a wrong v1: right in everything but its last digit
			if valid || sigs[0][:63] != hmacHex(checkTestSecret, ts, d.body)[:63] {
				t.Errorf("the bad-signature request is signed %q", d.signature)
			}
		case 1: // right, over a timestamp ten minutes old, which the body carries too
			if !valid || age < 595 || age > 610 {
				t.Errorf("the stale request: valid=%v age=%d", valid, age)
			}
		default:
			if !valid || age > 5 {
				t.Errorf("request %d: valid=%v age=%d", i+1, valid, age)
			}
		}
	}

	// the bodies look like the server's, and nothing in them is an app's record
	for i := range seen {
		var ev map[string]interface{}
		if err := json.Unmarshal(seen[i].body, &ev); err != nil {
			t.Fatal(err)
		}
		if ev["event"] != "workflow_run.completed" || ev["run_state"] != "completed" || ev["app_id"] != "app1" {
			t.Errorf("request %d is %v", i+1, ev)
		}
		for _, k := range []string{"reference", "run_id", "connection_id", "workflow"} {
			if s, _ := ev[k].(string); !strings.HasPrefix(s, "qube-check") {
				t.Errorf("request %d: %s = %v", i+1, k, ev[k])
			}
		}
		if ts, _, _ := parseSignature(seen[i].signature); int64(ev["timestamp"].(float64)) != ts {
			t.Errorf("request %d: the body's timestamp isn't the signed one", i+1)
		}
		links := ev["links"].(map[string]interface{})
		if !strings.HasPrefix(links["run"].(string), c.host+"/api/v2/connections/qube-check-connection/workflow-runs/") {
			t.Errorf("request %d: links = %v", i+1, links)
		}
	}
}

// --operation changes the segment the two refusal checks send to; the unknown path stays put.
func TestCheckWebhooksOperationChoosesThePath(t *testing.T) {
	app := newWebhookApp()
	target := serveWebhookApp(t, app)
	c, _ := checkCtx(t, true)

	rep, ok, _, _ := runChecks(t, c, target, "--operation", "orders")

	if !ok || rep.Summary != (webhookCheckSummary{Passed: 3}) {
		t.Fatalf("ok=%v summary=%+v", ok, rep.Summary)
	}
	var paths []string
	for _, d := range app.deliveries() {
		paths = append(paths, d.path)
	}
	want := "/webhooks/qube/orders/qube-check-1 /webhooks/qube/orders/qube-check-2 /webhooks/qube/qube-check-unknown/1"
	if strings.Join(paths, " ") != want {
		t.Errorf("paths = %v", paths)
	}
}

func TestCheckWebhooksTableNamesTheRuleOfEachCheck(t *testing.T) {
	app := newWebhookApp()
	target := serveWebhookApp(t, app)
	c, _ := checkCtx(t, true)
	setJSON(t, false)
	var ok bool
	out := captureStdout(t, func() { ok = c.checkWebhooks([]string{target}) })
	if !ok {
		t.Fatalf("not ok:\n%s", out)
	}
	for _, want := range []string{"CHECK", "RULE", "SENT", "EXPECTED", "GOT", "RESULT",
		"bad-signature-401", "verify-signature", "stale-timestamp-401", "refuse-stale-timestamp",
		"unknown-operation-404", "fixed-operation-map", "3 passed, 0 failed, 0 skipped."} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	for _, gone := range []string{"raw-body-signature", "fast-2xx", "connection-event-2xx"} {
		if strings.Contains(out, gone) {
			t.Errorf("the output still has %q:\n%s", gone, out)
		}
	}
}

// ---------------------------------------------------------------- one mistake, one failed check

func TestCheckWebhooksEachMistakeFailsOnlyItsCheck(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tweak  func(a *webhookApp)
		failed []string
		detail string
		got    string
	}{
		{"accepts a bad signature", func(a *webhookApp) { a.skipHMAC = true }, []string{checkBadSignature}, "didn't check the signature", "200"},
		{"accepts a stale timestamp", func(a *webhookApp) { a.skipAge = true }, []string{checkStale}, "replayed", "200"},
		{"200s an unknown slug", func(a *webhookApp) { a.anySlug = true }, []string{checkUnknownSlug}, "fixed map", "200"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newWebhookApp()
			tc.tweak(app)
			target := serveWebhookApp(t, app)
			c, _ := checkCtx(t, true)

			rep, ok, _, _ := runChecks(t, c, target)

			if ok || rep.OK {
				t.Error("the report says it passed")
			}
			if got := failedChecks(rep); strings.Join(got, ",") != strings.Join(tc.failed, ",") {
				t.Fatalf("failed %v, want %v: %+v", got, tc.failed, rep.Checks)
			}
			failed := resultOf(rep, tc.failed[0])
			if failed.Pass == nil || *failed.Pass || !strings.Contains(failed.Detail, tc.detail) || !strings.HasPrefix(failed.Got, tc.got) {
				t.Errorf("failed check = %+v", failed)
			}
			if rep.Summary.Failed != 1 || rep.Summary.Passed != 2 {
				t.Errorf("summary = %+v", rep.Summary)
			}
			for _, id := range allChecks {
				if id != tc.failed[0] && resultOf(rep, id).Detail != "" {
					t.Errorf("%s passed and still has a detail: %q", id, resultOf(rep, id).Detail)
				}
			}
		})
	}
}

// An app whose webhook secret isn't this app's refuses everything signed rightly, and the
// answer says so rather than blaming the body.
func TestCheckWebhooksAWrongSecretIsNamedAsSuch(t *testing.T) {
	app := newWebhookApp()
	app.secret = "whsec_another_apps_secret"
	target := serveWebhookApp(t, app)
	c, _ := checkCtx(t, true)

	rep, _, _, _ := runChecks(t, c, target)

	want := []string{checkUnknownSlug}
	if got := failedChecks(rep); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("failed %v, want %v", got, want)
	}
	for _, id := range want {
		if d := resultOf(rep, id).Detail; !strings.Contains(d, "QUBE_WEBHOOK_SECRET") {
			t.Errorf("%s detail = %q", id, d)
		}
	}
}

// An app that routes before it verifies answers 404 to the wrong signature and the stale
// timestamp, and the answer says to pass --operation. With the right one they pass.
func TestCheckWebhooksARouterFirstAppIsToldToPassAnOperation(t *testing.T) {
	app := newWebhookApp()
	app.routeFirst = true
	target := serveWebhookApp(t, app)
	c, _ := checkCtx(t, true)

	rep, ok, _, _ := runChecks(t, c, strings.Replace(target, "/webhooks/qube", "/webhooks/qube/elsewhere", 1))

	if ok {
		t.Fatalf("passed: %+v", rep.Checks)
	}
	for _, id := range []string{checkBadSignature, checkStale} {
		r := resultOf(rep, id)
		if r.Result != "fail" || r.Status != 404 || !strings.Contains(r.Detail, "--operation SLUG") || !strings.Contains(r.Detail, "answered 404 before checking the signature") {
			t.Errorf("%s = %+v", id, r)
		}
	}
}

// ---------------------------------------------------------------- no answer, or a redirect

func TestCheckWebhooksReportsARedirectAndDoesNotFollowIt(t *testing.T) {
	var loginHits int32
	login := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&loginHits, 1) }))
	t.Cleanup(login.Close)
	app := newWebhookApp()
	app.redirectAll = login.URL + "/login"
	target := serveWebhookApp(t, app)
	c, _ := checkCtx(t, true)

	rep, ok, _, _ := runChecks(t, c, target)

	if ok || rep.Summary.Failed != 3 {
		t.Fatalf("ok=%v summary=%+v", ok, rep.Summary)
	}
	for _, r := range rep.Checks {
		if r.Status != 302 || r.Got != "redirect 302 to "+login.URL+"/login" || !strings.Contains(r.Detail, "never follows") {
			t.Errorf("%s = %+v", r.Check, r)
		}
	}
	if n := atomic.LoadInt32(&loginHits); n != 0 {
		t.Errorf("the redirect was followed %d times", n)
	}
	if n := len(app.deliveries()); n != 3 {
		t.Errorf("the app saw %d requests, want 3", n)
	}
}

func TestCheckWebhooksReportsAConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	target := srv.URL + "/webhooks/qube"
	srv.Close()
	c, _ := checkCtx(t, true)

	rep, ok, _, _ := runChecks(t, c, target)

	if ok || rep.Summary.Failed != 3 {
		t.Fatalf("ok=%v summary=%+v", ok, rep.Summary)
	}
	for _, r := range rep.Checks {
		if r.Got != "connection refused" || r.Status != 0 || r.ElapsedMS != nil || !strings.Contains(r.Detail, "is the app running") {
			t.Errorf("%s = %+v", r.Check, r)
		}
	}
}

func TestCheckWebhooksGivesUpOnAnAppThatDoesNotAnswer(t *testing.T) {
	withLimits(t, 150*time.Millisecond, 0)
	app := newWebhookApp()
	app.sleepAll = 500 * time.Millisecond
	target := serveWebhookApp(t, app)
	c, _ := checkCtx(t, true)

	rep, ok, _, _ := runChecks(t, c, target)

	if ok || rep.Summary.Failed != 3 || rep.Summary.Skipped != 0 {
		t.Fatalf("ok=%v summary=%+v", ok, rep.Summary)
	}
	for _, id := range []string{checkBadSignature, checkStale, checkUnknownSlug} {
		r := resultOf(rep, id)
		if r.Got != "no answer in 150ms" || !strings.Contains(r.Detail, "waits") {
			t.Errorf("%s = %+v", id, r)
		}
	}
}

// ---------------------------------------------------------------- the secret

// Whatever an app answers -- a debug page quoting its environment, a redirect with the secret in
// its Location -- none of it, and no part of the secret, reaches the output, the --json report,
// stderr or a file.
func TestCheckWebhooksNeverShowsTheSecret(t *testing.T) {
	wd, _ := os.Getwd()
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	for _, asJSON := range []bool{false, true} {
		app := newWebhookApp()
		app.echoSecret = true
		app.anySlug = true
		target := serveWebhookApp(t, app)
		c, _ := checkCtx(t, true)
		setJSON(t, asJSON)

		var out, errOut string
		var ok bool
		errOut = captureStderr(t, func() {
			out = captureStdout(t, func() { ok = c.checkWebhooks([]string{target}) })
		})
		if ok {
			t.Fatalf("json=%v: the failures passed", asJSON)
		}
		if n := len(app.deliveries()); n != 3 {
			t.Fatalf("json=%v: the app saw %d requests, so the secret was never quoted to it", asJSON, n)
		}
		everything := out + "\n" + errOut
		if frag := secretFragment(everything); frag != "" {
			t.Errorf("json=%v: the output shows %q of the secret:\n%s", asJSON, frag, everything)
		}
		for _, quoted := range []string{"ENV QUBE", "<pre>", "X-Debug"} {
			if strings.Contains(everything, quoted) {
				t.Errorf("json=%v: an answer's body or header is printed (%q):\n%s", asJSON, quoted, everything)
			}
		}
	}
	if entries, _ := ioutil.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the check wrote %d files", len(entries))
	}
}

// secretFragment is the first run of 6 characters of the test secret that text holds, or "".
func secretFragment(text string) string {
	for i := 0; i+6 <= len(checkTestSecret); i++ {
		if strings.Contains(text, checkTestSecret[i:i+6]) {
			return checkTestSecret[i : i+6]
		}
	}
	return ""
}

func TestWebhookSecretPrintsAsRedacted(t *testing.T) {
	s := webhookSecret(checkTestSecret)
	run := &webhookCheckRun{base: "http://localhost:3000/webhooks/qube", secret: s}
	for _, verb := range []string{"%s", "%v", "%+v", "%#v", "%q", "%x", "%d"} {
		for _, v := range []interface{}{s, &s, run, *run, []webhookSecret{s}, map[string]webhookSecret{"k": s}} {
			if got := fmt.Sprintf(verb, v); secretFragment(got) != "" {
				t.Errorf("%s of %T shows the secret: %s", verb, v, got)
			}
		}
	}
	b, err := json.Marshal(map[string]interface{}{"secret": s, "run": run})
	if err != nil || secretFragment(string(b)) != "" {
		t.Errorf("json = %s, %v", b, err)
	}
	if s.reveal() != checkTestSecret {
		t.Error("reveal changed the secret")
	}
	if got := run.clean("the app said " + checkTestSecret + " twice " + checkTestSecret); secretFragment(got) != "" {
		t.Errorf("clean left %q", got)
	}
}

// ---------------------------------------------------------------- signing and bodies

// The signature is the server's (Qube.WebhookSender.signature_header/3): t=<unix>,v1=<hex
// HMAC-SHA256 of "<t>.<raw body>">. The expected value is from openssl and Python, not this code.
func TestSignatureHeaderMatchesTheServers(t *testing.T) {
	got := signatureHeader(1792350252, []byte(`{"event":"x"}`), "whsec_test_key")
	want := "t=1792350252,v1=66c3bbf94f8b3c771f45e21d21d6026b2a9f3b241bac884404fe78bac5812428"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestWrongV1ChangesOnlyTheLastDigit(t *testing.T) {
	good := signatureHeader(1792350252, []byte("{}"), "k")
	bad := wrongV1(good)
	if bad == good || len(bad) != len(good) || bad[:len(bad)-1] != good[:len(good)-1] {
		t.Fatalf("good %s, wrong %s", good, bad)
	}
	zero := "t=1,v1=00"
	if wrongV1(zero) == zero {
		t.Error("a signature ending in 0 stayed the same")
	}
}

func testRun() *webhookCheckRun {
	return &webhookCheckRun{host: "https://qubesync.com", secret: webhookSecret(checkTestSecret),
		app: apiAppForTest()}
}

// The run event is compact, with its keys in order, as the server writes it.
func TestRunEventSurvivesReserialization(t *testing.T) {
	r := testRun()
	body := r.runEvent(time.Now())
	var v map[string]interface{}
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	again, _ := json.Marshal(v)
	if !bytes.Equal(again, body) {
		t.Errorf("the run event changes when it is parsed and written out again:\n%s\n%s", body, again)
	}
}

// Every check has a rule, and they are the rules the integration rules name, when the QuBe Sync
// repo is checked out next to this one.
func TestCheckRulesAreTheIntegrationRulesLiveIds(t *testing.T) {
	data, err := ioutil.ReadFile("../../../qube/priv/integration_rules/rules.json")
	if err != nil {
		t.Skip("the QuBe Sync repo isn't next to this one")
	}
	var file struct {
		Rules []struct {
			ID   string `json:"id"`
			Live string `json:"live"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	live := map[string]string{}
	for _, r := range file.Rules {
		if r.Live != "" {
			live[r.Live] = r.ID
		}
	}
	if len(checkRules) != 3 || len(allChecks) != 3 {
		t.Errorf("qube check webhooks has %d checks and %d rules, want 3", len(allChecks), len(checkRules))
	}
	for _, id := range allChecks {
		if checkRules[id] == "" || live[id] != checkRules[id] {
			t.Errorf("check %s is for rule %q; the rules say %q", id, checkRules[id], live[id])
		}
	}
	for id := range live {
		if _, ok := checkRules[id]; !ok {
			t.Errorf("the rules have a live check %q that qube check doesn't make", id)
		}
	}
}

// ---------------------------------------------------------------- where it may send

func TestRefusesQuBeSyncsOwnHost(t *testing.T) {
	for _, tc := range []struct {
		host, target string
		refused      bool
		relay        bool
	}{
		{"https://qubesync.com", "https://qubesync.com/webhooks/qube", true, false},
		{"https://qubesync.com", "https://QUBESYNC.com:443/x", true, false},
		{"https://qubesync.com", "http://qubesync.com:8080/x", true, false},
		{"https://qubesync.com", "https://qubesync.com/relay/rl_4f7k2m9q8t3w6x1z5c0v7b2n4h/runs", true, true},
		{"https://qubesync.com", "https://example.com/relay/rl_abc", false, false},
		{"https://qubesync.com", "http://localhost:3000/webhooks/qube", false, false},
		{"http://localhost:4002", "http://localhost:3000/webhooks/qube", false, false},
		{"http://localhost:4002", "http://localhost:4002/webhooks/qube", true, false},
		{"http://localhost:4002", "http://127.0.0.1:4002/webhooks/qube", true, false},
		{"http://localhost:4002", "http://localhost:4002/relay/rl_x", true, true},
		{"http://127.0.0.1:55000", "http://[::1]:55000/x", true, false},
		{"http://127.0.0.1:55000", "http://[::1]:3000/x", false, false},
	} {
		target, err := parseCheckURL(tc.target)
		if err != nil {
			t.Fatalf("%s: %v", tc.target, err)
		}
		err = refuseOwnHost(tc.host, target)
		if (err != nil) != tc.refused {
			t.Errorf("%s from %s: err = %v, want refused=%v", tc.target, tc.host, err, tc.refused)
			continue
		}
		if err != nil && tc.relay != strings.Contains(err.Error(), "qube listen") {
			t.Errorf("%s from %s: relay=%v, message %q", tc.target, tc.host, tc.relay, err)
		}
	}
}

func TestLocalTargetsNeedNoYes(t *testing.T) {
	for raw, local := range map[string]bool{
		"http://localhost:3000/x": true, "http://127.0.0.1:3000/x": true, "http://[::1]:3000/x": true,
		"http://127.0.0.2/x": true, "https://LOCALHOST/x": true,
		"https://example.com/x": false, "http://192.168.1.5:3000/x": false, "http://0.0.0.0:3000/x": false,
		"http://app.localhost:3000/x": false, "https://qubesync.com/relay/rl_x": false,
	} {
		u, err := parseCheckURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := localTarget(u); got != local {
			t.Errorf("%s: local = %v, want %v", raw, got, local)
		}
	}
}

func TestParseCheckURL(t *testing.T) {
	u, err := parseCheckURL("localhost:3000/webhooks/qube#frag")
	if err != nil || u.String() != "http://localhost:3000/webhooks/qube" {
		t.Errorf("got %v, %v", u, err)
	}
	for _, bad := range []string{"ftp://localhost/x", "http://", "http://localhost:3000/x?a=1", "http://localhost:3000/x?", "://x", ""} {
		if _, err := parseCheckURL(bad); err == nil {
			t.Errorf("%q was taken", bad)
		}
	}
	u, _ = parseCheckURL("https://user:hunter2@example.com/webhooks/qube")
	if strings.Contains(u.Redacted(), "hunter2") {
		t.Errorf("redacted = %s", u.Redacted())
	}
}

// ---------------------------------------------------------------- the guards, in-process

// A production app is checked only with --production.
func TestCheckWebhooksProductionWithTheFlag(t *testing.T) {
	app := newWebhookApp()
	target := serveWebhookApp(t, app)
	c, hits := checkCtx(t, false)

	rep, ok, _, _ := runChecks(t, c, target, "--production")

	if !ok || rep.App.Sandbox || rep.Summary.Passed != 3 {
		t.Fatalf("ok=%v report=%+v", ok, rep)
	}
	if atomic.LoadInt32(hits) != 1 {
		t.Errorf("the secret was fetched %d times", atomic.LoadInt32(hits))
	}
}

// A URL that isn't on this machine goes ahead with --yes, and says it is sending there.
func TestCheckWebhooksRemoteURLWithYes(t *testing.T) {
	app := newWebhookApp()
	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")
	checkDial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	t.Cleanup(func() { checkDial = nil })
	c, _ := checkCtx(t, true)
	c.yes = true

	rep, ok, _, errOut := runChecks(t, c, "http://hooks.example.test:8080/webhooks/qube")

	if !ok || rep.Summary.Passed != 3 {
		t.Fatalf("ok=%v summary=%+v", ok, rep.Summary)
	}
	if !strings.Contains(errOut, "hooks.example.test:8080/webhooks/qube isn't on this machine") {
		t.Errorf("no warning: %q", errOut)
	}
	if seen := app.deliveries(); len(seen) != 3 || seen[0].host != "hooks.example.test:8080" {
		t.Errorf("the app saw %d requests", len(seen))
	}
}

// ---------------------------------------------------------------- the command line, as a process

// The refusals end the process, so each case runs this test binary again as a child pointed at
// a fake QuBe Sync the parent watches.
func TestCheckWebhooksCommandLine(t *testing.T) {
	if host := os.Getenv("QUBE_TEST_CHECK_HOST"); host != "" {
		session := config.Session{Token: "qct_x"}
		switch os.Getenv("QUBE_TEST_CHECK_MODE") {
		case "readonly":
			session.Access = "read_only"
		case "actas":
			session.ActingAs = &config.ActAs{Token: "qct_grant", Organization: "Acme Corp", Access: "read_write", Scope: "sandbox"}
		}
		c := &ctx{
			host: host,
			cfg:  &config.File{Sessions: map[string]config.Session{host: session}},
			bg:   context.Background(),
			yes:  os.Getenv("QUBE_TEST_CHECK_YES") != "",
		}
		ui.JSON = os.Getenv("QUBE_TEST_CHECK_JSON") != ""
		c.check(strings.Split(os.Getenv("QUBE_TEST_CHECK_ARGS"), " "))
		os.Exit(0)
	}

	runChild := func(t *testing.T, host, args string, env ...string) (stdout, stderr string, code int) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestCheckWebhooksCommandLine$")
		cmd.Env = append(append(os.Environ(), "QUBE_TEST_CHECK_HOST="+host, "QUBE_TEST_CHECK_ARGS="+args), env...)
		var so, se bytes.Buffer
		cmd.Stdout, cmd.Stderr = &so, &se
		err := cmd.Run()
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return so.String(), se.String(), code
	}

	t.Run("refusals send nothing and fetch no secret", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			sandbox  bool
			args     func(host, app string) string
			env      []string
			wantCode int
			wantErr  string
		}{
			{"QuBe Sync's own host", true, func(host, app string) string { return "webhooks " + host + "/webhooks/qube" }, nil, ui.ExitFailure, "QuBe Sync's own host"},
			{"a relay URL", true, func(host, app string) string { return "webhooks " + host + "/relay/rl_4f7k2m9q8t3w6x1z5c0v7b2n4h" }, nil, ui.ExitFailure, "qube listen"},
			{"a production app without --production", false, func(host, app string) string { return "webhooks " + app }, nil, ui.ExitUsage, "--production"},
			{"a URL that isn't on this machine", true, func(host, app string) string { return "webhooks http://hooks.example.test:8080/webhooks/qube" }, nil, ui.ExitUsage, "--yes"},
			{"a remote URL under --json", true, func(host, app string) string { return "webhooks https://hooks.example.test/webhooks/qube" }, []string{"QUBE_TEST_CHECK_JSON=1"}, ui.ExitUsage, "--yes"},
			{"a read-only session", true, func(host, app string) string { return "webhooks " + app }, []string{"QUBE_TEST_CHECK_MODE=readonly"}, ui.ExitFailure, "read-only"},
			{"an act-as grant", true, func(host, app string) string { return "webhooks " + app }, []string{"QUBE_TEST_CHECK_MODE=actas"}, ui.ExitFailure, "acting as Acme Corp"},
			{"no URL", true, func(host, app string) string { return "webhooks" }, nil, ui.ExitUsage, "usage: qube check webhooks"},
			{"a URL that isn't http", true, func(host, app string) string { return "webhooks ftp://localhost/x" }, nil, ui.ExitUsage, "http://"},
			{"a URL with a query", true, func(host, app string) string { return "webhooks http://localhost:3000/x?a=1" }, nil, ui.ExitUsage, "query"},
			{"an operation with a slash", true, func(host, app string) string { return "webhooks http://localhost:3000/x --operation a/b" }, nil, ui.ExitUsage, "--operation"},
			{"another check", true, func(host, app string) string { return "bogus" }, nil, ui.ExitUsage, "unknown check"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				app := newWebhookApp()
				appURL := serveWebhookApp(t, app)
				c, hits := checkCtx(t, tc.sandbox)

				_, stderr, code := runChild(t, c.host, tc.args(c.host, appURL), tc.env...)

				if code != tc.wantCode || !strings.Contains(stderr, tc.wantErr) {
					t.Errorf("exit %d (want %d), stderr:\n%s", code, tc.wantCode, stderr)
				}
				if n := atomic.LoadInt32(hits); n != 0 {
					t.Errorf("the secret was fetched %d times", n)
				}
				if n := len(app.deliveries()); n != 0 {
					t.Errorf("the app was sent %d requests", n)
				}
				if secretFragment(stderr) != "" {
					t.Errorf("stderr shows the secret: %s", stderr)
				}
			})
		}
	})

	t.Run("a failed check exits 1, a clean run exits 0", func(t *testing.T) {
		app := newWebhookApp()
		app.skipHMAC = true
		appURL := serveWebhookApp(t, app)
		c, _ := checkCtx(t, true)

		stdout, stderr, code := runChild(t, c.host, "webhooks "+appURL)
		if code != ui.ExitFailure || !strings.Contains(stdout, "FAIL") || !strings.Contains(stdout, "bad-signature-401") || !strings.Contains(stdout, "1 failed") {
			t.Errorf("exit %d\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
		}

		stdout, _, code = runChild(t, c.host, "webhooks "+appURL, "QUBE_TEST_CHECK_JSON=1")
		var rep webhookCheckReport
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil || code != ui.ExitFailure || rep.OK || rep.Summary.Failed != 1 {
			t.Errorf("exit %d, %v\n%s", code, err, stdout)
		}

		good := newWebhookApp()
		goodURL := serveWebhookApp(t, good)
		stdout, _, code = runChild(t, c.host, "webhooks "+goodURL+" --operation orders")
		if code != 0 || !strings.Contains(stdout, "3 passed, 0 failed, 0 skipped.") {
			t.Errorf("exit %d\n%s", code, stdout)
		}
	})

	t.Run("flags may come before the URL", func(t *testing.T) {
		good := newWebhookApp()
		goodURL := serveWebhookApp(t, good)
		c, _ := checkCtx(t, true)
		stdout, _, code := runChild(t, c.host, "webhooks --operation orders "+goodURL)
		if code != 0 || !strings.Contains(stdout, "3 passed") {
			t.Errorf("exit %d\n%s", code, stdout)
		}
	})
}

// ---------------------------------------------------------------- completion and help

func TestCheckIsInCompletionAndHelp(t *testing.T) {
	if !contains(commandTree[""], "check") {
		t.Error("completion's top level lacks check")
	}
	for _, w := range []string{"webhooks", "--operation", "--production"} {
		if !contains(commandTree["check"], w) {
			t.Errorf("completion's check lacks %s", w)
		}
	}
	setenv(t, "QUBE_CONFIG", t.TempDir()+"/credentials.json")
	help := captureStdout(t, usage)
	for _, want := range []string{"check webhooks URL [--operation SLUG] [--production]", "--yes"} {
		if !strings.Contains(help, want) {
			t.Errorf("qube help lacks %q", want)
		}
	}
}

func apiAppForTest() api.App {
	return api.App{ID: "app1", Name: "App", Sandbox: true}
}
