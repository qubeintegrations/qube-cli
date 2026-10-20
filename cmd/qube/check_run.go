package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
)

// The checks `qube check webhooks` makes, how each delivery is built and signed, and how the
// answers are graded. See check.go for the command and the rules it keeps.

// The limits, as variables so tests can shorten them.
var (
	checkRequestTimeout = 10 * time.Second // a request that gets no answer by then fails
	checkSlowAfter      = 5 * time.Second  // QuBe Sync waits 5 s for a 2xx (Qube.WebhookSender)
	checkStaleBy        = 10 * time.Minute // how old the stale-timestamp check's `t` is; receivers allow 5 minutes

	// checkDial replaces the dialer in tests, so a made-up host name reaches a test server.
	checkDial func(ctx context.Context, network, addr string) (net.Conn, error)
)

// webhookSecret is the app's webhook secret. It is held in memory for the run and never shown:
// every way of printing one (%s, %v, %q, %+v, %#v, JSON) says so instead.
type webhookSecret string

func (s webhookSecret) Format(f fmt.State, verb rune) { fmt.Fprint(f, "[webhook secret]") }
func (s webhookSecret) MarshalJSON() ([]byte, error)  { return []byte(`"[webhook secret]"`), nil }
func (s webhookSecret) reveal() string                { return string(s) }

// webhookCheckRun is one run of the checks against a base URL. Printing one says what it checks
// and never shows its secret: fmt can't call a field's own Format, so the run has its own.
type webhookCheckRun struct {
	base      string // the URL checked, without a trailing slash
	host      string // QuBe Sync's host: the links in the bodies point at it
	app       api.App
	secret    webhookSecret
	operation string // --operation: a slug the app's route has
	record    string // --record: the id of the app's record that keeps a connection, for the connection route
	now       func() time.Time
}

func (r webhookCheckRun) Format(f fmt.State, verb rune) {
	fmt.Fprintf(f, "webhookCheckRun(%s, secret %s)", r.shownBase(), r.secret)
}

// webhookCheck is one request: what is sent, and what it has to be answered with.
type webhookCheck struct {
	id        string
	sent      string // what the delivery is, in a phrase
	path      string // added to the base URL
	body      []byte
	signature string // X-QUBE-Signature
	expected  string
	skip      string // why nothing is sent, when nothing is
	passes    func(o checkOutcome) bool
}

// checkOutcome is how the app answered one request.
type checkOutcome struct {
	status   int           // 0 when there was no answer
	elapsed  time.Duration // until the answer's headers arrived
	location string        // where a redirect pointed
	err      string        // why there was no answer
}

func (o checkOutcome) answered() bool { return o.status != 0 }
func (o checkOutcome) redirect() bool { return o.status >= 300 && o.status < 400 }
func (o checkOutcome) twoXX() bool    { return o.status >= 200 && o.status < 300 }
func (o checkOutcome) serverError() bool {
	return o.status >= 500
}

// within says whether the answer is a 2xx that arrived before QuBe Sync would stop waiting.
func (o checkOutcome) within(limit time.Duration) bool { return o.twoXX() && o.elapsed <= limit }

func statusIs(want int) func(checkOutcome) bool {
	return func(o checkOutcome) bool { return o.status == want }
}

// The checks' ids. They are the `live` ids the integration rules cite.
const (
	checkBadSignature  = "bad-signature-401"
	checkStale         = "stale-timestamp-401"
	checkRawBody       = "raw-body-signature"
	checkUnknownSlug   = "unknown-operation-404"
	checkFast          = "fast-2xx"
	checkConnectionEvt = "connection-event-2xx"
)

// checkRules is the integration rule each check tests: the rule whose `live` id the check's is
// (priv/integration_rules/rules.json in the QuBe Sync repo; a test reads it when it is checked out
// next to this one).
var checkRules = map[string]string{
	checkBadSignature:  "verify-signature",
	checkStale:         "refuse-stale-timestamp",
	checkRawBody:       "verify-raw-body",
	checkUnknownSlug:   "fixed-operation-map",
	checkFast:          "answer-fast-work-in-job",
	checkConnectionEvt: "connection-event-for-saved-connection",
}

