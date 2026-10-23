// qube: the QuBe Sync command line. Log in once with a device code, then read your apps'
// credentials, create connections (simulated ones included), watch requests, drive the
// simulator and push workflows -- from a shell, a script, or an agent (--json).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

type ctx struct {
	host         string
	hostExplicit bool // --host or QUBE_HOST was given
	app          string
	timeout      time.Duration
	cfg          *config.File
	bg           context.Context
	yes          bool     // --yes: answer a confirmation (production writes, deletes) for a script
	timeoutSet   bool     // --timeout was given (not just QUBE_TIMEOUT): a waiting command's limit too
	connection   string   // --connection: the connection a command acts on, when it doesn't name one
	appCache     *api.App // the app this command acts as, once looked up

	// act-as (actas.go): what ended when the config was read, the organization whose grant the
	// server refused, and the once that drops it.
	actAsEnded string
	refusedOrg string
	actAsOnce  sync.Once
}

func main() {
	if len(os.Args) > 2 && os.Args[1] == "help" && os.Args[2] == "staff" {
		staffUsage()
		return
	}
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}
	// global flags may appear anywhere: --host, --app, --json, --timeout, --yes, --connection
	var host, app, timeout, connection string
	var yes bool
	var args []string
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "--json":
			ui.JSON = true
		case a == "--yes":
			yes = true
		case a == "--connection" && i+1 < len(os.Args):
			connection = os.Args[i+1]
			i++
		case strings.HasPrefix(a, "--connection="):
			connection = strings.TrimPrefix(a, "--connection=")
		case a == "--host" && i+1 < len(os.Args):
			host = os.Args[i+1]
			i++
		case strings.HasPrefix(a, "--host="):
			host = strings.TrimPrefix(a, "--host=")
		case a == "--app" && i+1 < len(os.Args):
			app = os.Args[i+1]
			i++
		case strings.HasPrefix(a, "--app="):
			app = strings.TrimPrefix(a, "--app=")
		case a == "--timeout" && i+1 < len(os.Args):
			timeout = os.Args[i+1]
			i++
		case strings.HasPrefix(a, "--timeout="):
			timeout = strings.TrimPrefix(a, "--timeout=")
		default:
			args = append(args, a)
		}
	}
	cfg, err := config.Load()
	if err != nil {
		ui.Fail("reading config: %v", err)
	}
	if host == "" {
		host = os.Getenv("QUBE_HOST")
	}
	timeoutSet := timeout != ""
	if timeout == "" {
		timeout = os.Getenv("QUBE_TIMEOUT")
	}
	c := &ctx{
		host:         cfg.ResolveHost(host),
		hostExplicit: host != "",
		app:          app,
		timeout:      parseTimeout(timeout),
		timeoutSet:   timeoutSet,
		cfg:          cfg,
		yes:          yes,
		connection:   connection,
	}
	bg, stop := signalContext()
	defer stop()
	c.bg = bg
	if len(args) == 0 {
		usage()
		return
	}
	c.dropEndedActAs()
	switch {
	case usesActAs(args):
		c.enterActAs()
	case c.actAsEnded != "" && (args[0] == "status" || args[0] == "admin" || args[0] == "act-as"):
		ui.Info("(%s; it's dropped)", c.actAsEnded)
	}
	switch args[0] {
	case "version":
		fmt.Println("qube " + api.Version)
	case "login":
		c.login(args[1:])
	case "logout":
		c.logout()
	case "whoami":
		c.whoami()
	case "status":
		c.status()
	case "apps":
		c.apps()
	case "use":
		c.use(args[1:])
	case "env":
		c.env(args[1:])
	case "sessions":
		c.sessions(args[1:])
	case "connections":
		c.connections(args[1:])
	case "match":
		c.match(args[1:])
	case "requests":
		c.requests(args[1:])
	case "listen":
		c.listen(args[1:])
	case "simulator":
		c.simulator(args[1:])
	case "workflows":
		c.workflows(args[1:])
	case "check":
		c.check(args[1:])
	case "skills":
		c.skills(args[1:])
	case "qb", "quickbooks":
		c.qb(args[1:])
	case "api":
		c.rawAPI(args[1:])
	case "admin":
		c.admin(args[1:])
	case "act-as":
		c.actAs(args[1:])
	case "completion":
		completion(args[1:])
	default:
		ui.Usage("unknown command %q (try `qube help`)", args[0])
	}
}

