package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ops"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// How long the operation list read from a host is used before it is read again. A
// resource, verb or flag the list doesn't know makes it read again sooner, once the list is
// older than opsRecheckAfter (so a typo doesn't download it every time).
const (
	opsMaxAge       = 24 * time.Hour
	opsRecheckAfter = 5 * time.Minute
)

// qb runs the QuickBooks operations: `qube qb <resource> <verb> <connection> [flags]`.
// They are not written out here: the host's /api/v2/openapi.json lists them (internal/ops),
// so the CLI offers what the host it talks to offers, flags and body fields included.
func (c *ctx) qb(args []string) {
	refresh := false
	var rest []string
	for i, a := range args {
		switch a {
		case "--refresh":
			refresh = true
			continue
		case "--complete":
			c.qbComplete(args[i+1:])
			return
		}
		rest = append(rest, a)
	}
	args = rest
	help := len(args) > 0 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help")
	if help {
		args = args[1:]
	}

	src := c.opsSource()
	ix, fetched := c.loadOps(src, refresh)
	if len(args) == 0 {
		if refresh && !help {
			ui.Info("Read %d QuickBooks operations from %s.", len(ix.Ops), c.host)
		}
		if ui.JSON {
			ui.PrintJSON(summaries(ix.Ops, ""))
			return
		}
		fmt.Printf("QuickBooks operations on %s (%d, as of %s). Each queues a request that the\nconnection's Web Connector brings to QuickBooks; --webhook-url is where the answer is sent.\n\n", c.host, len(ix.Ops), ix.FetchedAt.Local().Format("2006-01-02 15:04"))
		ix.WriteResources(os.Stdout)
		fmt.Println("\n`qube qb <resource>` lists its operations; `qube qb <resource> <verb> --help` shows the flags.")
		return
	}

	resource := args[0]
	if len(ix.Of(resource)) == 0 && recheck(ix, fetched) {
		ix, fetched = c.loadOps(src, true)
	}
	if len(ix.Of(resource)) == 0 {
		msg := fmt.Sprintf("%s has no QuickBooks resource %q (`qube qb` lists them; `qube qb --refresh` reads the list again)", c.host, resource)
		if s := ix.Suggest(resource); len(s) > 0 {
			msg += "; did you mean " + strings.Join(s, ", ") + "?"
		}
		ui.Usage("%s", msg)
	}
	if len(args) == 1 {
		if ui.JSON {
			ui.PrintJSON(summaries(ix.Ops, resource))
			return
		}
		ix.WriteVerbs(os.Stdout, resource)
		fmt.Printf("\n`qube qb %s <verb> --help` shows the flags.\n", resource)
		return
	}

	verb := args[1]
	op := ix.Find(resource, verb)
	if op == nil && recheck(ix, fetched) {
		ix, fetched = c.loadOps(src, true)
		op = ix.Find(resource, verb)
	}
	if op == nil {
		var verbs []string
		for _, o := range ix.Of(resource) {
			verbs = append(verbs, o.Verb)
		}
		ui.Usage("%s has no %q; it has %s", resource, verb, strings.Join(verbs, ", "))
	}
	for _, a := range args[2:] {
		if a == "--help" || a == "-h" {
			help = true
		}
	}
	if help {
		if ui.JSON {
			ui.PrintJSON(op)
			return
		}
		op.WriteHelp(os.Stdout, "qube qb")
		return
	}

	call, err := op.Bind(args[2:], os.Stdin)
	var unknown *ops.UnknownFlagError
	if errors.As(err, &unknown) && recheck(ix, fetched) {
		// The host may have added the flag since the list was read.
		ix, _ = c.loadOps(src, true)
		if op = ix.Find(resource, verb); op != nil {
			call, err = op.Bind(args[2:], os.Stdin)
		}
	}
	if err != nil {
		ui.Usage("%v (see `qube qb %s %s --help`)", err, resource, verb)
	}

	cl, creds := c.appClient()
	if call.Method != "GET" {
		c.confirmWrite(creds, fmt.Sprintf("%s on connection %s", op.Summary, call.Args[0]))
	}
	var body interface{}
	if call.Body != nil {
		body = call.Body
	}
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := cl.Do(call.Method, call.Path, call.Query, body, &out); err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	d := out.Data
	fmt.Printf("Queued %s on connection %s: %s (%s).\n", str(d["id"]), call.Args[0], strings.Join(strs(d["request_types"]), ", "), str(d["state"]))
	if call.Query.Get("webhook_url") == "" {
		ui.Info("QuickBooks answers when the connection's Web Connector next runs. `qube requests show %s %s` shows the answer once it is in; --webhook-url has it sent to you.", call.Args[0], str(d["id"]))
	}
}

// recheck says whether a list that lacks something should be read again: not when it was
// just read, nor when it is only minutes old.
func recheck(ix *ops.Index, fetched bool) bool {
	return !fetched && time.Since(ix.FetchedAt) > opsRecheckAfter
}

// qbComplete prints resource names, or a resource's verbs, from the cache only: shell
// completion must never wait on the network.
func (c *ctx) qbComplete(args []string) {
	ix := c.opsSource().Cached()
	if ix == nil {
		return
	}
	if len(args) == 0 {
		fmt.Println(strings.Join(ix.Resources(), "\n"))
		return
	}
	for _, op := range ix.Of(args[0]) {
		fmt.Println(op.Verb)
	}
}

func (c *ctx) opsSource() *ops.Source {
	return &ops.Source{
		Host:     c.host,
		CacheDir: opsCacheDir(),
		MaxAge:   opsMaxAge,
		Fetch:    c.fetchSpec,
	}
}

// loadOps reads the host's operations (from the cache while it is fresh) or exits.
func (c *ctx) loadOps(src *ops.Source, refresh bool) (*ops.Index, bool) {
	ix, fetched, warning, err := src.Load(refresh)
	if err != nil {
		fail(fmt.Errorf("reading the QuickBooks operations from %s: %w", c.host, err))
	}
	if warning != nil {
		if fetched {
			ui.Warn("%v", warning)
		} else {
			ui.Warn("using the operation list read %s, as %s can't be read now: %v", ix.FetchedAt.Local().Format("2006-01-02 15:04"), c.host, warning)
		}
	}
	return ix, fetched
}

// fetchSpec reads the host's v2 OpenAPI document. It is public: no key is sent.
func (c *ctx) fetchSpec() ([]byte, error) {
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	data, status, err := cl.Raw("GET", "/api/v2/openapi.json", nil, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case status == 404:
		return nil, fmt.Errorf("it has no v2 API (GET /api/v2/openapi.json is a 404; is --host right?)")
	case status < 200 || status > 299:
		return nil, fmt.Errorf("GET /api/v2/openapi.json answered HTTP %d", status)
	}
	return data, nil
}

// opsCacheDir: QUBE_CACHE_DIR, else the platform's cache directory; "" keeps no cache.
func opsCacheDir() string {
	if d := os.Getenv("QUBE_CACHE_DIR"); d != "" {
		return d
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "qube")
}

func summaries(list []ops.Op, resource string) []ops.Summary {
	out := []ops.Summary{}
	for i := range list {
		if resource == "" || list[i].Resource == resource {
			out = append(out, list[i].Summarize())
		}
	}
	return out
}
