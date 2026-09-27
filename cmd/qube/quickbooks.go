package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// ---------------------------------------------------------------- connections

func (c *ctx) connections(args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
		args = args[1:]
	}
	switch sub {
	case "list":
		cl, _ := c.appClient()
		var out struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections"), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		var rows [][]string
		for _, cn := range out.Data {
			rows = append(rows, []string{str(cn["id"]), str(cn["name"]), str(cn["type"]), str(cn["last_connected_at"]), str(cn["quickbooks_product_name"])})
		}
		ui.Table(os.Stdout, []string{"ID", "NAME", "TYPE", "LAST CONNECTED", "QUICKBOOKS"}, rows, "No connections yet: `qube connections create --simulated` makes one that is connected at once.")
	case "create":
		fs := flag.NewFlagSet("connections create", flag.ExitOnError)
		simulated := fs.Bool("simulated", false, "a fake QuickBooks answered by QuBe Sync (no Windows machine)")
		name := fs.String("name", "", "connection name")
		redirect := fs.String("redirect-url", "", "where onboarding sends the user back (no query string: QuBe adds ?connection_id=...&state=...)")
		parseAnywhere(fs, args)
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Create a connection")
		body := map[string]string{}
		if *simulated {
			body["type"] = "simulated"
		}
		if *name != "" {
			body["name"] = *name
		}
		if *redirect != "" {
			body["redirect_url"] = *redirect
		}
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/connections"), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Created %s connection %s (%s)\n", str(out.Data["type"]), str(out.Data["id"]), str(out.Data["name"]))
		if links, ok := out.Data["links"].(map[string]interface{}); ok {
			if *simulated {
				fmt.Printf("Connected already. Dashboard: %s\n", str(links["ui"]))
			} else {
				fmt.Printf("Send the customer to onboarding: %s\n", str(links["onboarding"]))
			}
		}
	case "show":
		fs := flag.NewFlagSet("connections show", flag.ExitOnError)
		parseAnywhere(fs, args)
		if fs.NArg() < 1 {
			ui.Usage("usage: qube connections show <connection>")
		}
		cl, _ := c.appClient()
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}", fs.Arg(0)), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		var rows [][]string
		for _, f := range []string{"id", "name", "type", "last_connected_at", "quickbooks_product_name", "company_file", "qbxml_version", "username", "redirect_url", "onboarding_token_expires_at"} {
			if v := out.Data[f]; v != nil && str(v) != "" {
				rows = append(rows, []string{f, str(v)})
			}
		}
		if links, ok := out.Data["links"].(map[string]interface{}); ok {
			if v := links["ui"]; v != nil && str(v) != "" {
				rows = append(rows, []string{"dashboard", str(v)})
			}
			if v := links["onboarding"]; v != nil && str(v) != "" {
				rows = append(rows, []string{"onboarding", str(v)})
			}
		}
		ui.Table(os.Stdout, []string{"CONNECTION", ""}, rows, "(nothing to show)")
	case "update":
		fs := flag.NewFlagSet("connections update", flag.ExitOnError)
		name := fs.String("name", "", "connection name")
		redirect := fs.String("redirect-url", "", "where onboarding sends the user back (no query string: QuBe adds ?connection_id=...&state=...)")
		parseAnywhere(fs, args)
		if fs.NArg() < 1 {
			ui.Usage("usage: qube connections update <connection> [--name N] [--redirect-url U]")
		}
		body := map[string]string{}
		if *name != "" {
			body["name"] = *name
		}
		if *redirect != "" {
			body["redirect_url"] = *redirect
		}
		if len(body) == 0 {
			ui.Usage("usage: qube connections update <connection> [--name N] [--redirect-url U]")
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Update connection "+fs.Arg(0))
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("PUT", v2path("/connections/{connection_id}", fs.Arg(0)), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Updated %s (%s)\n", str(out.Data["id"]), str(out.Data["name"]))
	case "delete":
		fs := flag.NewFlagSet("connections delete", flag.ExitOnError)
		parseAnywhere(fs, args)
		if fs.NArg() < 1 {
			ui.Usage("usage: qube connections delete <connection> [--yes]")
		}
		id := fs.Arg(0)
		cl, creds := c.appClient()
		var show struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}", id), nil, nil, &show); err != nil {
			fail(err)
		}
		c.confirmDelete(creds, fmt.Sprintf("Remove connection %q (%s) and every queued request on it?", str(show.Data["name"]), id))
		if err := cl.Do("DELETE", v2path("/connections/{connection_id}", id), nil, nil, nil); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"id": id, "deleted": true})
			return
		}
		fmt.Printf("Removed connection %s.\n", id)
	case "qwc":
		fs := flag.NewFlagSet("connections qwc", flag.ExitOnError)
		output := fs.String("output", "", "write the .qwc file here instead of stdout")
		parseAnywhere(fs, args)
		if fs.NArg() < 1 {
			ui.Usage("usage: qube connections qwc <connection> [--output FILE]")
		}
		cl, _ := c.appClient()
		var out struct {
			QWC string `json:"qwc"`
		}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/qwc", fs.Arg(0)), nil, nil, &out); err != nil {
			fail(err)
		}
		if *output == "" {
			if ui.JSON {
				ui.PrintJSON(map[string]interface{}{"qwc": out.QWC})
				return
			}
			ui.PrintRaw([]byte(out.QWC))
			return
		}
		if err := os.WriteFile(*output, []byte(out.QWC), 0o644); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"path": *output})
			return
		}
		fmt.Printf("Wrote %s. Add it to the Web Connector on the QuickBooks machine (Add an application).\n", *output)
	case "password":
		fs := flag.NewFlagSet("connections password", flag.ExitOnError)
		stdin := fs.Bool("stdin", false, "read the new password from stdin instead of letting QuBe generate one")
		parseAnywhere(fs, args)
		if fs.NArg() < 1 {
			ui.Usage("usage: qube connections password <connection> [--stdin]")
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Replace the Web Connector password of connection "+fs.Arg(0))
		var body interface{}
		if *stdin {
			raw, err := io.ReadAll(os.Stdin)
			if err != nil {
				fail(err)
			}
			pw := strings.TrimRight(string(raw), "\r\n \t")
			if pw == "" {
				ui.Usage("usage: qube connections password <connection> --stdin (stdin had no password)")
			}
			body = map[string]string{"password": pw}
		}
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/password", fs.Arg(0)), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Web Connector password for %s: %s\n", str(out.Data["id"]), str(out.Data["password"]))
		fmt.Println("It replaces the previous one; enter it in the Web Connector when it asks.")
	case "onboarding-url":
		fs := flag.NewFlagSet("connections onboarding-url", flag.ExitOnError)
		parseAnywhere(fs, args)
		if fs.NArg() < 1 {
			ui.Usage("usage: qube connections onboarding-url <connection>")
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Replace the onboarding link of connection "+fs.Arg(0))
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/onboarding_url", fs.Arg(0)), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		links, _ := out.Data["links"].(map[string]interface{})
		expires := str(out.Data["onboarding_token_expires_at"])
		if expires == "" && links != nil {
			expires = str(links["onboarding_token_expires_at"])
		}
		fmt.Printf("Onboarding link for %s (the previous one no longer works; this one expires %s):\n", str(out.Data["id"]), expires)
		if links != nil {
			fmt.Println(str(links["onboarding"]))
		}
	default:
		ui.Usage("usage: qube connections list | create [--simulated] [--name N] [--redirect-url U] | show <connection> | update <connection> [--name N] [--redirect-url U] | delete <connection> [--yes] | qwc <connection> [--output FILE] | password <connection> [--stdin] | onboarding-url <connection>")
	}
}