// unknownSlugPath is where the unknown-operation check sends: a slug no app has.
const unknownSlugPath = "/qube-check-unknown/1"

func (r *webhookCheckRun) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

// opPath is the route of a run event for the --operation slug, or the unknown slug's when none
// was given: an app that verifies before it routes (the kit does) answers either the same.
func (r *webhookCheckRun) opPath(n int) string {
	if r.operation == "" {
		return unknownSlugPath
	}
	return fmt.Sprintf("/%s/qube-check-%d", url.PathEscape(r.operation), n)
}

// checks builds the six checks, in the order they run and print.
func (r *webhookCheckRun) checks() []*webhookCheck {
	now := r.clock()
	stale := now.Add(-checkStaleBy)
	slow := checkSlowAfter

	runEvent := r.runEvent(now)
	staleEvent := r.runEvent(stale)
	raw := r.rawRunEvent(now)
	connEvent := r.connectionEvent(now)
	// the wrong v1 is the right one with its last digit changed: a receiver that compares only
	// a prefix of the signature takes it
	wrong := wrongV1(r.sign(now, runEvent))
	needOperation := ""
	if r.operation == "" {
		needOperation = "pass --operation SLUG, one of the slugs your webhook route has (QUBE_WEBHOOK_URL/SLUG/RECORD_ID), to send to a route the app has"
	}
	connPath := ""
	connSkip := "pass --record ID, the id of the record in your app that keeps a connection, to send a connection event to QUBE_WEBHOOK_URL/connection/ID"
	if r.record != "" {
		connPath = "/connection/" + url.PathEscape(r.record)
		connSkip = ""
	}

	checks := []*webhookCheck{
		{
			id: checkBadSignature, sent: "wrong v1", path: r.opPath(1), body: runEvent, signature: wrong,
			expected: "401", passes: statusIs(401),
		},
		{
			id: checkStale, sent: fmt.Sprintf("t %d min old", int(checkStaleBy/time.Minute)), path: r.opPath(2), body: staleEvent,
			signature: r.sign(stale, staleEvent), expected: "401", passes: statusIs(401),
		},
		{
			id: checkRawBody, sent: `odd spacing, \u escapes`, path: r.opPath(3), body: raw, signature: r.sign(now, raw),
			expected: "2xx", skip: needOperation, passes: func(o checkOutcome) bool { return o.twoXX() },
		},
		{
			id: checkUnknownSlug, sent: "signed, unknown slug", path: unknownSlugPath, body: runEvent, signature: r.sign(now, runEvent),
			expected: "404", passes: statusIs(404),
		},
		{
			id: checkFast, sent: "signed run event", path: r.opPath(5), body: runEvent, signature: r.sign(now, runEvent),
			expected: "2xx in " + slow.String(), skip: needOperation, passes: func(o checkOutcome) bool { return o.within(slow) },
		},
		{
			id: checkConnectionEvt, sent: "signed connection.connected", path: connPath, body: connEvent, signature: r.sign(now, connEvent),
			expected: "2xx in " + slow.String(), skip: connSkip, passes: func(o checkOutcome) bool { return o.within(slow) },
		},
	}
	return checks
}

