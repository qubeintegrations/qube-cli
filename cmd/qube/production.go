package main

import (
	"fmt"
	"strings"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// Every command that changes something goes through confirmWrite or confirmDelete first.
//
// A read-only session (`qube login --read-only`, or read-only chosen when it was approved)
// refuses at once: the server would refuse too, and this says so before asking anything.
//
// In a production app, every such request asks first: a QuickBooks write lands in a
// customer's real books, and a connection, request or workflow changed there is one a live
// integration depends on. Reads never ask, and nothing asks in a sandbox app except removing
// something. `--yes` answers for a script; without a terminal (or under --json) a write that
// would ask is refused until it is given.

// While acting as a customer's organization (`qube act-as`), the questions name it -- "in
// Acme Corp's production app" -- and the grant's access is what a read-only check reads. An
// agent's --yes answers these questions too, but the server holds every grant to its access
// and write budget whatever the CLI asked.

// confirmWrite asks before a request that changes something in a production app.
func (c *ctx) confirmWrite(app *api.App, what string) {
	c.refuseIfReadOnly(what)
	if app.Sandbox {
		return
	}
	confirm(fmt.Sprintf("%s in %s?", what, c.productionApp(app)), c.yes)
}

// confirmDelete asks before removing something, in any app, and says so when the app is
// a production one.
func (c *ctx) confirmDelete(app *api.App, question string) {
	c.refuseIfReadOnly(question)
	if !app.Sandbox {
		where := c.productionApp(app)
		question = fmt.Sprintf("%s%s: %s", strings.ToUpper(where[:1]), where[1:], question)
	}
	confirm(question, c.yes)
}

// productionApp names a production app in a question: `production app "X"`, or while acting
// as an organization `Acme Corp's production app "X"`.
func (c *ctx) productionApp(app *api.App) string {
	if a := c.acting(); a != nil {
		return fmt.Sprintf("%s's production app %q", a.Organization, app.Name)
	}
	return fmt.Sprintf("production app %q", app.Name)
}

func (c *ctx) refuseIfReadOnly(what string) {
	if a := c.acting(); a != nil {
		if a.ReadOnly() {
			ui.Fail("this act-as grant for %s is read-only, so it can't do this (%s). `qube act-as --end`, then ask for one with --access configure or read_write.", a.Organization, trimQuestion(what))
		}
		return
	}
	if c.cfg.Sessions[c.host].ReadOnly() {
		ui.Fail("this session is read-only, so it can't do this (%s). `qube login` again and choose read and write access.", trimQuestion(what))
	}
}

func trimQuestion(s string) string {
	if n := len(s); n > 0 && s[n-1] == '?' {
		return s[:n-1]
	}
	return s
}