// ---------------------------------------------------------------- requests

type requestPage struct {
	Data []map[string]interface{} `json:"data"`
	Meta *struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
	} `json:"meta"`
}

func (c *ctx) requests(args []string) {
	if len(args) < 2 {
		ui.Usage("usage: qube requests list <connection> [flags] | show <connection> <id> | pages <connection> <id> | tail <connection> | discard <connection> <id>")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("requests list", flag.ExitOnError)
		page := fs.Int("page", 1, "page number")
		size := fs.Int("page-size", 20, "requests per page")
		state := fs.String("state", "", "waiting|retryable|ticketed|sent|response_received|error|timed_out|discarded")
		webhookState := fs.String("webhook-state", "", "not_applicable|pending|succeeded|failed")
		search := fs.String("search", "", "search phrase")
		sortBy := fs.String("sort", "", "inserted_at|updated_at")
		sortDir := fs.String("sort-direction", "", "asc|desc")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube requests list <connection> [--page N] [--page-size N] [--state S] [--webhook-state S] [--search TEXT] [--sort inserted_at|updated_at] [--sort-direction asc|desc]")
		}
		cl, _ := c.appClient()
		var out requestPage
		q := url.Values{"page": {strconv.Itoa(*page)}, "page_size": {strconv.Itoa(*size)}}
		if *state != "" {
			q.Set("state", *state)
		}
		if *webhookState != "" {
			q.Set("webhook_state", *webhookState)
		}
		if *search != "" {
			q.Set("search_phrase", *search)
		}
		if *sortBy != "" {
			q.Set("sort", *sortBy)
		}
		if *sortDir != "" {
			q.Set("sort_direction", *sortDir)
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}/queued_requests", fs.Arg(0)), q, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out)
			return
		}
		if len(out.Data) == 0 && *page > 1 {
			ui.Info("nothing on page %d", *page)
		} else {
			printRequests(out.Data)
		}
		if out.Meta != nil && out.Meta.TotalPages > 1 {
			ui.Info("page %d of %d (--page N for the others)", out.Meta.Page, out.Meta.TotalPages)
		}
	case "show":
		if len(args) < 3 {
			ui.Usage("usage: qube requests show <connection> <id> (a request is addressed through its connection)")
		}
		cl, _ := c.appClient()
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}/queued_requests/{id}", args[1], args[2]), nil, nil, &out); err != nil {
			fail(err)
		}
		ui.PrintJSON(out.Data)
	case "pages":
		if len(args) < 3 {
			ui.Usage("usage: qube requests pages <connection> <id>")
		}
		cl, _ := c.appClient()
		var out struct {
			Data interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}/queued_requests/{id}/pages", args[1], args[2]), nil, nil, &out); err != nil {
			fail(err)
		}
		ui.PrintJSON(out.Data)
	case "discard":
		if len(args) < 3 {
			ui.Usage("usage: qube requests discard <connection> <id>")
		}
		connection, id := args[1], args[2]
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Discard request "+id)
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/queued_requests/{id}/discard", connection, id), nil, nil, &out); err != nil {
			var apiErr *api.Error
			if errors.As(err, &apiErr) && apiErr.Status == 409 {
				if msg := discardConflict(id, apiErr.Message); msg != "" {
					ui.Fail("%s", msg)
				}
			}
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Discarded %s. It won't be sent to QuickBooks, and a webhook with state \"discarded\" goes to its webhook_url if it has one.\n", id)
	case "tail":
		// A developer convenience for watching a connection in a terminal. Integrations do not
		// do this: they pass webhook_url and are told when a request is answered.
		cl, _ := c.appClient()
		seen := map[string]string{}
		ui.Info("watching %s (Ctrl-C stops)", args[1])
		for {
			var out requestPage
			if err := cl.Do("GET", v2path("/connections/{connection_id}/queued_requests", args[1]), url.Values{"page_size": {"10"}}, nil, &out); err != nil {
				fail(err)
			}
			for i := len(out.Data) - 1; i >= 0; i-- {
				r := out.Data[i]
				id, state := str(r["id"]), str(r["state"])
				if seen[id] != state {
					seen[id] = state
					if ui.JSON {
						ui.PrintJSON(map[string]interface{}{"at": time.Now().UTC().Format(time.RFC3339), "id": id, "state": state, "request_types": strs(r["request_types"]), "webhook_state": r["webhook_state"]})
					} else {
						fmt.Printf("%s  %-18s %-16s %s\n", time.Now().Format("15:04:05"), state, short(id), strings.Join(strs(r["request_types"]), ","))
					}
				}
			}
			select {
			case <-c.bg.Done():
				os.Exit(ui.ExitInterrupted)
			case <-time.After(3 * time.Second):
			}
		}
	default:
		ui.Usage("usage: qube requests list <connection> [flags] | show <connection> <id> | pages <connection> <id> | tail <connection> | discard <connection> <id>")
	}
}

