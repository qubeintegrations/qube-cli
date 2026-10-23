package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
	"github.com/qubeintegrations/qube-cli/internal/uuid"
)

// `qube listen --forward-to URL` gives this machine a QuBe-hosted webhook URL for the app (a
// relay), and forwards every delivery QuBe Sync makes to it to the local app: the same body
// and headers, X-QUBE-Signature included, POSTed to the --forward-to URL plus whatever path
// the webhook_url added after the relay's own. It then tells QuBe Sync how the local app
// answered, which is recorded as the delivery's outcome, as for any webhook_url.
//
// The URL is the same every time for one app, one user and one machine: the machine is a
// device id kept in os.UserConfigDir()/qube/device-id (config.DeviceID), shared by every
// config on it. Deliveries come over a long poll (GET .../deliveries, answered at once when
// there is work and 204 after about 25 s), one session id per run; a newer run anywhere for
// the same URL takes over and this one stops. Read-only sessions may listen: they receive
// only webhooks pointed at their own URL, data they can already read.

const listenUsage = "qube listen --forward-to URL [--write-env FILE] [--forget] [--app A] [--json]"

// webhookURLKey is the env variable the app reads its webhook_url from.
const webhookURLKey = "QUBE_WEBHOOK_URL"

// The timings and bounds, as variables so tests can shorten them.
var (
	listenPollDeadline   = 35 * time.Second // a poll is held up to 25 s by the server
	listenBackoffMin     = time.Second      // reconnecting: 1 s, doubling...
	listenBackoffMax     = 30 * time.Second // ...to 30 s
	listenStopTimeout    = 5 * time.Second  // telling the server on Ctrl-C
	listenReportTimeout  = 10 * time.Second // the server waits 15 s from hand-over for a result
	listenConcurrency    = 8                // deliveries forwarded at once
	forwardDialTimeout   = 5 * time.Second  // as QuBe Sync's own webhooks: 5 s to connect...
	forwardHeaderTimeout = 5 * time.Second  // ...and 5 s for the answer
	forwardBodyLimit     = int64(1000)      // bytes of the answer reported
	forwardDetailLimit   = 200              // characters of an error the server takes
)

type listenOptions struct {
	forwardTo *url.URL // nil only with --forget alone
	writeEnv  string
	forget    bool
}

func (c *ctx) listen(args []string) {
	opts := parseListen(args)
	err := c.runListen(opts)
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		os.Exit(ui.ExitInterrupted)
	default:
		ui.Fail("%v", err)
	}
}

func parseListen(args []string) listenOptions {
	fs := flag.NewFlagSet("listen", flag.ExitOnError)
	forwardTo := fs.String("forward-to", "", "the local URL each delivery is POSTed to, plus the path the webhook_url adds (required)")
	writeEnv := fs.String("write-env", "", "write the URL into this env file as "+webhookURLKey+" (e.g. .env); nothing else in it changes")
	forget := fs.Bool("forget", false, "delete this machine's URL for the app first (asks; --yes for a script); alone, only that")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n\n%s\nflags:\n", listenUsage, listenHelp)
		fs.PrintDefaults()
	}
	parseAnywhere(fs, args)
	if fs.NArg() > 0 {
		ui.Usage("usage: %s (unexpected %q: the local URL goes after --forward-to)", listenUsage, fs.Arg(0))
	}
	opts, err := listenOptionsFrom(*forwardTo, *writeEnv, *forget)
	if err != nil {
		ui.Usage("%v", err)
	}
	return opts
}

const listenHelp = `Gives this machine a QuBe Sync webhook URL for the app (sandbox apps only), the same every
time, and forwards each delivery to it to --forward-to: the same body and headers, so
X-QUBE-Signature verifies as in production. Use the URL as webhook_url, or keep it in .env as
` + webhookURLKey + ` (--write-env .env). A path the webhook_url adds after the URL is added to
--forward-to too. Each line shows a delivery and how the local app answered, which QuBe Sync
records as the delivery's outcome; it retries a failed one. Ctrl-C stops (exit 130).
`

