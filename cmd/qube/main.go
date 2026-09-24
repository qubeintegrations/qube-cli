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
}

func main() {
	if len(os.Args) < 2 || os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
		usage()
		return
	}
	// global flags may appear anywhere: --host, --app, --json, --timeout
	var host, app, timeout string
	var args []string
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		switch {
		case a == "--json":
			ui.JSON = true
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
	if timeout == "" {
		timeout = os.Getenv("QUBE_TIMEOUT")
	}
	c := &ctx{
		host:         cfg.ResolveHost(host),
		hostExplicit: host != "",
		app:          app,
		timeout:      parseTimeout(timeout),
		cfg:          cfg,
	}
	bg, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c.bg = bg
	if len(args) == 0 {
		usage()
		return
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
	case "requests":
		c.requests(args[1:])
	case "simulator":
		c.simulator(args[1:])
	case "workflows":
		c.workflows(args[1:])
	case "api":
		c.rawAPI(args[1:])
	case "completion":
		completion(args[1:])
	default:
		ui.Usage("unknown command %q (try `qube help`)", args[0])
	}
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
  login [--name N]                 connect this terminal (approve in the browser; no secrets typed)
  status                           host, user, scope, default app and expiry of the stored session (offline)
  whoami                           the same, confirmed by the server
  logout                           revoke this terminal's session
  sessions [revoke <id>]           every session of yours on this host
  apps                             the apps this session may act on
  use <app> | use --host H         pick the default app / the default host (when logged in to several)
  env [--write .env] [--print]     the app's QUBE_URL, QUBE_API_KEY and QUBE_WEBHOOK_SECRET (written, not shown)

QuickBooks (acts as the default app, or --app):
  connections list
  connections create [--simulated] [--name N] [--redirect-url U]
  requests list <connection> [--page N] [--page-size N]
  requests show <id>                                        (JSON)
  requests tail <connection>                                watch a connection; Ctrl-C stops
  simulator show|reset|sync <connection>
  simulator faults <connection> [--qb closed|modal|mismatch|unexpected|ok]
                                  [--next xml|3100|3120|3140|3180|3200|ok] [--latency ms]
  workflows list
  workflows push FILE [--publish] [--notes TEXT]
  workflows run KEY --connection C [--input JSON|@file] [--version V] [--webhook-url U]
  workflows runs <connection> [run-id] [--events]           (a run or its events: JSON)
  workflows decide <connection> <run-id> <option> [--data JSON]
  api METHOD PATH [--data JSON|@file|-]                     any /api/v1 or /api/v2 call with the app's key

Other:
  completion bash|zsh|fish         shell completion script (eval or save it)
  version

Global flags (anywhere on the line):
  --json          machine-readable output only (lists print the server's data; detail views always print JSON)
  --host H        the QuBe Sync host (default: the host you logged in to; QUBE_HOST)
  --app NAME|ID   act as this app instead of the default from ` + "`qube use`" + `
  --timeout 60    HTTP timeout in seconds, or a duration like 2m (QUBE_TIMEOUT)

Exit codes: 0 ok, 1 failed, 2 wrong usage, 130 interrupted.
Config: ` + configPathForHelp() + ` (QUBE_CONFIG overrides). QUBE_NO_BROWSER=1 stops login opening a browser.
`)
}

func configPathForHelp() string {
	p, err := config.Path()
	if err != nil {
		return "$XDG_CONFIG_HOME/qube/credentials.json"
	}
	return p
}