// discardConflict turns the code on a 409 from `discard` into a clear sentence, or ""
// when the code isn't one it recognizes (the caller then shows the error as-is).
func discardConflict(id, code string) string {
	switch code {
	case "in_flight":
		return fmt.Sprintf("The Web Connector already has %s, so it can't be discarded: it will be answered or time out.", id)
	case "already_finished":
		return fmt.Sprintf("%s has already ended, so there is nothing to discard.", id)
	default:
		return ""
	}
}

func printRequests(rows []map[string]interface{}) {
	var t [][]string
	for _, r := range rows {
		t = append(t, []string{str(r["id"]), str(r["state"]), str(r["webhook_state"]), strings.Join(strs(r["request_types"]), ","), str(r["inserted_at"])})
	}
	ui.Table(os.Stdout, []string{"ID", "STATE", "WEBHOOK", "REQUEST", "QUEUED AT"}, t, "No requests on this connection yet.")
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8] + "…"
	}
	return id
}

// ---------------------------------------------------------------- simulator

func (c *ctx) simulator(args []string) {
	if len(args) < 2 {
		ui.Usage("usage: qube simulator show|reset|sync|faults <connection> [flags]")
	}
	cl, creds := c.appClient()
	connection := args[1]
	if args[0] != "show" {
		c.confirmWrite(creds, "Change the simulator of connection "+connection)
	}
	var out map[string]interface{}
	switch args[0] {
	case "show":
		if err := cl.Do("GET", v2path("/connections/{connection_id}/simulator", connection), nil, nil, &out); err != nil {
			fail(err)
		}
		data, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(data)
			return
		}
		printSimulator(data)
	case "reset":
		if err := cl.Do("POST", v2path("/connections/{connection_id}/simulator/reset", connection), nil, nil, &out); err != nil {
			fail(err)
		}
		data, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(data)
			return
		}
		fmt.Println("Company file reset to its seeded state; every fault cleared.")
		printSimulator(data)
	case "sync":
		if err := cl.Do("POST", v2path("/connections/{connection_id}/simulator/sync", connection), nil, nil, &out); err != nil {
			fail(err)
		}
		data, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(data)
			return
		}
		fmt.Println(syncSummary(data, connection))
	case "faults":
		fs := flag.NewFlagSet("simulator faults", flag.ExitOnError)
		qb := fs.String("qb", "", "closed | modal | mismatch | unexpected | ok  (sticky connection fault)")
		next := fs.String("next", "", "xml | 3100 | 3120 | 3140 | 3180 | 3200 | ok  (one-shot, next request)")
		latency := fs.Int("latency", -1, "milliseconds QuickBooks thinks before each answer")
		parseAnywhere(fs, args[2:])
		if err := cl.Do("GET", v2path("/connections/{connection_id}/simulator", connection), nil, nil, &out); err != nil {
			fail(err)
		}
		faults := map[string]interface{}{}
		if data, ok := out["data"].(map[string]interface{}); ok {
			if f, ok := data["faults"].(map[string]interface{}); ok {
				faults = f
			}
		}
		if err := applyFaultFlags(faults, *qb, *next, *latency); err != nil {
			ui.Usage("%v", err)
		}
		if err := cl.Do("PUT", v2path("/connections/{connection_id}/simulator", connection), nil, map[string]interface{}{"faults": faults}, &out); err != nil {
			fail(err)
		}
		data, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(data)
			return
		}
		f, _ := data["faults"].(map[string]interface{})
		if len(f) == 0 {
			fmt.Println("No faults: every request is answered normally.")
		} else {
			fmt.Printf("Faults now: %s\n", compactJSON(f))
			fmt.Println("Queue a request (or `qube simulator sync`) to see them.")
		}
	default:
		ui.Usage("usage: qube simulator show|reset|sync|faults <connection>")
	}
}