// listenOptionsFrom checks the flags: --forward-to is required unless --forget is given alone,
// since answering deliveries without forwarding them would mark webhooks delivered that no app
// received.
func listenOptionsFrom(forwardTo, writeEnv string, forget bool) (listenOptions, error) {
	opts := listenOptions{writeEnv: writeEnv, forget: forget}
	if forwardTo == "" {
		if forget && writeEnv == "" {
			return opts, nil
		}
		return opts, fmt.Errorf("qube listen needs --forward-to, the local URL to send each delivery to (e.g. --forward-to http://localhost:3000/webhooks/qube)\nusage: %s", listenUsage)
	}
	if !strings.Contains(forwardTo, "://") {
		forwardTo = "http://" + forwardTo
	}
	u, err := url.Parse(forwardTo)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return opts, fmt.Errorf("--forward-to must be an http:// or https:// URL, like http://localhost:3000/webhooks/qube (got %q)", forwardTo)
	}
	u.Fragment = ""
	opts.forwardTo = u
	return opts, nil
}

func (c *ctx) runListen(opts listenOptions) error {
	app := c.currentApp()
	cl := c.cli()
	if opts.forget {
		if err := c.forgetRelay(cl, app); err != nil {
			return err
		}
		if opts.forwardTo == nil {
			return nil
		}
	}
	deviceID, err := config.DeviceID()
	if err != nil {
		return err
	}
	grant, err := cl.CreateRelay(app.ID, deviceID)
	if err != nil {
		return relayError(c.host, err)
	}
	if opts.writeEnv != "" {
		if err := upsertEnvKeys(opts.writeEnv, []string{webhookURLKey}, map[string]string{webhookURLKey: grant.Relay.URL}); err != nil {
			return err
		}
	}
	session, err := uuid.New()
	if err != nil {
		return err
	}
	l := &listener{
		cl:      cl,
		host:    c.host,
		appID:   app.ID,
		relay:   grant.Relay,
		session: session,
		forward: newForwarder(opts.forwardTo),
	}
	l.printStart(app, grant, opts)
	return l.run(c.bg)
}

// relayError puts the server's refusals of a relay in its own words: a production app, an
// unknown app, all of the user's URLs for the app in use. A 404 without the CLI's error
// envelope is a server from before `qube listen`.
func relayError(host string, err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	switch {
	case e.Status == http.StatusNotFound && e.Code != "not_found":
		return fmt.Errorf("this QuBe Sync (%s) doesn't support `qube listen` yet", host)
	case (e.Status == http.StatusForbidden || e.Code == "not_found" || e.Code == "too_many_relays") && e.Message != "":
		return errors.New(e.Message)
	}
	return err
}

// forgetRelay deletes this machine's URL for the app (`--forget`). It finds the URL by device
// id without creating one (findRelay); a machine that never listened anywhere has no device
// id and nothing to do.
func (c *ctx) forgetRelay(cl *api.Client, app *api.App) error {
	deviceID, err := config.ExistingDeviceID()
	if err != nil {
		return err
	}
	var relay *api.Relay
	var replaced *api.ReplacedRelay
	if deviceID != "" {
		if relay, replaced, err = findRelay(c.host, cl, app.ID, deviceID); err != nil {
			return err
		}
	}
	if relay == nil {
		if ui.JSON {
			printJSONLine(map[string]interface{}{"forgotten": nil})
		} else {
			fmt.Printf("This machine has no qube listen URL for %s; nothing to forget.\n", app.Name)
		}
		return nil
	}
	confirm(fmt.Sprintf("Forget this machine's qube listen URL for %s (%s)? Deliveries to it will be refused, and the next qube listen here gets a new URL.", app.Name, relay.URL), c.yes)
	if err := cl.ForgetRelay(app.ID, relay.ID); err != nil {
		return err
	}
	if ui.JSON {
		out := map[string]interface{}{"forgotten": relay}
		if replaced != nil {
			out["replaced"] = replaced
		}
		printJSONLine(out)
		return nil
	}
	fmt.Printf("Forgot %s: deliveries to it are refused from now on, and the next qube listen here gets a new URL.\n", relay.URL)
	if replaced != nil {
		ui.Info("%s", replacedNote(replaced))
	}
	return nil
}