// run sends each check, one request apiece and in order, and grades the answers. It fails only
// when ctx ends (Ctrl-C): an app that can't be reached is a failed check, not an error.
func (r *webhookCheckRun) run(ctx context.Context) (*webhookCheckReport, error) {
	client := r.client()
	report := &webhookCheckReport{
		URL:    r.clean(r.shownBase()),
		App:    webhookCheckApp{ID: r.app.ID, Name: r.app.Name, Sandbox: r.app.Sandbox},
		Checks: []webhookCheckResult{},
	}
	outcomes := map[string]checkOutcome{}
	for _, ck := range r.checks() {
		res := webhookCheckResult{Check: ck.id, Rule: checkRules[ck.id]}
		if ck.skip != "" {
			res.Result, res.Detail = "skipped", "skipped: "+ck.skip
			report.Checks = append(report.Checks, res)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o := r.send(ctx, client, ck)
		if o.err == "canceled" {
			return nil, context.Canceled
		}
		outcomes[ck.id] = o
		pass := ck.passes(o)
		res.Sent = ck.sent
		res.Path = ck.path
		res.Expected = ck.expected
		res.Got = r.clean(r.got(o))
		res.Status = o.status
		if o.answered() {
			ms := o.elapsed.Milliseconds()
			res.ElapsedMS = &ms
		}
		res.Pass = &pass
		res.Result = "pass"
		if !pass {
			res.Result = "fail"
		}
		report.Checks = append(report.Checks, res)
	}
	r.explain(report, outcomes)
	for _, res := range report.Checks {
		switch res.Result {
		case "pass":
			report.Summary.Passed++
		case "fail":
			report.Summary.Failed++
		default:
			report.Summary.Skipped++
		}
	}
	report.OK = report.Summary.Failed == 0
	return report, nil
}

// shownBase is the URL with any password blanked.
func (r *webhookCheckRun) shownBase() string {
	if u, err := url.Parse(r.base); err == nil {
		return u.Redacted()
	}
	return r.base
}

// clean keeps the secret out of anything printed, whatever an answer or an error quoted.
func (r *webhookCheckRun) clean(s string) string {
	if secret := r.secret.reveal(); secret != "" {
		s = strings.ReplaceAll(s, secret, "[webhook secret]")
	}
	return s
}

// client never follows a redirect, takes no cookies and opens a connection for each request.
func (r *webhookCheckRun) client() *http.Client {
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: checkRequestTimeout}).DialContext,
		TLSHandshakeTimeout: checkRequestTimeout,
		DisableKeepAlives:   true,
	}
	if checkDial != nil {
		tr.DialContext, tr.Proxy = checkDial, nil
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       checkRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// send makes one check's request. The answer's body is read no further than needed to close
// the connection and never kept: an app's error page can quote its environment.
func (r *webhookCheckRun) send(ctx context.Context, client *http.Client, ck *webhookCheck) checkOutcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+ck.path, bytes.NewReader(ck.body))
	if err != nil {
		return checkOutcome{err: "can't build the request: " + r.clean(err.Error())}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-QUBE-Signature", ck.signature)
	req.Header.Set("X-QUBE-Check", ck.id) // so the app's log says what these are
	req.Header.Set("User-Agent", "qube-cli/"+api.Version+" (qube check webhooks)")
	start := time.Now()
	resp, err := client.Do(req)
	elapsed := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			return checkOutcome{err: "canceled"}
		}
		return checkOutcome{elapsed: elapsed, err: r.describeError(err)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return checkOutcome{status: resp.StatusCode, elapsed: elapsed, location: printableLine(resp.Header.Get("Location"), 120)}
}

// describeError says why a request got no answer, in words that don't depend on the platform.
func (r *webhookCheckRun) describeError(err error) string {
	var netErr net.Error
	var dnsErr *net.DNSError
	text := strings.ToLower(err.Error())
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "no answer in " + checkRequestTimeout.String()
	case errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(text, "connection refused") || strings.Contains(text, "actively refused"):
		return "connection refused"
	case errors.As(err, &dnsErr):
		return "host not found"
	case strings.Contains(text, "x509") || strings.Contains(text, "tls:"):
		return "TLS failed: " + printableLine(r.clean(innermost(err).Error()), 100)
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || strings.Contains(text, "connection reset"):
		return "connection closed without an answer"
	}
	return "no answer: " + printableLine(r.clean(innermost(err).Error()), 100)
}

func innermost(err error) error {
	for {
		inner := errors.Unwrap(err)
		if inner == nil {
			return err
		}
		err = inner
	}
}

