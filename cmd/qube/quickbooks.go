package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

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
		if err := cl.Do("GET", "/api/v1/connections", nil, nil, &out); err != nil {
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
		redirect := fs.String("redirect-url", "", "where onboarding sends the user back")
		parseAnywhere(fs, args)
		cl, _ := c.appClient()
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
		if err := cl.Do("POST", "/api/v1/connections", nil, body, &out); err != nil {
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
	default:
		ui.Usage("usage: qube connections list | create [--simulated] [--name N] [--redirect-url U]")
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
		ui.Usage("usage: qube requests list <connection> [--page N] [--page-size N] | show <id> | tail <connection>")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("requests list", flag.ExitOnError)
		page := fs.Int("page", 1, "page number")
		size := fs.Int("page-size", 20, "requests per page")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube requests list <connection> [--page N] [--page-size N]")
		}
		cl, _ := c.appClient()
		var out requestPage
		q := url.Values{"page": {strconv.Itoa(*page)}, "page_size": {strconv.Itoa(*size)}}
		if err := cl.Do("GET", "/api/v1/connections/"+url.PathEscape(fs.Arg(0))+"/queued_requests", q, nil, &out); err != nil {
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
		cl, _ := c.appClient()
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", "/api/v1/queued_requests/"+url.PathEscape(args[1]), nil, nil, &out); err != nil {
			fail(err)
		}
		ui.PrintJSON(out.Data)
	case "tail":
		// A developer convenience for watching a connection in a terminal. Integrations do not
		// do this: they pass webhook_url and are told when a request is answered.
		cl, _ := c.appClient()
		seen := map[string]string{}
		ui.Info("watching %s (Ctrl-C stops)", args[1])
		for {
			var out requestPage
			if err := cl.Do("GET", "/api/v1/connections/"+url.PathEscape(args[1])+"/queued_requests", url.Values{"page_size": {"10"}}, nil, &out); err != nil {
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
		ui.Usage("usage: qube requests list <connection> | show <id> | tail <connection>")
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
	cl, _ := c.appClient()
	base := "/api/v1/connections/" + url.PathEscape(args[1]) + "/simulator"
	var out map[string]interface{}
	switch args[0] {
	case "show":
		if err := cl.Do("GET", base, nil, nil, &out); err != nil {
			fail(err)
		}
		data, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(data)
			return
		}
		printSimulator(data)
	case "reset":
		if err := cl.Do("POST", base+"/reset", nil, nil, &out); err != nil {
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
		if err := cl.Do("POST", base+"/sync", nil, nil, &out); err != nil {
			fail(err)
		}
		data, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(data)
			return
		}
		switch str(data["result"]) {
		case "nothing_to_do":
			fmt.Println("Nothing was waiting: everything queued had already been answered.")
		case "connection_error":
			fmt.Println("The session failed with the connection fault that is set; its requests stay queued as retryable.")
		default:
			fmt.Printf("Session done: %v answered, %v failed.\n", data["answered"], data["errors"])
		}
	case "faults":
		fs := flag.NewFlagSet("simulator faults", flag.ExitOnError)
		qb := fs.String("qb", "", "closed | modal | mismatch | unexpected | ok  (sticky connection fault)")
		next := fs.String("next", "", "xml | 3100 | 3120 | 3140 | 3180 | 3200 | ok  (one-shot, next request)")
		latency := fs.Int("latency", -1, "milliseconds QuickBooks thinks before each answer")
		parseAnywhere(fs, args[2:])
		if err := cl.Do("GET", base, nil, nil, &out); err != nil {
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
		if err := cl.Do("PUT", base, nil, map[string]interface{}{"faults": faults}, &out); err != nil {
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
		if err := cl.Do("GET", "/api/v2/workflows", nil, nil, &out); err != nil {
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
		cl, _ := c.appClient()
		var out map[string]interface{}
		if err := cl.Do("PUT", "/api/v2/workflows/"+url.PathEscape(key), nil, body, &out); err != nil {
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
	case "run":
		fs := flag.NewFlagSet("workflows run", flag.ExitOnError)
		conn := fs.String("connection", "", "connection id")
		input := fs.String("input", "{}", "JSON input, or @file")
		version := fs.String("version", "", "published | working | a version number (sandbox only)")
		webhook := fs.String("webhook-url", "", "where to send workflow_run.* events")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 || *conn == "" {
			ui.Usage("usage: qube workflows run KEY --connection C [--input JSON|@file] [--version V] [--webhook-url U]")
		}
		in := readJSONArg(*input)
		body := map[string]interface{}{"workflow": fs.Arg(0), "input": in}
		if *version != "" {
			body["version"] = *version
		}
		if *webhook != "" {
			body["webhook_url"] = *webhook
		}
		cl, _ := c.appClient()
		var out map[string]interface{}
		if err := cl.Do("POST", "/api/v2/connections/"+url.PathEscape(*conn)+"/workflow_runs", nil, body, &out); err != nil {
			fail(err)
		}
		d, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(d)
			return
		}
		fmt.Printf("Started run %s of %s (%s)\n", str(d["id"]), str(d["workflow"]), str(d["state"]))
		if links, ok := d["links"].(map[string]interface{}); ok && links["ui"] != nil {
			fmt.Printf("Watch it: %s\n", str(links["ui"]))
		}
		if *webhook == "" {
			ui.Info("(no --webhook-url: `qube workflows runs %s %s` shows how it ends)", *conn, str(d["id"]))
		}
	case "runs":
		fs := flag.NewFlagSet("workflows runs", flag.ExitOnError)
		events := fs.Bool("events", false, "print the run's event stream")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 1 {
			ui.Usage("usage: qube workflows runs <connection> [run-id] [--events]")
		}
		cl, _ := c.appClient()
		base := "/api/v2/connections/" + url.PathEscape(fs.Arg(0)) + "/workflow_runs"
		var out map[string]interface{}
		path := base
		if fs.NArg() >= 2 {
			path += "/" + url.PathEscape(fs.Arg(1))
			if *events {
				path += "/events"
			}
		}
		if err := cl.Do("GET", path, nil, nil, &out); err != nil {
			fail(err)
		}
		list, isList := out["data"].([]interface{})
		if ui.JSON || !isList || fs.NArg() >= 2 {
			ui.PrintJSON(out["data"])
			return
		}
		var rows [][]string
		for _, item := range list {
			r, _ := item.(map[string]interface{})
			rows = append(rows, []string{str(r["id"]), str(r["workflow"]), str(r["state"]), str(r["outcome"]), str(r["current_state"])})
		}
		ui.Table(os.Stdout, []string{"ID", "WORKFLOW", "STATE", "OUTCOME", "AT"}, rows, "No runs on this connection yet.")
	case "decide":
		fs := flag.NewFlagSet("workflows decide", flag.ExitOnError)
		data := fs.String("data", "{}", "JSON fields the option asks for, e.g. '{\"name\":\"New name\"}'")
		parseAnywhere(fs, args[1:])
		if fs.NArg() < 3 {
			ui.Usage("usage: qube workflows decide <connection> <run-id> <option> [--data JSON]")
		}
		body := map[string]interface{}{"option": fs.Arg(2), "data": readJSONArg(*data)}
		cl, _ := c.appClient()
		var out map[string]interface{}
		if err := cl.Do("POST", "/api/v2/connections/"+url.PathEscape(fs.Arg(0))+"/workflow_runs/"+url.PathEscape(fs.Arg(1))+"/decisions", nil, body, &out); err != nil {
			fail(err)
		}
		d, _ := out["data"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(d)
			return
		}
		fmt.Printf("Decided %q on run %s: now %s\n", fs.Arg(2), str(d["id"]), str(d["state"]))
	default:
		ui.Usage("usage: qube workflows list|push|run|runs|decide")
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
		ui.Usage("usage: qube api METHOD /api/v2/... [--data JSON|@file|-]")
	}
	method, path := strings.ToUpper(fs.Arg(0)), fs.Arg(1)
	if !strings.HasPrefix(path, "/api/v1/") && !strings.HasPrefix(path, "/api/v2/") {
		ui.Usage("the path must start with /api/v1/ or /api/v2/ (got %q)", path)
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
	cl, _ := c.appClient()
	res, status, err := cl.Raw(method, path, nil, body)
	if err != nil {
		fail(err)
	}
	ui.PrintRaw(res)
	if status < 200 || status > 299 {
		os.Exit(ui.ExitFailure)
	}
}