// signalContext is cancelled on Ctrl-C or SIGTERM: every command's requests carry it.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// usesActAs says whether a command would use an act-as grant the session holds: every one
// but those about the session itself, `qube admin` (always the staff session), `qube act-as`
// and `qube skills` (public files, fetched without a token). Completion's `qb --complete` reads only the cache and stays quiet.
func usesActAs(args []string) bool {
	switch args[0] {
	case "version", "login", "logout", "status", "admin", "act-as", "completion", "skills":
		return false
	case "qb", "quickbooks":
		for _, a := range args[1:] {
			if a == "--complete" {
				return false
			}
		}
	}
	return true
}

// parseTimeout reads "30" or "30s" or "2m"; empty or invalid means the default (60 s).
func parseTimeout(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return api.DefaultTimeout
	}
	if n, err := strconv.Atoi(s); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	ui.Usage("--timeout must be seconds or a duration like 30s (got %q)", s)
	return 0
}

// fail reports an error and exits: 130 when the user pressed Ctrl-C, 1 otherwise.
func fail(err error) {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "qube: interrupted")
		os.Exit(ui.ExitInterrupted)
	}
	ui.Fail("%v", err)
}

func usage() {
	fmt.Print(`qube ` + api.Version + ` -- the QuBe Sync command line

Account:
  login [--name N] [--read-only] [--agent NAME]
                                   connect this terminal (approve in the browser; no secrets typed);
                                   --read-only asks for a session that can look but not change anything;
                                   --agent NAME says an AI agent will hold it (it can revoke only itself)
  status                           host, user, scope, access, defaults and expiry of the session (offline)
  whoami                           the same, confirmed by the server
  logout                           revoke this terminal's session
  sessions [revoke <id>]           every session of yours on this host
  apps                             the apps this session may act on
  use <app> | use --host H         pick the default app / the default host (when logged in to several)
  use --connection <id|name|none>  pick the app's default connection, for commands that don't name one
  env [--write .env] [--print]     the app's QUBE_URL, QUBE_API_KEY and QUBE_WEBHOOK_SECRET (written, not shown;
                                   read and write sessions only)

QuickBooks operations (every one the host's v2 API has, read from its OpenAPI document):
  qb [--refresh]                   every resource and its verbs (--refresh reads the list again)
  qb <resource>                    a resource's operations
  qb <resource> <verb> --help      an operation's flags (--help --json: its full schema)
  qb <resource> <verb> --example   the API's example body, as JSON: save it, edit it, then --data @file
  qb <resource> <verb> [connection] [--flag value...] [--data JSON|@file|-] [--wait[=10m]]
                                   queue it: e.g. qb customers list --max-returned 5;
                                   --wait waits for QuickBooks' answer (every page) and prints it

Connections, matching, requests, the simulator and workflows (as the default app, or --app). [connection]
may be left out: it is then --connection, or the default from ` + "`qube use --connection`" + `.
  connections list                                          * marks the default
  connections create [--simulated] [--name N] [--webhook-url U] [--use]   (--use: make it the default)
                                   U is sent the connection's events (connection.connected, setup_cancelled, stalled, resumed)
  connections show [connection]                             (table, or --json)
  connections update [connection] [--name N] [--webhook-url U]   (--webhook-url "" stops the events)
  connections delete <connection>                           also discards every queued request on it
  connections qwc [connection] [--output FILE]              the .qwc file the Web Connector needs
  connections password [connection] [--stdin]               a new Web Connector password (never as a flag)
  connections onboarding-url [connection]                   a fresh onboarding link
  connections name-index [connection] [--webhook-url U]     build the name index before the first match
                                                            (U is sent name_index.ready when it's in)
  match [connection] <kind> <name> [--parent P] [--exact-only] [--min-score S]
        [--email E] [--phone P] [--account-number N] [--company-name C] [--city C] [--postal-code Z]
                                   the records a name could mean (exact, strong, candidates or none),
                                   from the name index of a realtime or simulated connection;
                                   kind: customer, vendor, item, account, entity... (qube match -h)
  requests list [connection] [--page N] [--page-size N] [--state S] [--webhook-state S]
                              [--search TEXT] [--sort inserted_at|updated_at] [--sort-direction asc|desc]
  requests show [connection] <id>                           (JSON)
  requests pages [connection] <id>                          every page of an iterated query (JSON)
  requests discard [connection] <id>                        withdraw request the Web Connector hasn't picked up
  requests tail [connection]                                watch a connection; Ctrl-C stops
  simulator show|reset|sync [connection]
  simulator faults [connection] [--qb closed|modal|mismatch|unexpected|ok]
                                  [--next xml|lost|3100|3120|3140|3180|3200|ok] [--latency ms]
  workflows list
  workflows show KEY                                        (JSON)
  workflows push FILE [--publish] [--notes TEXT]
  workflows validate FILE                                   check a chart without pushing it
  workflows publish KEY [--notes TEXT]
  workflows unpublish KEY
  workflows delete KEY
  workflows versions KEY [NUMBER]                           every published version, or one (JSON)
  workflows usage KEY [--version V] | --all                 Markdown docs for the chart's input/output;
                                   --all: every workflow's, one after another (--json: [{key, usage}])
  workflows schema                                          the chart JSON Schema
  workflows templates [KEY [--usage]]                       shipped charts ready to install
                                   KEY --usage: the usage contract it will have once installed (what it asks, what it
                                   ends in), printed as "workflows usage" prints one (--json: the contract as JSON)
  workflows install KEY [--publish] [--notes TEXT] [--as NEW_KEY] [--replace]
                                   --replace gives a workflow the app already has the template's chart
  workflows run KEY [--input JSON|@file] [--version V] [--webhook-url U] [--wait]
  workflows runs [connection] [run-id] [--events] [--state S] [--outcome O] [--after SEQ] [--limit N]
                                   (a run or its events: JSON; with a connection chosen, a lone id is the run;
                                   a run waiting at a question prints the question: its message, details and choices)
  workflows decide [connection] <run-id> --choice ID [--data JSON] [--wait]
                                   answer a waiting run's question with one of its choices; a choice with fields
                                   takes them in --data (<option> in place of --choice ID still works)
  workflows search [connection] <run-id> --field NAME QUERY
                                   the records a waiting question's record field can take (value, label, detail);
                                   put the value in --data: decide RUN --choice use --data '{"list_id":"..."}'
  workflows cancel [connection] <run-id> [--reason TEXT]
  workflows delete-run [connection] <run-id>                only once the run has ended
  api METHOD PATH [--data JSON|@file|-]                     any v2 call, as the current app (PATH /connections
                                                            means /api/v2/connections)

Webhooks on this machine (sandbox apps; as the default app, or --app):
  listen --forward-to URL [--write-env FILE] [--forget]
                                   a webhook_url for this machine, the same every time: each delivery to it is
                                   POSTed to URL (plus any path the webhook_url adds) with the same body and
                                   headers, and the answer goes back to QuBe Sync; one line per delivery
                                   (--json: one object per line); --write-env .env stores the URL as
                                   QUBE_WEBHOOK_URL; --forget deletes this machine's URL (alone: only that);
                                   Ctrl-C stops

Checking your integration (as the default app, or --app):
  check webhooks URL [--operation SLUG] [--record ID] [--production]
                                   sends signed test deliveries to your app's webhook endpoint (URL is
                                   QUBE_WEBHOOK_URL) and grades the answers: a wrong signature and a stale
                                   timestamp get 401, a signed body with unusual spacing is accepted, an
                                   unknown operation gets 404, a run event (--operation SLUG) and a connection
                                   event (--record ID) get a 2xx within 5 s. Exits 1 when a check fails.
                                   The webhook secret signs them and is never shown; not for QuBe Sync's own
                                   host, and a URL that isn't on this machine needs --yes

Agent skills (no login needed):
  skills install [NAME] [--dir DIR]
                                   an agent skill QuBe Sync publishes, written to DIR/.claude/skills/NAME/SKILL.md
                                   (NAME: qube-sync-audit, the integration audit, by default; DIR: here); run it
                                   again to update it

Other:
  completion bash|zsh|fish         shell completion script (eval or save it)
  version

Global flags (anywhere on the line):
  --json          machine-readable output only (lists print the server's data; detail views always print JSON)
  --host H        the QuBe Sync host (default: the host you logged in to; QUBE_HOST)
  --app NAME|ID   act as this app instead of the default from ` + "`qube use`" + `
  --timeout 60    HTTP timeout in seconds, or a duration like 2m (QUBE_TIMEOUT)
  --connection C  the connection a command acts on when it doesn't name one (default: ` + "`qube use --connection`" + `)
  --yes           answer yes when asked to confirm, for a script

In a production app every request that changes something asks first (removing something asks in
any app). Without a terminal, or under --json, such a request needs --yes. A read-only session
(` + "`qube login --read-only`" + `) can't change anything at all.

Exit codes: 0 ok, 1 failed, 2 wrong usage, 130 interrupted.
Config: ` + configPathForHelp() + ` (QUBE_CONFIG overrides). QUBE_NO_BROWSER=1 stops login opening a browser.
The qb operation list is cached in ` + opsCacheDir() + ` (QUBE_CACHE_DIR overrides).
`)
	if staffSession() {
		fmt.Print("\nQuBe Sync staff: `qube help staff` lists the admin and act-as commands.\n")
	}
}