// findRelay is this machine's relay for the app, or nil when it has none, looked up with
// GET .../relays?device_id=, which never creates one or takes one over. Only a server from
// before that route (a 404 without the /api/cli envelope's code) is asked with POST
// .../relays instead, which creates the relay when there is none, and past the user's limit
// first takes over the least recently used one (replaced).
func findRelay(host string, cl *api.Client, appID, deviceID string) (*api.Relay, *api.ReplacedRelay, error) {
	relay, err := cl.FindRelay(appID, deviceID)
	var e *api.Error
	switch {
	case err == nil:
		return relay, nil, nil
	case !errors.As(err, &e) || e.Status != http.StatusNotFound || e.Code != "":
		return nil, nil, relayError(host, err)
	}
	grant, err := cl.CreateRelay(appID, deviceID)
	if err != nil {
		// all of the user's URLs are in use, and none is this machine's
		if errors.As(err, &e) && e.Code == "too_many_relays" {
			return nil, nil, nil
		}
		return nil, nil, relayError(host, err)
	}
	return &grant.Relay, grant.Replaced, nil
}

// replacedNote says that this machine's new URL took over another of the user's, which no
// longer works.
func replacedNote(r *api.ReplacedRelay) string {
	when := "never polled"
	if r.LastPolledAt != "" {
		when = "last polled " + r.LastPolledAt
		if t, err := time.Parse(time.RFC3339, r.LastPolledAt); err == nil {
			when = "last polled " + t.Local().Format("2006-01-02 15:04 MST")
		}
	}
	return fmt.Sprintf("This machine's new URL took over %s (%s), the least recently used of your qube listen URLs for this app. That URL no longer works: update any .env that still holds it.", r.URL, when)
}

// printJSONLine prints v as one line of JSON: under --json, `qube listen` prints a stream.
func printJSONLine(v interface{}) {
	_ = json.NewEncoder(os.Stdout).Encode(v)
}

// ---------------------------------------------------------------- listening

type listener struct {
	cl      *api.Client
	host    string
	appID   string
	relay   api.Relay
	session string
	forward *forwarder
	out     sync.Mutex // one delivery's line at a time
}

func (l *listener) printStart(app *api.App, grant *api.RelayGrant, opts listenOptions) {
	if ui.JSON {
		out := map[string]interface{}{
			"relay":      grant.Relay,
			"app":        map[string]string{"id": app.ID, "name": app.Name},
			"forward_to": opts.forwardTo.Redacted(),
		}
		if grant.Replaced != nil {
			out["replaced"] = grant.Replaced
		}
		if opts.writeEnv != "" {
			out["written"] = opts.writeEnv
		}
		printJSONLine(out)
		return
	}
	fmt.Printf("Webhook URL for %s (the same every time on this machine):\n  %s\n", app.Name, grant.Relay.URL)
	if grant.Replaced != nil {
		ui.Info("%s", replacedNote(grant.Replaced))
	}
	if opts.writeEnv != "" {
		ui.Info("Wrote %s to %s.", webhookURLKey, opts.writeEnv)
	} else {
		envHint(".env", grant.Relay.URL)
	}
	fmt.Printf("Forwarding to %s. Ctrl-C to stop.\n", opts.forwardTo.Redacted())
}

// envHint prints the line to add when an env file exists and doesn't set the URL.
func envHint(path, relayURL string) {
	if _, err := os.Stat(path); err != nil {
		return
	}
	value, found, err := envFileValue(path, webhookURLKey)
	switch {
	case err != nil:
	case !found:
		ui.Info("%s doesn't set %s. Add this line (or run with --write-env %s):\n  %s=%s", path, webhookURLKey, path, webhookURLKey, relayURL)
	case value != relayURL:
		ui.Info("%s sets %s to another URL, so webhooks sent with it don't come here (--write-env %s replaces it).", path, webhookURLKey, path)
	}
}

// envFileValue reads key's value from a dotenv file: found is false when the file or the key
// is missing.
func envFileValue(path, key string) (value string, found bool, err error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(parts[0], "export "))
		if name != key {
			continue
		}
		value = strings.TrimSpace(parts[1])
		if n := len(value); n >= 2 && (value[0] == '"' || value[0] == '\'') && value[n-1] == value[0] {
			value = value[1 : n-1]
		}
		found = true
	}
	return value, found, nil
}

