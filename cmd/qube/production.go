package main

import (
	"fmt"

	"github.com/qubeintegrations/qube-cli/internal/api"
)

// In a production app, every request that changes something asks first: a QuickBooks
// write lands in a customer's real books, and a connection, request or workflow changed
// there is one a live integration depends on. Reads never ask, and nothing asks in a
// sandbox app except removing something. `--yes` answers for a script; without a terminal
// (or under --json) a write that would ask is refused until it is given.

// confirmWrite asks before a request that changes something in a production app.
func (c *ctx) confirmWrite(creds *api.Credentials, what string) {
	if creds.App.Sandbox {
		return
	}
	confirm(fmt.Sprintf("%s in production app %q?", what, creds.App.Name), c.yes)
}

// confirmDelete asks before removing something, in any app, and says so when the app is
// a production one.
func (c *ctx) confirmDelete(creds *api.Credentials, question string) {
	if !creds.App.Sandbox {
		question = fmt.Sprintf("Production app %q: %s", creds.App.Name, question)
	}
	confirm(question, c.yes)
}