// staffSession says whether this host's session holds staff permissions (or an act-as
// grant): only then does `qube help` point at the staff commands.
func staffSession() bool {
	cfg, err := config.Load()
	if err != nil {
		return false
	}
	s, ok := cfg.Sessions[cfg.ResolveHost(os.Getenv("QUBE_HOST"))]
	return ok && (s.Staff() || s.ActingAs != nil)
}

// staffUsage is `qube help staff` and `qube admin help`: the commands for QuBe Sync staff,
// kept out of `qube help` so customers never wonder about them.
func staffUsage() {
	fmt.Print(`qube ` + api.Version + ` -- commands for QuBe Sync staff

These work only for QuBe Sync staff: a session approved with staff permissions.

Logging in:
  login --admin                    ask for staff permissions, builds.manage among them (a person's staff
                                   session lasts 12 hours)
  login --agent NAME [--admin]     a session for an AI agent; with --admin it asks for admin.read,
                                   admin.read_customer_data, org.act, org.act_production and
                                   builds.manage, and its person ticks which (4 hours). Give the
                                   agent its own QUBE_CONFIG, ideally where it can't read yours.
  status | sessions | whoami       who holds the session (a person or an agent), its staff
                                   permissions and when it ends

Monitoring, across every organization (always with the staff session, never an act-as grant):
  admin status [--check]                   service health; --check exits 1 when something needs attention
  admin tail [--kind K,...] [--org ORG] [--production] [--since 10m] [--detail]
                                           events as they happen (Ctrl-C stops, exit 130)
  admin events [--kind K,...] [--org ORG] [--production] [--since 24h] [--after CURSOR] [--detail]
  admin orgs [--search TEXT] [--page N]    |  admin org <id or exact name>
  admin users [--search TEXT] [--page N]   |  admin user <id or email>
  admin connections [--org ORG] [--app ID] [--type standard|realtime|simulated] [--stalled]
  admin connection <id>
  admin requests (--connection ID | --app ID | --org ORG) [--state S] [--webhook-state S]
                 [--since 1h] [--limit N] [--after CURSOR]
  admin request <id> [--xml]
  admin runs (--org ORG | --connection ID | --state S | --stuck) [--limit N] [--after CURSOR]
  admin run <id> [--events] [--io] [--events-after CURSOR]
  admin jobs --state S [--queue Q] [--worker W] [--since 24h] [--limit N] [--before CURSOR] [--args]
  admin feedback [--status new|fixed|wont_fix|all] [--subject S]
  admin mcp-calls [--days 1|7|30]
  admin audit [--org ORG] [--actor EMAIL] [--token SESSION] [--action A] [--since 7d] [--before CURSOR]
  ORG is an organization's id or exact name. Customer data (--xml, --io, --events, --args,
  --detail) is asked for explicitly and needs admin.read_customer_data.

Building integrations ("build it free"; reading needs admin.read, a request's text also
admin.read_customer_data, changing anything builds.manage -- which an agent's session can hold):
  admin integration-requests [--status submitted|building|delivered] [--page N] [--page-size N]
  admin integration-request ID                       what they sent, the kit, the app, the latest build
  admin integration-request ID set-app APP_ID        the sandbox app the build installs and tests in
  admin integration-request ID kit                   the kit's stack, HTTP library and mappings
  admin integration-request ID kit set [--stack S] [--http H] [--mapping-out FILE] [--mapping-in FILE]
                                   only the fields you give; an empty value clears one
  admin integration-request ID kit files --out DIR [--force]
                                   the generated kit's files, written under DIR: never outside it
                                   (an absolute path or one with .. is refused), and no file
                                   overwritten without --force
  admin integration-request ID builds                its builds, newest first
  admin builds start REQUEST_ID                      start build n+1 (it begins with the last plan)
  admin build ID                                     stages, gates, what's next and the test company's scenarios
  admin build ID manifest                            the plan, as JSON
  admin build ID manifest set FILE|-                 save the plan; a 422 prints every problem as path: message
  admin build ID stage STAGE running|done|failed [--error MSG] [--output JSON|@FILE]
                                   STAGE: plan, install, test_company (or test-company), code, video
  admin build ID test-company [--wait[=10m]]         run the test company on the server; --wait reads the
                                   build every few seconds, prints each scenario's result as it
                                   lands, and exits 1 when one fails or the wait runs out (--timeout D
                                   sets the limit too). Under --json, results go to stderr and stdout
                                   holds the build at the end.
  Gates are approved, and builds reopened, only on the admin page in a browser: no session holds that.

Acting in a customer's organization:
  act-as ORG --reason R [--for 60m] [--production] [--access read_only|configure|read_write]
         [--write-budget N] [--wait|--no-wait]
                                   a person's sandbox grant starts at once (up to 8 h, read and
                                   write); a production grant (--production: every app, up to 2 h,
                                   configure) and any grant an agent asks for is approved in the
                                   browser; --no-wait prints the link and exits
  act-as [--status]                the grant this session holds, or waits for
  act-as --resume                  wait for a grant asked for with --no-wait
  act-as --end                     revoke it and go back to your own organization
  While acting, every other command (workflows, connections, simulator, qb, listen, api...) works
  in the customer's organization, with the grant's apps, and prints "(acting as ...)" on stderr,
  --json included. qube env is unavailable: the customer's API key never leaves the server. A
  grant that ends stops the next command instead of letting it run in your own organization.

Grants and agents:
  admin grants                     live grants, and grants waiting for approval
  admin grants revoke <id>         end one
  admin agents                     agents' live sessions, and whether agent staff access is on
  admin agents revoke <id> | revoke --all [--staff]
                                   end an agent's session and its grants, or every agent's (with
                                   --staff, every staff session and grant too, this one included)
  admin agents disable | enable    the kill switch: every agent's staff access off, or back on

--yes answers qube's own questions (a production write, a delete), for an agent's session too. It
widens nothing: the server holds every agent and every grant to its access, write budget and rate
limit, whatever --yes says.
`)
}

func configPathForHelp() string {
	p, err := config.Path()
	if err != nil {
		return "$XDG_CONFIG_HOME/qube/credentials.json"
	}
	return p
}