// applyFaultFlags edits the simulator's fault map the way the flags ask.
func applyFaultFlags(faults map[string]interface{}, qb, next string, latency int) error {
	switch qb {
	case "":
	case "ok":
		delete(faults, "connection_error")
	case "closed":
		faults["connection_error"] = "qb_closed"
	case "modal":
		faults["connection_error"] = "modal_dialog"
	case "mismatch":
		faults["connection_error"] = "company_file_mismatch"
	case "unexpected":
		faults["connection_error"] = "unexpected"
	default:
		return fmt.Errorf("--qb must be closed|modal|mismatch|unexpected|ok (got %q)", qb)
	}
	switch next {
	case "":
	case "ok":
		delete(faults, "next_response_error")
		delete(faults, "next_status")
	case "xml":
		delete(faults, "next_status")
		faults["next_response_error"] = "xml_error"
	default:
		code, err := strconv.Atoi(next)
		if err != nil {
			return fmt.Errorf("--next must be xml|<QuickBooks status code>|ok (got %q)", next)
		}
		delete(faults, "next_response_error")
		faults["next_status"] = map[string]interface{}{"code": code}
	}
	if latency >= 0 {
		if latency == 0 {
			delete(faults, "latency_ms")
		} else {
			faults["latency_ms"] = latency
		}
	}
	return nil
}

// syncSummary is the human-readable line for one `simulator sync` result.
func syncSummary(data map[string]interface{}, connection string) string {
	switch str(data["result"]) {
	case "nothing_to_do":
		return "Nothing was waiting: everything queued had already been answered."
	case "connection_error":
		return "The session failed with the connection fault that is set. The request it was sending is retried next session; while the fault lasts, QuBe spaces sessions out, up to 30 minutes apart."
	case "postponed":
		retryIn, _ := data["retry_in"].(float64)
		return fmt.Sprintf("QuickBooks couldn't be reached on the last tries, so QuBe asked the Web Connector to wait %ds. Run `qube simulator sync %s` again to go ahead, as clicking Update Selected again would.", int(retryIn), connection)
	default:
		return fmt.Sprintf("Session done: %v answered, %v failed.", data["answered"], data["errors"])
	}
}

func printSimulator(data map[string]interface{}) {
	if company, ok := data["company"].(map[string]interface{}); ok {
		keys := make([]string, 0, len(company))
		for k := range company {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var rows [][]string
		for _, k := range keys {
			rows = append(rows, []string{k, compactJSON(company[k])})
		}
		ui.Table(os.Stdout, []string{"COMPANY FILE", ""}, rows, "(empty company file)")
	}
	if f, ok := data["faults"].(map[string]interface{}); ok && len(f) > 0 {
		fmt.Printf("Faults: %s\n", compactJSON(f))
	} else {
		fmt.Println("Faults: none")
	}
	if links, ok := data["links"].(map[string]interface{}); ok {
		fmt.Printf("Dashboard: %s\n", str(links["ui"]))
	}
}

func compactJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return str(v)
	}
	return string(b)
}