// printableLine is s on one line, without control characters, cut to n characters.
func printableLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, s)
	if r := []rune(s); len(r) > n {
		s = string(r[:n]) + "..."
	}
	return s
}

// got is the answer in a table cell.
func (r *webhookCheckRun) got(o checkOutcome) string {
	switch {
	case !o.answered():
		return o.err
	case o.redirect():
		to := ""
		if o.location != "" {
			to = " to " + o.location
		}
		return fmt.Sprintf("redirect %d%s", o.status, to)
	}
	return fmt.Sprintf("%d in %s", o.status, formatElapsed(o.elapsed))
}

// ---------------------------------------------------------------- what failed, in words

const secretHint = "the app refused a correctly signed delivery: its QUBE_WEBHOOK_SECRET may not be this app's webhook secret (`qube env --write .env` writes it)"

// explain gives each failed check its reason. A correctly signed delivery refused with 401 is
// one of two mistakes: an app whose secret isn't this app's refuses every one, and an app that
// verifies a re-serialized body refuses only the one whose formatting a parser doesn't keep.
func (r *webhookCheckRun) explain(report *webhookCheckReport, outcomes map[string]checkOutcome) {
	signedOK := false // some correctly signed, ordinarily formatted delivery was not refused
	for _, id := range []string{checkUnknownSlug, checkFast, checkConnectionEvt} {
		if o, ran := outcomes[id]; ran && o.answered() && o.status != 401 {
			signedOK = true
		}
	}
	for i := range report.Checks {
		res := &report.Checks[i]
		if res.Result != "fail" {
			continue
		}
		o := outcomes[res.Check]
		res.Detail = r.clean(r.reason(res.Check, o, signedOK))
	}
}

func (r *webhookCheckRun) reason(id string, o checkOutcome, signedOK bool) string {
	switch {
	case !o.answered():
		hint := ""
		if o.err == "connection refused" {
			hint = ": is the app running at that URL?"
		}
		if strings.HasPrefix(o.err, "no answer in") {
			hint = fmt.Sprintf(": QuBe Sync waits %s for a 2xx, so a delivery to this app would fail", checkSlowAfter)
		}
		return o.err + hint
	case o.redirect():
		return fmt.Sprintf("the app answered %d, a redirect (%s). QuBe Sync never follows one, so a delivery to this URL would fail: "+
			"serve the webhook route without a login or https redirect.", o.status, orElse(o.location, "no Location"))
	}
	s := o.status
	routed := r.operation != ""
	switch id {
	case checkBadSignature:
		switch {
		case o.twoXX():
			return fmt.Sprintf("answered %d to a delivery signed wrongly: the app didn't check the signature, or checked it and went on. Verify X-QUBE-Signature on the raw body first, and answer 401 when it fails.", s)
		case s == 404 && !routed:
			return "answered 404 before checking the signature, since no route matches /qube-check-unknown/1. Pass --operation SLUG so the delivery reaches a route the app has."
		}
		return fmt.Sprintf("answered %d; a delivery signed wrongly should be refused with 401.", s)
	case checkStale:
		switch {
		case o.twoXX():
			return fmt.Sprintf("answered %d to a correctly signed delivery whose timestamp is %d minutes old. Refuse a `t` more than 5 minutes from now, or a captured delivery can be replayed.", s, int(checkStaleBy/time.Minute))
		case s == 404 && !routed:
			return "answered 404 before checking the signature, since no route matches /qube-check-unknown/1. Pass --operation SLUG so the delivery reaches a route the app has."
		}
		return fmt.Sprintf("answered %d; a stale timestamp should be refused with 401.", s)
	case checkRawBody:
		switch {
		case s == 401 && signedOK:
			return "refused a correctly signed delivery whose JSON has unusual spacing, key order and \\u escapes, though it took ordinary ones: it verifies a parsed and re-serialized body. Sign-check the body exactly as received, before anything parses it."
		case s == 401:
			return secretHint + "."
		case s == 404:
			return fmt.Sprintf("answered 404 for operation %q: is that one of the slugs the app routes?", r.operation)
		}
		return fmt.Sprintf("answered %d to a correctly signed run event; a 2xx was expected.", s)
	case checkUnknownSlug:
		switch {
		case o.twoXX():
			return fmt.Sprintf("answered %d for a slug none of its operations has. Look the slug up in a fixed map, and answer 404 for any other.", s)
		case s == 401:
			return secretHint + "."
		}
		return fmt.Sprintf("answered %d; an unknown operation should be answered 404.", s)
	case checkFast:
		return r.fastReason(o, r.opPath(5), "a signed run event")
	case checkConnectionEvt:
		return r.fastReason(o, "/connection/"+url.PathEscape(r.record), "a connection event for a connection that isn't the record's, which it should log and skip")
	}
	return fmt.Sprintf("answered %d.", s)
}

