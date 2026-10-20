package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// `qube check webhooks URL` sends signed test deliveries to the app's own webhook endpoint
// and grades the HTTP answers (docs/integration-audit.md, "Step 2: live checks", in the QuBe
// Sync repo). Each check is one request, tied to an integration rule by its id: a wrong
// signature and a stale timestamp must be refused with 401, a correctly signed body with
// unusual formatting must be accepted (the signature is over the raw bytes), a slug none of
// the app's operations has gets 404, and a run event and a connection event get a 2xx within
// 5 seconds. The deliveries are fake: their ids say qube-check, and no record of the app's can
// match them, so an app that handles them right logs and skips them.
//
// It signs with the app's webhook secret, fetched as `qube env` fetches it and kept in memory
// only (check_run.go: webhookSecret). Nothing here prints, logs or writes it, and no answer
// body is ever printed, since an app's error page can quote its environment.

const checkWebhooksUsage = "qube check webhooks URL [--operation SLUG] [--record ID] [--production] [--yes] [--json]"

const checkWebhooksHelp = `Sends signed test deliveries to your app's webhook endpoint and grades the answers. URL is
the base your app builds each webhook URL from, QUBE_WEBHOOK_URL (http://localhost:3000/webhooks/qube).
One request per check, signed with the app's webhook secret, which is never shown:
  bad-signature-401       a wrong signature must be refused with 401
  stale-timestamp-401     a correct signature over a timestamp 10 minutes old must get 401
  raw-body-signature      a correct signature over a body with unusual spacing, key order and \u
                          escapes must be accepted (--operation): the signature is over the raw bytes
  unknown-operation-404   a slug none of your operations has must get 404
  fast-2xx                a run event for --operation SLUG must get a 2xx within 5 s, not a 5xx
  connection-event-2xx    with --record ID, a connection.connected for another connection must get
                          a 2xx within 5 s (the app should skip it: see its log)
Exits 1 when a check fails. It refuses QuBe Sync's own host, and asks for --yes when URL isn't on this
machine. A redirect is reported, never followed; a request that gets no answer in 10 s fails.
`

// remoteTargetNote is why a URL that isn't on this machine needs --yes.
const remoteTargetNote = "isn't on this machine: qube check will send it fake deliveries (their ids say qube-check), signed with the app's webhook secret"

func (c *ctx) check(args []string) {
	if len(args) == 0 {
		ui.Usage("usage: %s", checkWebhooksUsage)
	}
	if args[0] != "webhooks" {
		ui.Usage("unknown check %q (usage: %s)", args[0], checkWebhooksUsage)
	}
	if !c.checkWebhooks(args[1:]) {
		os.Exit(ui.ExitFailure)
	}
}