// ---------------------------------------------------------------- workflows

// workflowIssue is one entry of the `errors` or `warnings` array `POST /workflows/validate`
// answers with.
type workflowIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (c *ctx) workflows(args []string) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		cl, _ := c.appClient()
		var out struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/workflows"), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		var rows [][]string
		for _, w := range out.Data {
			rows = append(rows, []string{str(w["key"]), str(w["name"]), str(w["state"]), str(w["published_version"]), str(w["updated_at"])})
		}
		ui.Table(os.Stdout, []string{"KEY", "NAME", "STATE", "PUBLISHED", "UPDATED"}, rows, "No workflows yet: `qube workflows push chart.json` creates one.")
	case "show":
		if len(args) < 2 {
			ui.Usage("usage: qube workflows show KEY")
		}
		cl, _ := c.appClient()
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/workflows/{key}", args[1]), nil, nil, &out); err != nil {
			fail(err)
		}
		ui.PrintJSON(out.Data)
	case "push":
		fs := flag.NewFlagSet("workflows push", flag.ExitOnError)
		publish := fs.Bool("publish", false, "publish as the next version in the same call")
		notes := fs.String("notes", "", "a note for the version history")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows push FILE [--publish] [--notes TEXT]")
		}
		data, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			fail(err)
		}
		key, body, err := pushBody(data, *publish, *notes)
		if err != nil {
			ui.Fail("%s: %v", fs.Arg(0), err)
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Push workflow "+key+publishing(*publish))
		var out map[string]interface{}
		if err := cl.Do("PUT", v2path("/workflows/{key}", key), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out["data"])
			return
		}
		d, _ := out["data"].(map[string]interface{})
		published := "not published"
		if d["published_version"] != nil {
			published = fmt.Sprintf("published version %v", d["published_version"])
		}
		fmt.Printf("Pushed %s (%s, %s)\n", key, str(d["state"]), published)
	case "validate":
		if len(args) < 2 {
			ui.Usage("usage: qube workflows validate FILE")
		}
		file := args[1]
		raw, err := os.ReadFile(file)
		if err != nil {
			fail(err)
		}
		var definition interface{}
		if err := json.Unmarshal(raw, &definition); err != nil {
			ui.Fail("%s: not JSON: %v", file, err)
		}
		cl, _ := c.appClient()
		var out struct {
			Valid    bool            `json:"valid"`
			Errors   []workflowIssue `json:"errors"`
			Warnings []workflowIssue `json:"warnings"`
		}
		if err := cl.Do("POST", v2path("/workflows/validate"), nil, map[string]interface{}{"definition": definition}, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out)
			if !out.Valid {
				os.Exit(ui.ExitFailure)
			}
			return
		}
		if out.Valid {
			fmt.Printf("%s is valid.\n", file)
		} else {
			fmt.Printf("%s has %d error(s):\n", file, len(out.Errors))
			for _, e := range out.Errors {
				fmt.Printf("  %s: %s\n", e.Path, e.Message)
			}
		}
		if len(out.Warnings) > 0 {
			fmt.Printf("%d warning(s):\n", len(out.Warnings))
			for _, w := range out.Warnings {
				fmt.Printf("  %s: %s\n", w.Path, w.Message)
			}
		}
		if !out.Valid {
			os.Exit(ui.ExitFailure)
		}
	case "publish":
		fs := flag.NewFlagSet("workflows publish", flag.ExitOnError)
		notes := fs.String("notes", "", "a note for the version history")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows publish KEY [--notes TEXT]")
		}
		var body interface{}
		if *notes != "" {
			body = map[string]string{"notes": *notes}
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Publish workflow "+fs.Arg(0))
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/workflows/{key}/publish", fs.Arg(0)), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Published %s as version %v\n", fs.Arg(0), out.Data["published_version"])
	case "unpublish":
		if len(args) < 2 {
			ui.Usage("usage: qube workflows unpublish KEY")
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Unpublish workflow "+args[1])
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/workflows/{key}/unpublish", args[1]), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Unpublished %s (%s).\n", args[1], str(out.Data["state"]))
	case "delete":
		fs := flag.NewFlagSet("workflows delete", flag.ExitOnError)
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows delete KEY [--yes]")
		}
		key := fs.Arg(0)
		cl, creds := c.appClient()
		c.confirmDelete(creds, fmt.Sprintf("Delete workflow %q and its published versions?", key))
		if err := cl.Do("DELETE", v2path("/workflows/{key}", key), nil, nil, nil); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"key": key, "deleted": true})
			return
		}
		fmt.Printf("Deleted workflow %s. Its runs stay readable by id.\n", key)
	case "versions":
		if len(args) < 2 {
			ui.Usage("usage: qube workflows versions KEY [NUMBER]")
		}
		key := args[1]
		cl, _ := c.appClient()
		if len(args) >= 3 {
			var out struct {
				Data map[string]interface{} `json:"data"`
			}
			if err := cl.Do("GET", v2path("/workflows/{key}/versions/{number}", key, args[2]), nil, nil, &out); err != nil {
				fail(err)
			}
			ui.PrintJSON(out.Data)
			return
		}
		var out struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/workflows/{key}/versions", key), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		var rows [][]string
		for _, v := range out.Data {
			rows = append(rows, []string{str(v["number"]), str(v["published_at"]), str(v["published_by"]), str(v["notes"])})
		}
		ui.Table(os.Stdout, []string{"VERSION", "PUBLISHED AT", "BY", "NOTES"}, rows, "No published versions of "+key+" yet.")
	case "usage":
		fs := flag.NewFlagSet("workflows usage", flag.ExitOnError)
		version := fs.String("version", "", "a version number instead of the published one")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows usage KEY [--version V]")
		}
		key := fs.Arg(0)
		var q url.Values
		if *version != "" {
			q = url.Values{"version": {*version}}
		}
		cl, _ := c.appClient()
		if ui.JSON {
			var out struct {
				Data interface{} `json:"data"`
			}
			if err := cl.Do("GET", v2path("/workflows/{key}/usage", key), q, nil, &out); err != nil {
				fail(err)
			}
			ui.PrintJSON(out.Data)
			return
		}
		body, status, err := cl.Raw("GET", v2path("/workflows/{key}/usage.md", key), q, nil)
		if err != nil {
			fail(err)
		}
		if status < 200 || status > 299 {
			os.Stderr.Write(body)
			os.Exit(ui.ExitFailure)
		}
		ui.PrintRaw(body)
	case "schema":
		cl, _ := c.appClient()
		var out interface{}
		if err := cl.Do("GET", v2path("/workflows/schema"), nil, nil, &out); err != nil {
			fail(err)
		}
		ui.PrintJSON(out)
	case "templates":
		cl, _ := c.appClient()
		if len(args) >= 2 {
			var out struct {
				Data map[string]interface{} `json:"data"`
			}
			if err := cl.Do("GET", v2path("/workflow_templates/{key}", args[1]), nil, nil, &out); err != nil {
				fail(err)
			}
			ui.PrintJSON(out.Data)
			return
		}
		var out struct {
			Data []map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/workflow_templates"), nil, nil, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		var rows [][]string
		for _, t := range out.Data {
			rows = append(rows, []string{str(t["key"]), str(t["name"]), strings.Join(strs(t["installs"]), ",")})
		}
		ui.Table(os.Stdout, []string{"KEY", "NAME", "INSTALLS"}, rows, "No workflow templates on this host.")
	case "install":
		fs := flag.NewFlagSet("workflows install", flag.ExitOnError)
		publish := fs.Bool("publish", false, "publish the installed workflow (and its children) immediately")
		notes := fs.String("notes", "", "a note for the version history")
		as := fs.String("as", "", "install under a different key")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows install KEY [--publish] [--notes TEXT] [--as NEW_KEY]")
		}
		fields := map[string]interface{}{}
		if *publish {
			fields["publish"] = true
		}
		if *notes != "" {
			fields["notes"] = *notes
		}
		if *as != "" {
			fields["as"] = *as
		}
		var body interface{}
		if len(fields) > 0 {
			body = fields
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Install template "+fs.Arg(0)+publishing(*publish))
		var out struct {
			Data      map[string]interface{} `json:"data"`
			Installed []string               `json:"installed"`
		}
		if err := cl.Do("POST", v2path("/workflow_templates/{key}/install", fs.Arg(0)), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out)
			return
		}
		installedKey := str(out.Data["key"])
		fmt.Printf("Installed %s as %s (%s).\n", fs.Arg(0), installedKey, str(out.Data["state"]))
		if len(out.Installed) > 1 {
			var others []string
			for _, k := range out.Installed {
				if k != installedKey {
					others = append(others, k)
				}
			}
			if len(others) > 0 {
				fmt.Printf("Also created: %s\n", strings.Join(others, ", "))
			}
		}
	case "run":
		fs := flag.NewFlagSet("workflows run", flag.ExitOnError)
		conn := fs.String("connection", "", "connection id")
		input := fs.String("input", "{}", "JSON input, or @file")
		version := fs.String("version", "", "published | working | a version number (sandbox only)")
		webhook := fs.String("webhook-url", "", "where to send workflow_run.* events")
		wait := &waitFlag{}
		fs.Var(wait, "wait", "wait until the run ends or needs a decision (--wait=30m for longer than 10m)")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 || *conn == "" {
			ui.Usage("usage: qube workflows run KEY --connection C [--input JSON|@file] [--version V] [--webhook-url U] [--wait]")
		}
		in := readJSONArg(*input)
		body := map[string]interface{}{"workflow": fs.Arg(0), "input": in}
		if *version != "" {
			body["version"] = *version
		}
		if *webhook != "" {
			body["webhook_url"] = *webhook
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, fmt.Sprintf("Run workflow %s on connection %s", fs.Arg(0), *conn))
		var out map[string]interface{}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/workflow_runs", *conn), nil, body, &out); err != nil {
			fail(err)
		}
		d, _ := out["data"].(map[string]interface{})
		if wait.on {
			ui.Info("Started run %s of %s.", str(d["id"]), str(d["workflow"]))
			printRun(*conn, c.waitForRun(cl, *conn, d, wait.limit))
			return
		}
		if ui.JSON {
			ui.PrintJSON(d)
			return
		}
		fmt.Printf("Started run %s of %s (%s)\n", str(d["id"]), str(d["workflow"]), str(d["state"]))
		if links, ok := d["links"].(map[string]interface{}); ok && links["ui"] != nil {
			fmt.Printf("Watch it: %s\n", str(links["ui"]))
		}
		if *webhook == "" {
			ui.Info("(no --webhook-url: `qube workflows runs %s %s` shows how it ends, or --wait waits for it)", *conn, str(d["id"]))
		}
	case "runs":
		fs := flag.NewFlagSet("workflows runs", flag.ExitOnError)
		events := fs.Bool("events", false, "print the run's event stream")
		state := fs.String("state", "", "filter the list by run state")
		outcome := fs.String("outcome", "", "filter the list by run outcome")
		after := fs.String("after", "", "resume the event stream after this sequence number")
		limit := fs.Int("limit", 0, "max events to return")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows runs <connection> [run-id] [--events] [--state S] [--outcome O] [--after SEQ] [--limit N]")
		}
		cl, _ := c.appClient()
		connection := fs.Arg(0)
		switch {
		case fs.NArg() >= 2 && *events:
			q := url.Values{}
			if *after != "" {
				q.Set("after", *after)
			}
			if *limit > 0 {
				q.Set("limit", strconv.Itoa(*limit))
			}
			var out map[string]interface{}
			if err := cl.Do("GET", v2path("/connections/{connection_id}/workflow_runs/{run_id}/events", connection, fs.Arg(1)), q, nil, &out); err != nil {
				fail(err)
			}
			ui.PrintJSON(out)
			if !ui.JSON {
				if na, ok := out["next_after"]; ok && na != nil {
					ui.Info("more: --after %v", na)
				}
			}
		case fs.NArg() >= 2:
			var out struct {
				Data map[string]interface{} `json:"data"`
			}
			if err := cl.Do("GET", v2path("/connections/{connection_id}/workflow_runs/{run_id}", connection, fs.Arg(1)), nil, nil, &out); err != nil {
				fail(err)
			}
			ui.PrintJSON(out.Data)
		default:
			q := url.Values{}
			if *state != "" {
				q.Set("state", *state)
			}
			if *outcome != "" {
				q.Set("outcome", *outcome)
			}
			var out struct {
				Data []map[string]interface{} `json:"data"`
			}
			if err := cl.Do("GET", v2path("/connections/{connection_id}/workflow_runs", connection), q, nil, &out); err != nil {
				fail(err)
			}
			if ui.JSON {
				ui.PrintJSON(out.Data)
				return
			}
			var rows [][]string
			for _, r := range out.Data {
				rows = append(rows, []string{str(r["id"]), str(r["workflow"]), str(r["state"]), str(r["outcome"]), str(r["current_state"])})
			}
			ui.Table(os.Stdout, []string{"ID", "WORKFLOW", "STATE", "OUTCOME", "AT"}, rows, "No runs on this connection yet.")
		}
	case "decide":
		fs := flag.NewFlagSet("workflows decide", flag.ExitOnError)
		data := fs.String("data", "{}", "JSON fields the option asks for, e.g. '{\"name\":\"New name\"}'")
		wait := &waitFlag{}
		fs.Var(wait, "wait", "wait until the run ends or needs another decision (--wait=30m for longer than 10m)")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 3 {
			ui.Usage("usage: qube workflows decide <connection> <run-id> <option> [--data JSON] [--wait]")
		}
		body := map[string]interface{}{"option": fs.Arg(2), "data": readJSONArg(*data)}
		cl, creds := c.appClient()
		c.confirmWrite(creds, fmt.Sprintf("Answer %q on run %s", fs.Arg(2), fs.Arg(1)))
		var out map[string]interface{}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/workflow_runs/{run_id}/decisions", fs.Arg(0), fs.Arg(1)), nil, body, &out); err != nil {
			fail(err)
		}
		d, _ := out["data"].(map[string]interface{})
		if wait.on {
			printRun(fs.Arg(0), c.waitForRun(cl, fs.Arg(0), d, wait.limit))
			return
		}
		if ui.JSON {
			ui.PrintJSON(d)
			return
		}
		fmt.Printf("Decided %q on run %s: now %s\n", fs.Arg(2), str(d["id"]), str(d["state"]))
	case "cancel":
		fs := flag.NewFlagSet("workflows cancel", flag.ExitOnError)
		reason := fs.String("reason", "", "why the run is being cancelled")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 2 {
			ui.Usage("usage: qube workflows cancel <connection> <run-id> [--reason TEXT]")
		}
		var body interface{}
		if *reason != "" {
			body = map[string]string{"reason": *reason}
		}
		cl, creds := c.appClient()
		c.confirmWrite(creds, "Cancel run "+fs.Arg(1))
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("POST", v2path("/connections/{connection_id}/workflow_runs/{run_id}/cancel", fs.Arg(0), fs.Arg(1)), nil, body, &out); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Cancelled run %s (%s).\n", str(out.Data["id"]), str(out.Data["state"]))
	case "delete-run":
		fs := flag.NewFlagSet("workflows delete-run", flag.ExitOnError)
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 2 {
			ui.Usage("usage: qube workflows delete-run <connection> <run-id> [--yes]")
		}
		id := fs.Arg(1)
		cl, creds := c.appClient()
		c.confirmDelete(creds, fmt.Sprintf("Delete run %s and its step history?", id))
		if err := cl.Do("DELETE", v2path("/connections/{connection_id}/workflow_runs/{run_id}", fs.Arg(0), id), nil, nil, nil); err != nil {
			fail(err)
		}
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"id": id, "deleted": true})
			return
		}
		fmt.Printf("Deleted run %s.\n", id)
	default:
		ui.Usage("usage: qube workflows list|show|push|validate|publish|unpublish|delete|versions|usage|schema|templates|install|run|runs|decide|cancel|delete-run")
	}
}