// fastReason is why a run event or a connection event wasn't answered with a 2xx in time.
func (r *webhookCheckRun) fastReason(o checkOutcome, path, what string) string {
	switch {
	case o.twoXX():
		return fmt.Sprintf("answered %d, but after %s, and QuBe Sync waits %s. Verify, enqueue a job and answer; do the work in the job.", o.status, formatElapsed(o.elapsed), checkSlowAfter)
	case o.status == 401:
		return secretHint + "."
	case o.status == 404:
		return fmt.Sprintf("answered 404 for %s: does the app route it?", path)
	case o.serverError():
		return fmt.Sprintf("answered %d to %s. The request should only verify, check the operation and enqueue; the work, and any failure in it, belongs to the job.", o.status, what)
	}
	return fmt.Sprintf("answered %d to %s; a 2xx was expected.", o.status, what)
}

func orElse(s, otherwise string) string {
	if s == "" {
		return otherwise
	}
	return s
}

// ---------------------------------------------------------------- signing

// sign is the X-QUBE-Signature header as the server makes it (Qube.WebhookSender.signature_header/3):
// t=<unix>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">.
func (r *webhookCheckRun) sign(t time.Time, body []byte) string {
	return signatureHeader(t.Unix(), body, r.secret.reveal())
}

func signatureHeader(t int64, body []byte, secret string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%d.", t)
	mac.Write(body)
	return fmt.Sprintf("t=%d,v1=%s", t, hex.EncodeToString(mac.Sum(nil)))
}

// wrongV1 changes the last hex digit of a header's signature.
func wrongV1(header string) string {
	if header == "" {
		return header
	}
	last := header[len(header)-1]
	swap := byte('0')
	if last == '0' {
		swap = '1'
	}
	return header[:len(header)-1] + string(swap)
}

// ---------------------------------------------------------------- the bodies

// The deliveries are shaped like the ones QuBe Sync sends (priv/openapi/workflows.json's
// workflow_run.completed and connections.json's connection.connected), with ids that say
// qube-check and that no record of the app's will match, so an app that is right skips them.
// An ordinary body is compact JSON with its keys in order, as the server writes them.

const (
	checkConnectionID = "qube-check-connection"
	checkReference    = "qube-check-reference"
)

func (r *webhookCheckRun) links(path string) string { return r.host + path }