// run polls until Ctrl-C (context.Canceled, after telling the server), a takeover, or an
// answer that waiting won't fix. A network error, a 5xx or a 429 backs off and polls again.
func (l *listener) run(ctx context.Context) error {
	slots := make(chan struct{}, listenConcurrency)
	var inFlight sync.WaitGroup
	var pause time.Duration
	for {
		pollCtx, cancel := context.WithTimeout(ctx, listenPollDeadline)
		deliveries, err := l.cl.PollDeliveries(pollCtx, l.appID, l.relay.ID, l.session)
		cancel()
		if ctx.Err() != nil {
			l.stop()
			return context.Canceled
		}
		if err != nil {
			if !retryablePoll(err) {
				// results for deliveries already handed over still count, after a takeover too
				inFlight.Wait()
				return l.pollError(err)
			}
			pause = nextPause(pause, err)
			ui.Info("Lost the connection to %s (%s); reconnecting in %s...", l.host, truncateRunes(shortError(err), 200), pause)
			select {
			case <-ctx.Done():
				l.stop()
				return context.Canceled
			case <-time.After(pause):
			}
			continue
		}
		if pause > 0 {
			ui.Info("Reconnected.")
			pause = 0
		}
		for _, d := range deliveries {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				l.stop()
				return context.Canceled
			}
			inFlight.Add(1)
			go func(d api.Delivery) {
				defer inFlight.Done()
				defer func() { <-slots }()
				l.handle(ctx, d)
			}(d)
		}
	}
}

// retryablePoll says whether polling again may help: a network error, a 5xx, a 429, the
// poll's own deadline, or an answer that isn't JSON (a proxy's error page, say).
func retryablePoll(err error) bool {
	var syntax *json.SyntaxError
	return api.Retryable(err) || errors.As(err, &syntax)
}

// nextPause is the reconnect backoff: 1 s, doubling to 30 s, or longer when a 429 says so.
func nextPause(pause time.Duration, err error) time.Duration {
	if pause == 0 {
		pause = listenBackoffMin
	} else if pause *= 2; pause > listenBackoffMax {
		pause = listenBackoffMax
	}
	var e *api.Error
	if errors.As(err, &e) && e.RetryAfter > 0 {
		if ra := time.Duration(e.RetryAfter) * time.Second; ra > pause {
			pause = ra
		}
	}
	return pause
}

func (l *listener) pollError(err error) error {
	var e *api.Error
	if errors.As(err, &e) {
		switch {
		case e.Code == "relay_taken_over":
			return fmt.Errorf("another qube listen took over %s (in another terminal, or under another config on this machine); only the newest one receives deliveries, so this one stopped", l.relay.URL)
		case e.Status == http.StatusNotFound:
			return fmt.Errorf("%s stopped working (%s): it was forgotten with `qube listen --forget`, taken over by another of your machines, or its app was deleted. Run qube listen again for a new URL", l.relay.URL, e.Message)
		case e.Status == http.StatusForbidden && e.Message != "":
			return errors.New(e.Message)
		}
	}
	return err
}

// stop tells the server this session stopped (Ctrl-C), so the deliveries waiting on it fail
// now and are retried, rather than at their deadline.
func (l *listener) stop() {
	ctx, cancel := context.WithTimeout(context.Background(), listenStopTimeout)
	defer cancel()
	if err := l.cl.StopListening(ctx, l.appID, l.relay.ID, l.session); err != nil {
		ui.Info("(couldn't tell QuBe Sync that qube listen stopped: %s)", shortError(err))
		return
	}
	ui.Info("Stopped. Deliveries to %s fail until qube listen runs again; QuBe Sync retries each one a few times.", l.relay.URL)
}

// handle forwards one delivery, reports the answer and prints its line.
func (l *listener) handle(ctx context.Context, d api.Delivery) {
	target := l.forward.target(d.Path)
	started := time.Now()
	result := l.forward.post(ctx, target, d)
	elapsed := time.Since(started)
	if ctx.Err() != nil {
		return // Ctrl-C: the server is told the session stopped
	}
	reportErr := l.report(ctx, d.AttemptID, result)
	l.print(started, elapsed, d, target, result, reportErr)
}

// report sends the result, once more after a network error, a 5xx or a 429 if there is
// still time.
func (l *listener) report(ctx context.Context, attemptID string, result api.DeliveryResult) error {
	var err error
	for try := 0; try < 2; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		reportCtx, cancel := context.WithTimeout(ctx, listenReportTimeout)
		err = l.cl.ReportDelivery(reportCtx, l.appID, l.relay.ID, attemptID, result)
		cancel()
		if err == nil || !api.Retryable(err) {
			return err
		}
	}
	return err
}

