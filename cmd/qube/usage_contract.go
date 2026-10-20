package main

import (
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// usageRoutes asks for one usage contract in its two forms: the JSON route's answer decoded
// into out, and the Markdown route's raw answer. A workflow's contract
// (`workflows usage KEY`) and a shipped template's (`workflows templates KEY --usage`) have the
// same shape, so both print through printUsage. Each caller names its own routes, as a
// literal `v2path(...)` inside the cl.Do / cl.Raw call, where TestV2RoutesHaveCommands reads them.
type usageRoutes struct {
	json     func(out interface{}) error
	markdown func() (body []byte, status int, err error)
}

// printUsage prints a usage contract: under --json the answer's "data", otherwise the host's
// Markdown rendering of it. A refusal (a key the host doesn't have, say) prints the server's
// message and exits 1.
func printUsage(r usageRoutes) {
	if ui.JSON {
		var out struct {
			Data interface{} `json:"data"`
		}
		if err := r.json(&out); err != nil {
			fail(err)
		}
		ui.PrintJSON(out.Data)
		return
	}
	body, status, err := r.markdown()
	if err != nil {
		fail(err)
	}
	if status < 200 || status > 299 {
		ui.Fail("HTTP %d: %s", status, answerMessage(body))
	}
	ui.PrintRaw(body)
}
