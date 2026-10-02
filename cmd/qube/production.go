package main

import (
	"fmt"

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

// confirmWrite asks before a request that changes something in a production app.
func (c *ctx) confirmWrite(app *api.App, what string) {
	c.refuseIfReadOnly(what)
	if app.Sandbox {
		return
	}
	confirm(fmt.Sprintf("%s in production app %q?", what, app.Name), c.yes)
}

// confirmDelete asks before removing something, in any app, and says so when the app is
// a production one.
func (c *ctx) confirmDelete(app *api.App, question string) {
	c.refuseIfReadOnly(question)
	if !app.Sandbox {
		question = fmt.Sprintf("Production app %q: %s", app.Name, question)
	}
	confirm(question, c.yes)
}

func (c *ctx) refuseIfReadOnly(what string) {
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