func (l *listener) print(at time.Time, elapsed time.Duration, d api.Delivery, target string, r api.DeliveryResult, reportErr error) {
	path := target
	if u, err := url.Parse(target); err == nil {
		path = u.RequestURI()
	}
	ms := elapsed.Milliseconds()
	l.out.Lock()
	defer l.out.Unlock()
	if ui.JSON {
		line := map[string]interface{}{
			"at":      at.UTC().Format(time.RFC3339),
			"summary": d.Summary,
			"path":    path,
			"ms":      ms,
		}
		if r.Error != nil {
			line["error"] = r.Error
		} else {
			line["status"] = r.Status
		}
		if reportErr != nil {
			line["reported"] = false
		}
		printJSONLine(line)
		return
	}
	var outcome string
	switch {
	case r.Error != nil:
		outcome = forwardErrorText(r.Error)
	case r.Status >= 300 && r.Status < 400 && r.Location != "":
		outcome = fmt.Sprintf("%d to %s, not followed", r.Status, r.Location)
	default:
		outcome = strconv.Itoa(r.Status)
	}
	note := ""
	switch {
	case reportErr != nil:
		note = "  (" + reportErrorText(reportErr) + "; QuBe will retry)"
	case r.Error != nil || r.Status < 200 || r.Status > 299:
		note = "  (QuBe will retry)"
	}
	fmt.Printf("%s  %s  → POST %s  %s  %d ms%s\n", at.Format("15:04:05"), deliveryText(d), path, outcome, ms, note)
}

func reportErrorText(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		switch e.Code {
		case "attempt_not_pending":
			return "reported too late"
		case "":
		default:
			return "QuBe didn't take the result: " + e.Message
		}
	}
	return "couldn't report it: " + shortError(err)
}

func forwardErrorText(e *api.ForwardError) string {
	switch e.Kind {
	case "connect":
		return "couldn't connect: " + e.Detail
	case "timeout":
		return "no answer in time: " + e.Detail
	case "tls":
		return "TLS failed: " + e.Detail
	}
	return "failed: " + e.Detail
}

// deliveryText is what a delivery is: its summary's text, but for a connection event the
// summary ({kind: "connection", id, event}) has no stall_reason, so the body is read too:
// "connection.stalled  connection 3f2a…  not_checking_in". A body that isn't a connection
// event's JSON gets the generic line.
func deliveryText(d api.Delivery) string {
	if str(d.Summary["kind"]) != "connection" {
		return summaryText(d.Summary)
	}
	var body struct {
		Event      string `json:"event"`
		Connection *struct {
			ID          string `json:"id"`
			StallReason string `json:"stall_reason"`
		} `json:"connection"`
	}
	if err := json.Unmarshal([]byte(d.Body), &body); err != nil || body.Connection == nil {
		return summaryText(d.Summary)
	}
	event, id := orEmpty(str(d.Summary["event"]), body.Event), orEmpty(str(d.Summary["id"]), body.Connection.ID)
	text := event + "  connection " + short(id)
	if event == "connection.stalled" && body.Connection.StallReason != "" {
		text += "  " + body.Connection.StallReason
	}
	return text
}

// summaryText is what a delivery is, from its summary: "request 1f0c2a3b…  response_received",
// "workflow_run.notification  customers_page #3", "name_index.ready  connection 3f2a…".
func summaryText(s map[string]interface{}) string {
	id := short(str(s["id"]))
	switch str(s["kind"]) {
	case "request":
		text := "request " + id + "  " + str(s["state"])
		if page, ok := s["page"].(float64); ok && page > 1 {
			text += fmt.Sprintf("  page %d", int64(page))
		}
		return text
	case "workflow_run":
		return str(s["event"]) + "  run " + id
	case "notification":
		text := str(s["event"]) + "  " + str(s["name"])
		if seq, ok := s["seq"].(float64); ok {
			text += fmt.Sprintf(" #%d", int64(seq))
		}
		return text
	case "name_index":
		return str(s["event"]) + "  connection " + id
	}
	var parts []string
	for _, k := range []string{"kind", "event", "state"} {
		if v := str(s[k]); v != "" {
			parts = append(parts, v)
		}
	}
	if id != "" {
		parts = append(parts, id)
	}
	if len(parts) == 0 {
		return "delivery"
	}
	return strings.Join(parts, "  ")
}