// checkWebhooks runs the checks and prints the report; it says whether every check that ran
// passed. The command line's mistakes and the refusals below end the process first.
func (c *ctx) checkWebhooks(args []string) bool {
	fs := flag.NewFlagSet("check webhooks", flag.ExitOnError)
	operation := fs.String("operation", "", "an operation slug your webhook route has: the run-event checks send to it")
	record := fs.String("record", "", "the id of the record in your app that keeps a connection (URL/connection/ID is its connection webhook URL): the connection-event check sends there")
	production := fs.Bool("production", false, "the current app is a production app: say so to send it test deliveries")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n\n%sflags:\n", checkWebhooksUsage, checkWebhooksHelp)
		fs.PrintDefaults()
	}
	parseAnywhere(fs, args)
	if fs.NArg() != 1 {
		ui.Usage("usage: %s", checkWebhooksUsage)
	}
	if strings.ContainsAny(*operation, "/?# \t\r\n") {
		ui.Usage("--operation is one slug of your webhook route, like orders (got %q)", *operation)
	}

	target, err := parseCheckURL(fs.Arg(0))
	if err != nil {
		ui.Usage("%v", err)
	}
	if err := refuseOwnHost(c.host, target); err != nil {
		ui.Fail("%v", err)
	}
	if a := c.acting(); a != nil {
		ui.Fail("qube check doesn't work while acting as %s: it signs with the app's webhook secret, which an act-as grant never reads. "+
			"`qube act-as --end` returns to your own apps.", a.Organization)
	}
	if c.cfg.Sessions[c.host].ReadOnly() {
		ui.Fail("this session is read-only, so it can't read the app's webhook secret, which qube check signs with. `qube login` again and choose read and write access.")
	}
	app := c.currentApp()
	if !app.Sandbox && !*production {
		ui.Usage("%q is a production app: qube check reads its webhook secret and sends your endpoint fake deliveries signed with it. Pass --production to do that.", app.Name)
	}
	shown := target.Redacted()
	if !localTarget(target) {
		question := fmt.Sprintf("%s %s.", shown, remoteTargetNote)
		if c.yes {
			ui.Warn("%s", question)
		} else {
			confirm(question+" Send them?", false)
		}
	}

	creds, err := c.cli().Credentials(app.ID)
	if err != nil {
		fail(err)
	}
	if creds.WebhookSecret == "" {
		ui.Fail("%s has no webhook secret to sign with: add one in the app's settings in the dashboard.", app.Name)
	}
	run := &webhookCheckRun{
		base:      strings.TrimRight(target.String(), "/"),
		host:      strings.TrimRight(c.host, "/"),
		app:       *app,
		secret:    webhookSecret(creds.WebhookSecret),
		operation: *operation,
		record:    *record,
	}
	creds.WebhookSecret = ""
	report, err := run.run(c.bg)
	if err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(report)
	} else {
		report.print(os.Stdout)
	}
	return report.OK
}

// parseCheckURL reads the URL to check: an http or https URL with a host, the base the app builds
// its webhook URLs from, so without a query (its slugs are added to the path).
func parseCheckURL(raw string) (*url.URL, error) {
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return nil, fmt.Errorf("URL must be an http:// or https:// URL, like http://localhost:3000/webhooks/qube (got %q)", raw)
	}
	if u.RawQuery != "" || u.ForceQuery {
		return nil, fmt.Errorf("URL is the base your app adds each operation to (/webhooks/qube), so it can't carry a query (got %q)", u.Redacted())
	}
	u.Fragment = ""
	return u, nil
}

// refuseOwnHost refuses QuBe Sync's own host, the session's. A `qube listen` relay URL is on
// it too (https://qubesync.com/relay/rl_...): QuBe Sync serves nothing there, since it hands a
// delivery to the running `qube listen` instead, so the answer says where to send the checks.
func refuseOwnHost(host string, target *url.URL) error {
	h, err := url.Parse(host)
	if err != nil || h.Hostname() == "" {
		return nil
	}
	if !sameServer(target, h) {
		return nil
	}
	if isRelayURL(h, target) {
		return fmt.Errorf("%s is a qube listen URL: QuBe Sync hands a delivery to it to your running `qube listen`, and serves nothing there for qube check to post to. "+
			"Check the local URL you gave --forward-to (http://localhost:3000/webhooks/qube) instead.", target.Redacted())
	}
	return fmt.Errorf("%s is on QuBe Sync's own host (%s): qube check sends test deliveries to your app, never to QuBe Sync.", target.Redacted(), h.Host)
}

// isRelayURL says whether target is a `qube listen` URL of the host: its own origin, and a path
// that starts /relay/rl_ (as Qube.CLIRelay.parse_url reads one).
func isRelayURL(host, target *url.URL) bool {
	return sameOrigin(host, target) && strings.HasPrefix(target.Path, "/relay/rl_")
}

func sameOrigin(a, b *url.URL) bool {
	ah, ap := hostPort(a)
	bh, bp := hostPort(b)
	return strings.EqualFold(a.Scheme, b.Scheme) && ah == bh && ap == bp
}