// pushBody builds the PUT /api/v2/workflows/:key body from a chart file. `name` and
// `description` are sent only when the chart has them: a JSON null would keep the server
// from defaulting the name to the key.
func pushBody(chartJSON []byte, publish bool, notes string) (string, map[string]interface{}, error) {
	var chart map[string]interface{}
	if err := json.Unmarshal(chartJSON, &chart); err != nil {
		return "", nil, fmt.Errorf("not JSON: %w", err)
	}
	key, _ := chart["key"].(string)
	if key == "" {
		return "", nil, fmt.Errorf("the chart needs a top-level \"key\"")
	}
	body := map[string]interface{}{"definition": chart}
	if s, ok := chart["name"].(string); ok && s != "" {
		body["name"] = s
	}
	if s, ok := chart["description"].(string); ok && s != "" {
		body["description"] = s
	}
	if publish {
		body["publish"] = true
	}
	if notes != "" {
		body["notes"] = notes
	}
	return key, body, nil
}

// ---------------------------------------------------------------- raw

func (c *ctx) rawAPI(args []string) {
	fs := flag.NewFlagSet("api", flag.ExitOnError)
	data := fs.String("data", "", "JSON body, or @file, or - for stdin")
	parseAnywhere(fs, args)
	if fs.NArg() < 2 {
		ui.Usage("usage: qube api METHOD PATH [--data JSON|@file|-]  (PATH: /connections/... or /api/v2/connections/...)")
	}
	method, path := strings.ToUpper(fs.Arg(0)), rawPath(fs.Arg(1))
	if !strings.HasPrefix(path, "/api/v1/") && !strings.HasPrefix(path, "/api/v2/") {
		ui.Usage("the path must be a v2 path such as /connections or /api/v2/connections (got %q)", fs.Arg(1))
	}
	var body []byte
	if *data != "" {
		switch {
		case *data == "-":
			body, _ = io.ReadAll(os.Stdin)
		case strings.HasPrefix(*data, "@"):
			var err error
			body, err = os.ReadFile((*data)[1:])
			if err != nil {
				fail(err)
			}
		default:
			body = []byte(*data)
		}
	}
	cl, creds := c.appClient()
	if method != "GET" && method != "HEAD" {
		c.confirmWrite(creds, method+" "+path)
	}
	res, status, err := cl.Raw(method, path, nil, body)
	if err != nil {
		fail(err)
	}
	ui.PrintRaw(res)
	if status < 200 || status > 299 {
		os.Exit(ui.ExitFailure)
	}
}

// rawPath reads a path that names no API version as a v2 one: /connections is
// /api/v2/connections.
func rawPath(p string) string {
	if strings.HasPrefix(p, "/api/") {
		return p
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "/api/v2" + p
}

// publishing finishes a confirmation question for a call that may also publish.
func publishing(publish bool) string {
	if publish {
		return " and publish it"
	}
	return ""
}