// shortError is an error without the method and URL Go puts in front of a request's.
func shortError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err.Error()
	}
	return err.Error()
}

// ---------------------------------------------------------------- forwarding

// forwarder POSTs deliveries to the local app as QuBe Sync's own webhooks are sent: 5 s to
// connect, 5 s for the answer, redirects never followed, and only the first 1,000 bytes of
// the answer read (and reported, for an answer that isn't a 2xx).
type forwarder struct {
	base   *url.URL
	client *http.Client
	limit  time.Duration // the whole forward, body included
}

func newForwarder(base *url.URL) *forwarder {
	dialer := &net.Dialer{Timeout: forwardDialTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   forwardDialTimeout,
		ResponseHeaderTimeout: forwardHeaderTimeout,
		// the delivery's own headers only: no Accept-Encoding of Go's, so the answer's
		// excerpt is what the app wrote
		DisableCompression:  true,
		ForceAttemptHTTP2:   true,
		MaxIdleConnsPerHost: listenConcurrency,
		IdleConnTimeout:     90 * time.Second,
	}
	return &forwarder{
		base: base,
		client: &http.Client{
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		limit: forwardDialTimeout + forwardHeaderTimeout + time.Second,
	}
}

// target is the --forward-to URL with the delivery's path and query after the relay's own
// added: --forward-to http://localhost:3000/hooks and a delivery to <relay URL>/runs?src=qb
// make http://localhost:3000/hooks/runs?src=qb. With nothing added, it is --forward-to as given.
func (f *forwarder) target(suffix string) string {
	if suffix == "" {
		return f.base.String()
	}
	path, query := suffix, ""
	if i := strings.IndexByte(suffix, '?'); i >= 0 {
		path, query = suffix[:i], suffix[i+1:]
	}
	u := *f.base
	if path != "" {
		joined := strings.TrimRight(f.base.EscapedPath(), "/") + path
		if unescaped, err := url.PathUnescape(joined); err == nil {
			u.Path, u.RawPath = unescaped, joined
		}
	}
	switch {
	case query == "":
	case u.RawQuery == "":
		u.RawQuery = query
	default:
		u.RawQuery += "&" + query
	}
	return u.String()
}

// hopHeaders are never forwarded: Go sets them for the connection to the local app.
var hopHeaders = map[string]bool{
	"host": true, "content-length": true, "connection": true, "keep-alive": true, "proxy-connection": true,
	"transfer-encoding": true, "te": true, "trailer": true, "upgrade": true, "accept-encoding": true,
}

func (f *forwarder) post(ctx context.Context, target string, d api.Delivery) api.DeliveryResult {
	ctx, cancel := context.WithTimeout(ctx, f.limit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", target, strings.NewReader(d.Body))
	if err != nil {
		return forwardFailure(err)
	}
	for name, value := range d.Headers {
		if !hopHeaders[strings.ToLower(name)] {
			req.Header.Set(name, value)
		}
	}
	res, err := f.client.Do(req)
	if err != nil {
		return forwardFailure(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, forwardBodyLimit))
	result := api.DeliveryResult{Status: res.StatusCode}
	// QuBe Sync records an excerpt of a failure only, so a success's answer stays here
	if res.StatusCode < 200 || res.StatusCode > 299 {
		result.Body = string(body)
	}
	if res.StatusCode >= 300 && res.StatusCode < 400 {
		result.Location = res.Header.Get("Location")
	}
	return result
}

// forwardFailure reports why the local app couldn't be reached: a kind from the server's fixed
// set, and Go's error text (which names the URL) as the detail.
func forwardFailure(err error) api.DeliveryResult {
	return api.DeliveryResult{Error: &api.ForwardError{Kind: forwardErrorKind(err), Detail: truncateRunes(err.Error(), forwardDetailLimit)}}
}

func forwardErrorKind(err error) string {
	var op *net.OpError
	var dns *net.DNSError
	switch {
	case errors.As(err, &op) && op.Op == "dial", errors.As(err, &dns):
		return "connect"
	case tlsError(err):
		return "tls"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "other"
}

func tlsError(err error) bool {
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	if errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) || errors.As(err, &record) {
		return true
	}
	text := err.Error()
	return strings.Contains(text, "tls: ") || strings.Contains(text, "x509: ")
}

// truncateRunes keeps s within n characters, ending a cut one with "…".
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}