// sameServer says whether target is QuBe Sync's host. The hostnames match, except that a
// local QuBe Sync (a dev server on localhost:4002) and a local app (localhost:3000) are told
// apart by port, and any two spellings of the loopback address on the same port are one server.
func sameServer(target, host *url.URL) bool {
	th, tp := hostPort(target)
	hh, hp := hostPort(host)
	switch {
	case loopbackHost(th) && loopbackHost(hh):
		return tp == hp
	default:
		return th == hh
	}
}

// hostPort is a URL's lower-case hostname and its port, the scheme's default when it has none.
func hostPort(u *url.URL) (host, port string) {
	host = strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port = u.Port()
	if port == "" {
		port = "80"
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		}
	}
	return host, port
}

// loopbackHost says whether a hostname is this machine: localhost, 127.0.0.0/8 or ::1.
func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// localTarget says whether the URL is on this machine, which sending fake deliveries to needs no
// --yes for.
func localTarget(u *url.URL) bool {
	host, _ := hostPort(u)
	return loopbackHost(host)
}

// ---------------------------------------------------------------- the report

// webhookCheckReport is what the command prints: a table, or under --json this object.
type webhookCheckReport struct {
	URL     string               `json:"url"`
	App     webhookCheckApp      `json:"app"`
	Checks  []webhookCheckResult `json:"checks"`
	Summary webhookCheckSummary  `json:"summary"`
	OK      bool                 `json:"ok"` // no check failed (a skipped one doesn't)
}

type webhookCheckApp struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Sandbox bool   `json:"sandbox"`
}

type webhookCheckSummary struct {
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// webhookCheckResult is one check. Result is pass, fail or skipped; Pass is true, false or
// null (skipped). A skipped check says why in Detail and sent nothing.
type webhookCheckResult struct {
	Check     string `json:"check"`
	Rule      string `json:"rule,omitempty"`
	Sent      string `json:"sent,omitempty"`
	Path      string `json:"path,omitempty"`
	Expected  string `json:"expected,omitempty"`
	Got       string `json:"got,omitempty"`
	Status    int    `json:"status,omitempty"`
	ElapsedMS *int64 `json:"elapsed_ms,omitempty"`
	Result    string `json:"result"`
	Pass      *bool  `json:"pass"`
	Detail    string `json:"detail,omitempty"`
}

func (r *webhookCheckReport) print(w io.Writer) {
	fmt.Fprintf(w, "Checking %s as %q (%s app).\n", r.URL, r.App.Name, appKind(r.App.Sandbox))
	fmt.Fprintln(w, "One test delivery per check, signed with the app's webhook secret. Their ids say qube-check, so an app that handles them right logs and skips them.")
	fmt.Fprintln(w)
	var rows [][]string
	for _, ck := range r.Checks {
		if ck.Result == "skipped" {
			rows = append(rows, []string{ck.Check, ck.Rule, "-", "-", "-", "skipped"})
			continue
		}
		result := "pass"
		if ck.Result == "fail" {
			result = "FAIL"
		}
		rows = append(rows, []string{ck.Check, ck.Rule, ck.Sent, ck.Expected, ck.Got, result})
	}
	ui.Table(w, []string{"CHECK", "RULE", "SENT", "EXPECTED", "GOT", "RESULT"}, rows, "")
	notes := false
	for _, ck := range r.Checks {
		if ck.Detail == "" {
			continue
		}
		if !notes {
			fmt.Fprintln(w)
			notes = true
		}
		fmt.Fprintf(w, "%s: %s\n", ck.Check, ck.Detail)
	}
	fmt.Fprintf(w, "\n%d passed, %d failed, %d skipped.\n", r.Summary.Passed, r.Summary.Failed, r.Summary.Skipped)
}

func appKind(sandbox bool) string {
	if sandbox {
		return "sandbox"
	}
	return "production"
}

// formatElapsed is a duration for a table: 40ms, 1.2s.
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d/time.Millisecond)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