func (r *webhookCheckRun) runEvent(t time.Time) []byte {
	return mustJSON(map[string]interface{}{
		"event":         "workflow_run.completed",
		"timestamp":     t.Unix(),
		"app_id":        r.app.ID,
		"sandbox":       r.app.Sandbox,
		"connection_id": checkConnectionID,
		"workflow":      "qube-check-workflow",
		"workflow_id":   nil,
		"run_id":        "qube-check-run",
		"reference":     checkReference,
		"revision":      1,
		"run_state":     "completed",
		"outcome":       "qube-check",
		"output":        map[string]interface{}{},
		"error":         nil,
		"summary": map[string]interface{}{
			"ended_at":     t.UTC().Format(time.RFC3339),
			"stopped_at":   nil,
			"side_effects": []interface{}{},
			"decisions":    []interface{}{},
		},
		"links": map[string]interface{}{
			"run":       r.links("/api/v2/connections/" + checkConnectionID + "/workflow-runs/qube-check-run"),
			"decisions": r.links("/api/v2/connections/" + checkConnectionID + "/workflow-runs/qube-check-run/decisions"),
			"cancel":    r.links("/api/v2/connections/" + checkConnectionID + "/workflow-runs/qube-check-run/cancel"),
			"ui":        r.links("/app/workflow_runs/qube-check-run"),
		},
	})
}

func (r *webhookCheckRun) connectionEvent(t time.Time) []byte {
	return mustJSON(map[string]interface{}{
		"event":       "connection.connected",
		"delivery_id": "qube-check-delivery",
		"revision":    1,
		"timestamp":   t.Unix(),
		"app_id":      r.app.ID,
		"sandbox":     r.app.Sandbox,
		"connection": map[string]interface{}{
			"id":                checkConnectionID,
			"name":              "qube-check",
			"type":              "standard",
			"last_connected_at": t.UTC().Format(time.RFC3339),
			"setup":             "first",
		},
		"links": map[string]interface{}{
			"self": r.links("/api/v2/connections/" + checkConnectionID),
		},
	})
}

// rawRunEvent is the run event again, as no serializer would write it: keys out of order,
// spaces, tabs and newlines in odd places, the slashes of its URLs and a few letters written as
// \u escapes, in upper case, which no serializer does. Parsed and written out again it is a
// different string, so a receiver that checks the signature over what it wrote gets another
// one. It is valid JSON with the same fields as runEvent.
func (r *webhookCheckRun) rawRunEvent(t time.Time) []byte {
	str := func(s string) string { // a JSON string, with its slashes escaped too
		return strings.ReplaceAll(string(mustJSON(s)), "/", `\/`)
	}
	return []byte("{\n" +
		"\t\"links\" : {\"ui\":" + str(r.links("/app/workflow_runs/qube-check-run")) + " ,\n" +
		"\t\t\"run\":  " + str(r.links("/api/v2/connections/"+checkConnectionID+"/workflow-runs/qube-check-run")) + ",\n" +
		"\t\t\"decisions\" :" + str(r.links("/api/v2/connections/"+checkConnectionID+"/workflow-runs/qube-check-run/decisions")) + ",\n" +
		"\t\t\"cancel\":" + str(r.links("/api/v2/connections/"+checkConnectionID+"/workflow-runs/qube-check-run/cancel")) + " },\n" +
		"\t\"reference\"\t:\t" + str(checkReference) + " ,\n" +
		"\t\"event\":\"workflow_run.completed\",   \"revision\" : 1,\n" +
		"\t\"outcome\" : \"caf\\u00E9 \\u2713 \\u0071ube-check\" ,\n" +
		"\t\"run_state\":\"completed\", \"run_id\"  :  \"qube-check-run\",\n" +
		"\t\"timestamp\" :" + fmt.Sprint(t.Unix()) + " ,\n" +
		"\t\"app_id\": " + str(r.app.ID) + ", \"sandbox\" : " + fmt.Sprint(r.app.Sandbox) + ",\n" +
		"\t\"connection_id\":" + str(checkConnectionID) + ",\"workflow\":\"qube-check-workflow\" , \"workflow_id\":null,\n" +
		"\t\"output\":{ }, \"error\" :null,\n" +
		"\t\"summary\" : { \"decisions\":[ ], \"side_effects\" :[ ],\"stopped_at\":null ,\"ended_at\":" + str(t.UTC().Format(time.RFC3339)) + " }\n" +
		"}\n")
}

// mustJSON is v as compact JSON, with the keys of a map in order, as Jason writes a small one.
func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // the values are all strings, numbers and maps of them
	}
	return b
}
