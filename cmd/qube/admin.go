package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
	"github.com/qubeintegrations/qube-cli/internal/uuid"
)

// `qube admin ...` (QuBe Sync staff): service health and the read routes under /api/admin,
// across every organization, plus act-as grants and the agents' kill switch. They always use
// the session's own token, never an act-as grant's (api.StaffOnly), and need a session
// approved with staff permissions (`qube login --admin`); the server checks each route's.
// Customer data -- XML, run inputs and outputs, job arguments, event details -- is asked for
// with an explicit flag (--xml, --io, --events, --args, --detail), which maps onto the route's
// include= parameter and needs admin.read_customer_data.

func (c *ctx) admin(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		staffUsage()
		return
	}
	c.requireStaff("use `qube admin`")
	cl := c.staffClient()
	sub, rest := args[0], args[1:]
	switch sub {
	case "status":
		if code := c.adminStatus(cl, rest); code != 0 {
			os.Exit(code)
		}
	case "orgs", "organizations":
		c.adminOrgs(cl, rest)
	case "org", "organization":
		c.adminOrg(cl, rest)
	case "users":
		c.adminUsers(cl, rest)
	case "user":
		c.adminUser(cl, rest)
	case "connections":
		c.adminConnections(cl, rest)
	case "connection":
		c.adminConnection(cl, rest)
	case "requests":
		c.adminRequests(cl, rest)
	case "request":
		c.adminRequest(cl, rest)
	case "runs":
		c.adminRuns(cl, rest)
	case "run":
		c.adminRun(cl, rest)
	case "jobs":
		c.adminJobs(cl, rest)
	case "feedback":
		c.adminFeedback(cl, rest)
	case "mcp-calls":
		c.adminMCPCalls(cl, rest)
	case "tail":
		if err := c.adminTail(cl, rest); err != nil {
			if errors.Is(err, context.Canceled) {
				os.Exit(ui.ExitInterrupted)
			}
			fail(staffError(c.host, err))
		}
	case "events":
		c.adminEvents(cl, rest)
	case "audit":
		c.adminAudit(cl, rest)
	case "grants":
		c.adminGrants(cl, rest)
	case "agents":
		c.adminAgents(cl, rest)
	case "integration-requests":
		c.adminIntegrationRequests(cl, rest)
	case "integration-request":
		c.adminIntegrationRequest(cl, rest)
	case "builds":
		c.adminBuilds(cl, rest)
	case "build":
		c.adminBuild(cl, rest)
	default:
		ui.Usage("unknown admin command %q (try `qube admin help`)", sub)
	}
}

// get reads a staff route into out, stopping the command on an error.
func (c *ctx) get(cl *api.Client, path string, q url.Values, out interface{}) {
	if err := cl.Do("GET", path, q, nil, out); err != nil {
		fail(staffError(c.host, err))
	}
}

// adminFlags is a flag set that also takes --org (and --organization) for the organization
// filter, an id or an exact name.
func adminFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("admin "+name, flag.ExitOnError)
	org := new(string)
	fs.StringVar(org, "org", "", "an organization's id or exact name")
	fs.StringVar(org, "organization", "", "the same as --org")
	return fs, org
}

// orgFilter is --org as an id: a name is looked up as `qube act-as` does.
func (c *ctx) orgFilter(cl *api.Client, ref string) string {
	if ref == "" {
		return ""
	}
	id, _ := c.resolveOrganization(cl, ref)
	return id
}

type listAnswer struct {
	Data []map[string]interface{} `json:"data"`
	Meta map[string]interface{}   `json:"meta"`
}

// pageNote says where the rest of a paged list is: by page, or by cursor.
func pageNote(meta map[string]interface{}, cursorFlag string) {
	if meta == nil {
		return
	}
	if next := str(meta["next_cursor"]); next != "" {
		ui.Info("(more: %s %s)", cursorFlag, next)
		return
	}
	page, _ := meta["page"].(float64)
	if total, ok := meta["total_pages"].(float64); ok && total > 1 {
		ui.Info("page %d of %d (--page N for the others)", int(page), int(total))
	} else if meta["has_next_page"] == true {
		ui.Info("(more: --page %d)", int(page)+1)
	}
}

func setIf(q url.Values, key, value string) {
	if value != "" {
		q.Set(key, value)
	}
}

func setInt(q url.Values, key string, n int) {
	if n > 0 {
		q.Set(key, strconv.Itoa(n))
	}
}

func onePositional(fs *flag.FlagSet, usage string) string {
	if fs.NArg() != 1 {
		ui.Usage("usage: %s", usage)
	}
	return fs.Arg(0)
}

// ---------------------------------------------------------------- status

// adminStatus prints GET /api/admin/status and returns the exit code: with --check, 1 when
// something needs attention.
func (c *ctx) adminStatus(cl *api.Client, args []string) int {
	fs := flag.NewFlagSet("admin status", flag.ExitOnError)
	check := fs.Bool("check", false, "exit 1 when something needs attention (for a script or an agent)")
	parseAnywhere(fs, args)
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	c.get(cl, "/api/admin/status", nil, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
	} else {
		fmt.Print(statusText(c.host, out.Data))
	}
	if *check && out.Data["attention"] == true {
		return ui.ExitFailure
	}
	return 0
}

// statusText is `qube admin status` for a person: a line per section, `!` where one needs
// attention and `?` where it couldn't be read.
func statusText(host string, d map[string]interface{}) string {
	var b strings.Builder
	errs, _ := d["errors"].(map[string]interface{})
	section := func(name string) (map[string]interface{}, string) {
		m, _ := d[name].(map[string]interface{})
		return m, str(errs[name])
	}
	line := func(mark, label, text string) {
		fmt.Fprintf(&b, "%-2s%-15s%s\n", mark, label, text)
	}
	mark := func(m map[string]interface{}) string {
		if m != nil && m["attention"] == true {
			return "!"
		}
		return ""
	}
	unavailable := func(label, reason string) {
		line("?", label, "unavailable: "+orEmpty(reason, "no answer"))
	}

	head := []string{"QuBe Sync", host}
	if n, _ := section("nodes"); n != nil {
		head = append(head, plural(num(n["count"]), "node", "nodes"))
		if r := str(n["release"]); r != "" {
			head = append(head, "release "+r)
		}
		if v := str(n["version"]); v != "" {
			head = append(head, "version "+v)
		}
	}
	if t, err := time.Parse(time.RFC3339, str(d["generated_at"])); err == nil {
		head = append(head, t.UTC().Format("15:04:05 UTC"))
	}
	b.WriteString(strings.Join(head, " · ") + "\n\n")

	wc, wcErr := section("web_connectors")
	soap, soapErr := section("soap_sessions")
	switch {
	case wc == nil && soap == nil:
		unavailable("Web Connector", wcErr+soapErr)
	default:
		var parts []string
		if wc != nil {
			parts = append(parts, fmt.Sprintf("%s seen in the last %d min", plural(num(wc["seen"]), "connection", "connections"), num(wc["window_minutes"])))
		} else {
			parts = append(parts, "connections seen: unavailable ("+wcErr+")")
		}
		if soap != nil {
			parts = append(parts, fmt.Sprintf("%s live", plural(num(soap["live"]), "SOAP session", "SOAP sessions")))
		} else {
			parts = append(parts, "SOAP sessions: unavailable ("+soapErr+")")
		}
		line(mark(wc)+mark(soap), "Web Connector", strings.Join(parts, " · "))
	}

	if q, qErr := section("queues"); q == nil {
		unavailable("Queues", qErr)
	} else {
		line(mark(q), "Queues", fmt.Sprintf("%9s  %9s  %9s  %9s", "available", "executing", "retryable", "scheduled"))
		rows, _ := q["queues"].([]interface{})
		for _, r := range rows {
			m, _ := r.(map[string]interface{})
			fmt.Fprintf(&b, "    %-13s%9d  %9d  %9d  %9d\n", str(m["queue"]), num(m["available"]), num(m["executing"]), num(m["retryable"]), num(m["scheduled"]))
		}
	}

	if f, fErr := section("failed_jobs"); f == nil {
		unavailable("Failed jobs", fErr)
	} else {
		text := fmt.Sprintf("%d discarded in %d h", num(f["discarded"]), num(f["hours"]))
		var workers []string
		list, _ := f["by_worker"].([]interface{})
		for i, w := range list {
			if i == 5 {
				workers = append(workers, "...")
				break
			}
			m, _ := w.(map[string]interface{})
			workers = append(workers, fmt.Sprintf("%d %s", num(m["count"]), strings.TrimPrefix(str(m["worker"]), "Qube.")))
		}
		if len(workers) > 0 {
			text += " (" + strings.Join(workers, ", ") + ")"
		}
		line(mark(f), "Failed jobs", text)
	}

	if w, wErr := section("webhooks_failed"); w == nil {
		unavailable("Webhooks", wErr)
	} else {
		text := fmt.Sprintf("none failed for good in production apps in %d h", num(w["hours"]))
		if n := num(w["count"]); n > 0 {
			text = fmt.Sprintf("%d failed for good in %d h, in %s", n, num(w["hours"]), plural(num(w["production_apps"]), "production app", "production apps"))
		}
		sandbox := 0
		apps, _ := w["by_app"].([]interface{})
		for _, a := range apps {
			if m, _ := a.(map[string]interface{}); m != nil && m["production"] != true {
				sandbox += num(m["count"])
			}
		}
		if sandbox > 0 {
			text += fmt.Sprintf(" (and %d in sandbox apps)", sandbox)
		}
		line(mark(w), "Webhooks", text)
	}

	if r, rErr := section("requests_waiting"); r == nil {
		unavailable("Requests", rErr)
	} else {
		older := minutesWords(num(r["older_than_minutes"]))
		text := fmt.Sprintf("no connection has had requests waiting over %s", older)
		if n := num(r["connections"]); n > 0 {
			text = fmt.Sprintf("%s waiting over %s on %s", plural(num(r["requests"]), "request", "requests"), older, plural(n, "connection", "connections"))
			var top []string
			list, _ := r["top"].([]interface{})
			for _, t := range list {
				m, _ := t.(map[string]interface{})
				top = append(top, fmt.Sprintf("%s %d", orEmpty(joinNames(str(m["organization_name"]), str(m["connection_name"])), str(m["connection_id"])), num(m["requests"])))
			}
			if len(top) > 0 {
				text += " (" + strings.Join(top, ", ") + ")"
			}
		}
		if s := num(r["stalled_connections"]); s > 0 {
			text += fmt.Sprintf(" · %s told requests are stalled", plural(s, "connection's owners", "connections' owners"))
		}
		line(mark(r), "Requests", text)
	}

	if s, sErr := section("stuck_runs"); s == nil {
		unavailable("Runs", sErr)
	} else {
		text := fmt.Sprintf("%d running with no progress in %d min · %d steps swept by the stuck-run reaper in 1 h", num(s["running_without_progress"]), num(s["minutes"]), num(s["steps_swept_last_hour"]))
		if ids := strs(s["run_ids"]); len(ids) > 0 {
			text += " · " + strings.Join(ids, ", ")
		}
		line(mark(s), "Runs", text)
	}

	if e, eErr := section("events"); e == nil {
		unavailable("Events", eErr)
	} else {
		text := fmt.Sprintf("%d in %d h", num(e["count"]), num(e["hours"]))
		if last, ok := e["last"].(map[string]interface{}); ok {
			where := []string{}
			if n := str(last["organization_name"]); n != "" {
				where = append(where, n)
			}
			if last["production"] == true {
				where = append(where, "production")
			}
			text += fmt.Sprintf(" · last: %s at %s", str(last["kind"]), clock(str(last["at"])))
			if len(where) > 0 {
				text += " (" + strings.Join(where, ", ") + ")"
			}
		}
		line(mark(e), "Events", text)
	}

	// a section this CLI doesn't know yet still shows that it failed
	known := map[string]bool{"nodes": true, "web_connectors": true, "soap_sessions": true, "queues": true, "failed_jobs": true, "webhooks_failed": true, "requests_waiting": true, "stuck_runs": true, "events": true}
	var other []string
	for name := range errs {
		if !known[name] {
			other = append(other, name)
		}
	}
	sort.Strings(other)
	for _, name := range other {
		unavailable(name, str(errs[name]))
	}
	if d["attention"] == true {
		b.WriteString("\n! needs attention\n")
	}
	return b.String()
}

func num(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	i, _ := strconv.Atoi(str(v))
	return i
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func minutesWords(m int) string {
	if m > 0 && m%60 == 0 {
		return fmt.Sprintf("%d h", m/60)
	}
	return fmt.Sprintf("%d min", m)
}

func joinNames(names ...string) string {
	var out []string
	for _, n := range names {
		if n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, " / ")
}

// clock is an RFC 3339 time as local "15:04".
func clock(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("15:04")
}

// stamp is an RFC 3339 time as local "2006-01-02 15:04:05".
func stamp(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// ---------------------------------------------------------------- organizations and users

func (c *ctx) adminOrgs(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin orgs", flag.ExitOnError)
	search := fs.String("search", "", "names containing this (any case)")
	page := fs.Int("page", 0, "page number")
	size := fs.Int("page-size", 0, "organizations per page (at most 100)")
	parseAnywhere(fs, args)
	q := url.Values{}
	setIf(q, "search", *search)
	setInt(q, "page", *page)
	setInt(q, "page_size", *size)
	var out listAnswer
	c.get(cl, "/api/admin/organizations", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, o := range out.Data {
		var owners []string
		list, _ := o["owners"].([]interface{})
		for _, ow := range list {
			m, _ := ow.(map[string]interface{})
			owners = append(owners, str(m["email"]))
		}
		rows = append(rows, []string{str(o["id"]), str(o["name"]), strings.Join(owners, ","), str(o["sandbox_status"]), str(o["production_connection_count"]), str(o["created_at"])})
	}
	ui.Table(os.Stdout, []string{"ID", "NAME", "OWNERS", "SANDBOX", "PROD CONNECTIONS", "CREATED"}, rows, "No organizations match.")
	pageNote(out.Meta, "")
}

func (c *ctx) adminOrg(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin org", flag.ExitOnError)
	parseAnywhere(fs, args)
	id, _ := c.resolveOrganization(cl, onePositional(fs, "qube admin org <id or exact name>"))
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	c.get(cl, "/api/admin/organizations/"+url.PathEscape(id), nil, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	d := out.Data
	kv := [][]string{}
	for _, f := range []string{"id", "name", "personal", "created_at", "sandbox_status", "sandbox_access_expires_at", "sandbox_access_disabled_at", "sandbox_permanently_enabled", "workflows_promotion_only", "production_connection_count"} {
		if v := d[f]; v != nil && str(v) != "" {
			kv = append(kv, []string{f, str(v)})
		}
	}
	if cb, ok := d["created_by"].(map[string]interface{}); ok {
		kv = append(kv, []string{"created_by", str(cb["email"])})
	}
	if bl, ok := d["billing"].(map[string]interface{}); ok {
		kv = append(kv, []string{"billing", fmt.Sprintf("active subscription: %v, period ends %v", bl["active_subscription"], str(bl["current_period_end"]))})
	}
	ui.Table(os.Stdout, []string{"FIELD", "VALUE"}, kv, "")
	var apps [][]string
	list, _ := d["apps"].([]interface{})
	for _, a := range list {
		m, _ := a.(map[string]interface{})
		kind := "production"
		if m["sandbox"] == true {
			kind = "sandbox"
		}
		apps = append(apps, []string{str(m["id"]), str(m["name"]), kind, str(m["connections"])})
	}
	fmt.Println()
	ui.Table(os.Stdout, []string{"APP", "NAME", "KIND", "CONNECTIONS"}, apps, "No apps.")
	var members [][]string
	list, _ = d["members"].([]interface{})
	for _, mm := range list {
		m, _ := mm.(map[string]interface{})
		members = append(members, []string{str(m["email"]), str(m["role"]), str(m["user_id"])})
	}
	fmt.Println()
	ui.Table(os.Stdout, []string{"MEMBER", "ROLE", "USER"}, members, "No members.")
}

func (c *ctx) adminUsers(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin users", flag.ExitOnError)
	search := fs.String("search", "", "emails containing this (any case)")
	page := fs.Int("page", 0, "page number")
	size := fs.Int("page-size", 0, "users per page (at most 100)")
	parseAnywhere(fs, args)
	q := url.Values{}
	setIf(q, "search", *search)
	setInt(q, "page", *page)
	setInt(q, "page_size", *size)
	var out listAnswer
	c.get(cl, "/api/admin/users", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, u := range out.Data {
		rows = append(rows, []string{str(u["id"]), str(u["email"]), str(u["name"]), str(u["role"]), str(u["created_at"])})
	}
	ui.Table(os.Stdout, []string{"ID", "EMAIL", "NAME", "ROLE", "CREATED"}, rows, "No users match.")
	pageNote(out.Meta, "")
}

func (c *ctx) adminUser(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin user", flag.ExitOnError)
	parseAnywhere(fs, args)
	ref := onePositional(fs, "qube admin user <id or email>")
	id := ref
	if !uuid.Valid(ref) {
		var found listAnswer
		c.get(cl, "/api/admin/users", url.Values{"search": {ref}, "page_size": {"100"}}, &found)
		id = ""
		for _, u := range found.Data {
			if strings.EqualFold(str(u["email"]), ref) {
				id = str(u["id"])
			}
		}
		if id == "" {
			ui.Fail("no user has the email %q (`qube admin users --search TEXT` lists them)", ref)
		}
	}
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	c.get(cl, "/api/admin/users/"+url.PathEscape(id), nil, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	d := out.Data
	var kv [][]string
	for _, f := range []string{"id", "email", "name", "role", "confirmed_at", "created_at", "current_organization_id"} {
		if v := d[f]; v != nil && str(v) != "" {
			kv = append(kv, []string{f, str(v)})
		}
	}
	ui.Table(os.Stdout, []string{"FIELD", "VALUE"}, kv, "")
	var rows [][]string
	list, _ := d["memberships"].([]interface{})
	for _, mm := range list {
		m, _ := mm.(map[string]interface{})
		rows = append(rows, []string{str(m["organization_id"]), str(m["organization_name"]), str(m["role"])})
	}
	fmt.Println()
	ui.Table(os.Stdout, []string{"ORGANIZATION", "NAME", "ROLE"}, rows, "No memberships.")
}

// ---------------------------------------------------------------- connections, requests, runs

func (c *ctx) adminConnections(cl *api.Client, args []string) {
	fs, org := adminFlags("connections")
	typ := fs.String("type", "", "standard, realtime or simulated")
	stalled := fs.Bool("stalled", false, "only connections whose owners were told requests are stalled")
	page := fs.Int("page", 0, "page number")
	size := fs.Int("page-size", 0, "connections per page (at most 100)")
	parseAnywhere(fs, args)
	q := url.Values{}
	setIf(q, "organization", c.orgFilter(cl, *org))
	setIf(q, "app", c.app)
	setIf(q, "type", *typ)
	if *stalled {
		q.Set("stalled", "true")
	}
	setInt(q, "page", *page)
	setInt(q, "page_size", *size)
	var out listAnswer
	c.get(cl, "/api/admin/connections", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, cn := range out.Data {
		rows = append(rows, []string{str(cn["id"]), str(cn["name"]), str(cn["type"]), ownerLabel(cn), str(cn["last_connected_at"]), str(cn["quickbooks_product_name"])})
	}
	ui.Table(os.Stdout, []string{"ID", "NAME", "TYPE", "ORGANIZATION / APP", "LAST CONNECTED", "QUICKBOOKS"}, rows, "No connections match.")
	pageNote(out.Meta, "")
}

// ownerLabel is "Organization / App" from a record's organization and app briefs, with
// "(sandbox)" for a sandbox app.
func ownerLabel(m map[string]interface{}) string {
	org, _ := m["organization"].(map[string]interface{})
	app, _ := m["app"].(map[string]interface{})
	label := joinNames(str(org["name"]), str(app["name"]))
	if app["sandbox"] == true {
		label += " (sandbox)"
	}
	return label
}

func (c *ctx) adminConnection(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin connection", flag.ExitOnError)
	parseAnywhere(fs, args)
	id := onePositional(fs, "qube admin connection <id>")
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	c.get(cl, "/api/admin/connections/"+url.PathEscape(id), nil, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	d := out.Data
	var kv [][]string
	kv = append(kv, []string{"organization / app", ownerLabel(d)})
	for _, f := range []string{"id", "name", "type", "last_connected_at", "last_work_at", "quickbooks_product_name", "qbxml_version", "company_file", "username", "web_connector_schedule_seconds", "standard_pace", "postponed_until", "connection_errors", "last_connection_error_at", "stalled", "stalled_notified_at", "downgrade_at", "onboarding_reset_at", "created_at", "updated_at"} {
		if f == "stalled" {
			if v := stalledText(d); v != "" {
				kv = append(kv, []string{f, v})
			}
			continue
		}
		if v := d[f]; v != nil && str(v) != "" {
			kv = append(kv, []string{f, str(v)})
		}
	}
	ui.Table(os.Stdout, []string{"FIELD", "VALUE"}, kv, "")
}

// stalledText is a stalled connection's stalled_at and stall_reason (not_checking_in or
// quickbooks_unavailable) on one row: "since 2026-10-22T14:05:00Z (not_checking_in)". Both are
// null while the connection isn't stalled, and the row is left out.
func stalledText(d map[string]interface{}) string {
	at, reason := str(d["stalled_at"]), str(d["stall_reason"])
	switch {
	case at != "" && reason != "":
		return "since " + at + " (" + reason + ")"
	case at != "":
		return "since " + at
	}
	return reason
}

func (c *ctx) adminRequests(cl *api.Client, args []string) {
	const usage = "qube admin requests (--connection ID | --app ID | --org ORG) [--state S] [--webhook-state S] [--since 1h] [--limit N] [--after CURSOR]"
	fs, org := adminFlags("requests")
	state := fs.String("state", "", "waiting, retryable, ticketed, sent, response_received, error, timed_out or discarded")
	webhookState := fs.String("webhook-state", "", "not_applicable, pending, succeeded or failed")
	since := fs.String("since", "", "queued since: 1h, 24h, 7d or an ISO 8601 time")
	limit := fs.Int("limit", 0, "requests per page (at most 100)")
	after := fs.String("after", "", "the cursor a previous page ended with")
	parseAnywhere(fs, args)
	if c.connection == "" && c.app == "" && *org == "" {
		ui.Usage("which requests? one of --connection, --app or --org is required\nusage: %s", usage)
	}
	q := url.Values{}
	setIf(q, "connection", c.connection)
	setIf(q, "app", c.app)
	setIf(q, "organization", c.orgFilter(cl, *org))
	setIf(q, "state", *state)
	setIf(q, "webhook_state", *webhookState)
	setIf(q, "since", *since)
	setInt(q, "limit", *limit)
	setIf(q, "after", *after)
	var out listAnswer
	c.get(cl, "/api/admin/requests", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, r := range out.Data {
		rows = append(rows, []string{str(r["id"]), str(r["state"]), str(r["webhook_state"]), strings.Join(strs(r["request_types"]), ","), requestError(r["error"]), str(r["connection_id"]), str(r["created_at"])})
	}
	ui.Table(os.Stdout, []string{"ID", "STATE", "WEBHOOK", "REQUEST", "ERROR", "CONNECTION", "QUEUED AT"}, rows, "No requests match.")
	pageNote(out.Meta, "--after")
}

func requestError(v interface{}) string {
	e, ok := v.(map[string]interface{})
	if !ok {
		return ""
	}
	return strings.TrimSpace(str(e["type"]) + " " + str(e["code"]))
}

func (c *ctx) adminRequest(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin request", flag.ExitOnError)
	xml := fs.Bool("xml", false, "add the request and response XML and JSON, QuickBooks' messages and the webhook URL (customer data: needs admin.read_customer_data)")
	parseAnywhere(fs, args)
	id := onePositional(fs, "qube admin request <id> [--xml]")
	q := url.Values{}
	if *xml {
		q.Set("include", "xml")
	}
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	c.get(cl, "/api/admin/requests/"+url.PathEscape(id), q, &out)
	ui.PrintJSON(out.Data)
	if t := strs(out.Data["truncated"]); len(t) > 0 {
		ui.Info("(cut at 5 MB: %s)", strings.Join(t, ", "))
	}
}

func (c *ctx) adminRuns(cl *api.Client, args []string) {
	const usage = "qube admin runs (--org ORG | --connection ID | --state S | --stuck) [--limit N] [--after CURSOR]"
	fs, org := adminFlags("runs")
	state := fs.String("state", "", "a run state: running, waiting, succeeded, failed, cancelled...")
	stuck := fs.Bool("stuck", false, "runs running with no progress in 30 minutes")
	limit := fs.Int("limit", 0, "runs per page (at most 100)")
	after := fs.String("after", "", "the cursor a previous page ended with")
	parseAnywhere(fs, args)
	if c.connection == "" && *org == "" && *state == "" && !*stuck {
		ui.Usage("which runs? one of --org, --connection, --state or --stuck is required\nusage: %s", usage)
	}
	q := url.Values{}
	setIf(q, "organization", c.orgFilter(cl, *org))
	setIf(q, "connection", c.connection)
	setIf(q, "state", *state)
	if *stuck {
		q.Set("stuck", "true")
	}
	setInt(q, "limit", *limit)
	setIf(q, "after", *after)
	var out listAnswer
	c.get(cl, "/api/admin/runs", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, r := range out.Data {
		workflow := str(r["workflow_key"])
		if v := str(r["workflow_version"]); v != "" {
			workflow += " v" + v
		}
		rows = append(rows, []string{str(r["id"]), workflow, str(r["state"]), str(r["outcome"]), str(r["current_state"]), ownerLabel(r), str(r["created_at"])})
	}
	ui.Table(os.Stdout, []string{"ID", "WORKFLOW", "STATE", "OUTCOME", "AT", "ORGANIZATION / APP", "STARTED"}, rows, "No runs match.")
	pageNote(out.Meta, "--after")
}

func (c *ctx) adminRun(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin run", flag.ExitOnError)
	events := fs.Bool("events", false, "add 200 entries of the run's log (customer data: needs admin.read_customer_data)")
	io := fs.Bool("io", false, "add its input, variables, output, question and error (customer data: needs admin.read_customer_data)")
	eventsAfter := fs.String("events-after", "", "the log's next page: next_events_cursor from the previous answer")
	parseAnywhere(fs, args)
	id := onePositional(fs, "qube admin run <id> [--events] [--io]")
	var include []string
	if *events || *eventsAfter != "" {
		include = append(include, "events")
	}
	if *io {
		include = append(include, "io")
	}
	q := url.Values{}
	if len(include) > 0 {
		q.Set("include", strings.Join(include, ","))
	}
	setIf(q, "events_after", *eventsAfter)
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	c.get(cl, "/api/admin/runs/"+url.PathEscape(id), q, &out)
	ui.PrintJSON(out.Data)
	if next := str(out.Data["next_events_cursor"]); next != "" {
		ui.Info("(more of the log: --events-after %s)", next)
	}
}

// ---------------------------------------------------------------- jobs, feedback, MCP

func (c *ctx) adminJobs(cl *api.Client, args []string) {
	const usage = "qube admin jobs --state available|scheduled|executing|retryable|completed|discarded|cancelled [--queue Q] [--worker W] [--since 24h] [--limit N] [--before ID] [--args]"
	fs := flag.NewFlagSet("admin jobs", flag.ExitOnError)
	state := fs.String("state", "", "available, scheduled, executing, retryable, completed, discarded or cancelled (required)")
	queue := fs.String("queue", "", "a queue: xml, webhooks, default...")
	worker := fs.String("worker", "", "a worker module, e.g. Qube.SlackAlerts.Worker")
	since := fs.String("since", "", "inserted since: 1h, 24h, 7d or an ISO 8601 time")
	limit := fs.Int("limit", 0, "jobs per page (at most 100)")
	before := fs.String("before", "", "the cursor a previous page ended with")
	withArgs := fs.Bool("args", false, "add each job's args and errors, printed as JSON (customer data: needs admin.read_customer_data)")
	parseAnywhere(fs, args)
	if *state == "" {
		ui.Usage("--state is required\nusage: %s", usage)
	}
	q := url.Values{"state": {*state}}
	setIf(q, "queue", *queue)
	setIf(q, "worker", *worker)
	setIf(q, "since", *since)
	setInt(q, "limit", *limit)
	setIf(q, "before", *before)
	if *withArgs {
		q.Set("include", "args")
	}
	var out listAnswer
	c.get(cl, "/api/admin/jobs", q, &out)
	if ui.JSON || *withArgs {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, j := range out.Data {
		at := str(j["attempted_at"])
		if at == "" {
			at = str(j["scheduled_at"])
		}
		rows = append(rows, []string{str(j["id"]), str(j["state"]), str(j["queue"]), strings.TrimPrefix(str(j["worker"]), "Qube."), fmt.Sprintf("%d/%d", num(j["attempt"]), num(j["max_attempts"])), at})
	}
	ui.Table(os.Stdout, []string{"ID", "STATE", "QUEUE", "WORKER", "ATTEMPT", "AT"}, rows, "No jobs match.")
	pageNote(out.Meta, "--before")
}

func (c *ctx) adminFeedback(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin feedback", flag.ExitOnError)
	status := fs.String("status", "", "new (the default), fixed, wont_fix or all")
	subject := fs.String("subject", "", "reports about this subject")
	parseAnywhere(fs, args)
	q := url.Values{}
	setIf(q, "status", *status)
	setIf(q, "subject", *subject)
	var out listAnswer
	c.get(cl, "/api/admin/feedback", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, f := range out.Data {
		rows = append(rows, []string{str(f["id"]), str(f["status"]), str(f["kind"]), str(f["subject"]), truncate(str(f["summary"]), 60), str(f["inserted_at"])})
	}
	ui.Table(os.Stdout, []string{"ID", "STATUS", "KIND", "SUBJECT", "SUMMARY", "AT"}, rows, "No feedback reports.")
}

func (c *ctx) adminMCPCalls(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin mcp-calls", flag.ExitOnError)
	days := fs.Int("days", 0, "1, 7 (the default) or 30")
	parseAnywhere(fs, args)
	q := url.Values{}
	setInt(q, "days", *days)
	var out listAnswer
	c.get(cl, "/api/admin/mcp-calls", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, t := range out.Data {
		top := ""
		if list, _ := t["top_failures"].([]interface{}); len(list) > 0 {
			m, _ := list[0].(map[string]interface{})
			top = strings.TrimSpace(str(m["outcome"]) + " " + str(m["reason"]))
			if p := str(m["parameter"]); p != "" {
				top += " (" + p + ")"
			}
			top += fmt.Sprintf(" ×%d", num(m["count"]))
		}
		rows = append(rows, []string{str(t["tool"]), str(t["calls"]), str(t["failures"]), top})
	}
	ui.Table(os.Stdout, []string{"TOOL", "CALLS", "FAILURES", "TOP FAILURE"}, rows, "No MCP tool calls.")
	if d := num(out.Meta["days"]); d > 0 {
		ui.Info("(the last %s)", plural(d, "day", "days"))
	}
}

// ---------------------------------------------------------------- audit

func (c *ctx) adminAudit(cl *api.Client, args []string) {
	fs, org := adminFlags("audit")
	actor := fs.String("actor", "", "what this person (email) did")
	token := fs.String("token", "", "what this session, and the grants it asked for, did (a session id)")
	action := fs.String("action", "", "api, admin_api, act_as.requested, act_as.approved, act_as.denied, act_as.ended, act_as.revoked, staff_login.approved, agents.revoked, agents.flag...")
	since := fs.String("since", "", "since: 1h, 24h, 7d or an ISO 8601 time")
	limit := fs.Int("limit", 0, "entries per page (at most 100)")
	before := fs.String("before", "", "the cursor a previous page ended with")
	parseAnywhere(fs, args)
	q := url.Values{}
	setIf(q, "organization", c.orgFilter(cl, *org))
	setIf(q, "actor", *actor)
	setIf(q, "token", *token)
	setIf(q, "action", *action)
	setIf(q, "since", *since)
	setInt(q, "limit", *limit)
	setIf(q, "before", *before)
	var out listAnswer
	c.get(cl, "/api/admin/audit", q, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, e := range out.Data {
		who := str(e["actor_email"])
		if str(e["actor_kind"]) == "agent" {
			who += fmt.Sprintf(" (agent %q)", str(e["agent_name"]))
		}
		what := strings.TrimSpace(str(e["method"]) + " " + str(e["route"]))
		if what == "" {
			what = str(e["resource_id"])
		}
		if r := str(e["reason"]); r != "" && strings.HasPrefix(str(e["action"]), "act_as") {
			what = strings.TrimSpace(what + " " + strconv.Quote(truncate(r, 40)))
		}
		result := strings.TrimSpace(str(e["status"]) + " " + str(e["error_code"]))
		if result == "" {
			result = "outcome unknown"
		}
		if created := str(e["created"]); created != "" {
			result += " → " + created
		}
		rows = append(rows, []string{stamp(str(e["at"])), who, str(e["action"]), what, result})
	}
	ui.Table(os.Stdout, []string{"AT", "ACTOR", "ACTION", "WHAT", "RESULT"}, rows, "No audit entries match.")
	pageNote(out.Meta, "--before")
}

// ---------------------------------------------------------------- grants and agents

func (c *ctx) adminGrants(cl *api.Client, args []string) {
	if len(args) > 0 && args[0] == "revoke" {
		if len(args) != 2 {
			ui.Usage("usage: qube admin grants revoke <grant id>")
		}
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("DELETE", "/api/admin/grants/"+url.PathEscape(args[1]), nil, nil, &out); err != nil {
			fail(staffError(c.host, err))
		}
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		org, _ := out.Data["organization"].(map[string]interface{})
		fmt.Printf("Revoked grant %s (%s).\n", args[1], orEmpty(str(org["name"]), "its organization"))
		if a := c.acting(); a != nil && a.GrantID == args[1] {
			s := c.cfg.Sessions[c.host]
			s.ActingAs = nil
			c.cfg.Sessions[c.host] = s
			if err := c.cfg.Save(); err != nil {
				ui.Warn("saving config: %v", err)
			}
			ui.Info("It was this session's grant: commands use your own session again.")
		}
		return
	}
	if len(args) > 0 {
		ui.Usage("usage: qube admin grants [revoke <grant id>]")
	}
	var out struct {
		Data struct {
			Active  []map[string]interface{} `json:"active"`
			Pending []map[string]interface{} `json:"pending"`
		} `json:"data"`
	}
	c.get(cl, "/api/admin/grants", nil, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	var active [][]string
	for _, g := range out.Data.Active {
		org, _ := g["organization"].(map[string]interface{})
		active = append(active, []string{str(g["id"]), sessionWho(g), str(org["name"]), str(g["scope"]), str(g["access"]), budgetLabel(g["write_budget"]), str(g["expires_at"]), truncate(str(g["reason"]), 50)})
	}
	fmt.Println("Active grants:")
	ui.Table(os.Stdout, []string{"ID", "FOR", "ORGANIZATION", "SCOPE", "ACCESS", "BUDGET", "EXPIRES", "REASON"}, active, "  none")
	var pending [][]string
	for _, p := range out.Data.Pending {
		org, _ := p["organization"].(map[string]interface{})
		by, _ := p["requested_by"].(map[string]interface{})
		who := str(by["email"])
		if str(by["actor"]) == "agent" {
			who += fmt.Sprintf(" (agent %q)", str(by["agent_name"]))
		}
		pending = append(pending, []string{str(p["user_code"]), who, str(org["name"]), str(p["scope"]), str(p["access"]), str(p["minutes"]), str(p["expires_at"]), truncate(str(p["reason"]), 50)})
	}
	fmt.Println("\nWaiting for approval:")
	ui.Table(os.Stdout, []string{"CODE", "ASKED BY", "ORGANIZATION", "SCOPE", "ACCESS", "MINUTES", "EXPIRES", "REASON"}, pending, "  none")
}

// sessionWho is a staff session's person, and its agent's name.
func sessionWho(s map[string]interface{}) string {
	u, _ := s["user"].(map[string]interface{})
	who := str(u["email"])
	if str(s["actor"]) == "agent" {
		who += fmt.Sprintf(" (agent %q)", str(s["agent_name"]))
	}
	return who
}

func budgetLabel(v interface{}) string {
	if v == nil {
		return "-"
	}
	return str(v)
}

func (c *ctx) adminAgents(cl *api.Client, args []string) {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "":
		var out struct {
			Data struct {
				Enabled  bool                     `json:"enabled"`
				Sessions []map[string]interface{} `json:"sessions"`
			} `json:"data"`
		}
		c.get(cl, "/api/admin/agents", nil, &out)
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		state := "on"
		if !out.Data.Enabled {
			state = "OFF: every agent's session holds no staff permissions and every agent's grant is refused"
		}
		fmt.Printf("Agent staff access: %s\n\n", state)
		var rows [][]string
		for _, s := range out.Data.Sessions {
			u, _ := s["user"].(map[string]interface{})
			rows = append(rows, []string{str(s["id"]), str(s["agent_name"]), str(u["email"]), str(s["kind"]), orEmpty(strings.Join(strs(s["permissions"]), ","), "-"), str(s["expires_at"]), str(s["last_used_at"])})
		}
		ui.Table(os.Stdout, []string{"ID", "AGENT", "PERSON", "KIND", "STAFF", "EXPIRES", "LAST USED"}, rows, "No live agent sessions.")
	case "revoke":
		fs := flag.NewFlagSet("admin agents revoke", flag.ExitOnError)
		all := fs.Bool("all", false, "every agent session, and every grant an agent asked for")
		staff := fs.Bool("staff", false, "with --all: also every act-as grant and every session holding staff permissions, people's (this one) included")
		parseAnywhere(fs, args[1:])
		var body map[string]interface{}
		switch {
		case *all && fs.NArg() == 0:
			body = map[string]interface{}{"all": true, "staff": *staff}
			if *staff {
				confirm("Revoke every agent session, every act-as grant and every staff session, this one included?", c.yes)
			}
		case !*all && !*staff && fs.NArg() == 1:
			body = map[string]interface{}{"id": fs.Arg(0)}
		default:
			ui.Usage("usage: qube admin agents revoke <agent session id> | revoke --all [--staff]")
		}
		var out struct {
			Data struct {
				Revoked int `json:"revoked"`
			} `json:"data"`
		}
		if err := cl.Do("POST", "/api/admin/agents/revoke", nil, body, &out); err != nil {
			fail(staffError(c.host, err))
		}
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"revoked": out.Data.Revoked})
			return
		}
		fmt.Printf("Revoked %s.\n", plural(out.Data.Revoked, "session or grant", "sessions and grants"))
		if *staff {
			ui.Info("This session was revoked too: `qube login --admin` again.")
		}
	case "disable", "enable", "off", "on":
		enabled := sub == "enable" || sub == "on"
		var out struct {
			Data struct {
				Enabled bool `json:"enabled"`
			} `json:"data"`
		}
		if err := cl.Do("PUT", "/api/admin/agents/access", nil, map[string]bool{"enabled": enabled}, &out); err != nil {
			fail(staffError(c.host, err))
		}
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"enabled": out.Data.Enabled})
			return
		}
		if out.Data.Enabled {
			fmt.Println("Agent staff access is on: agents' sessions and grants work again.")
		} else {
			fmt.Println("Agent staff access is off: every agent's session holds no staff permissions and every agent's grant is refused, until `qube admin agents enable`.")
		}
	default:
		ui.Usage("usage: qube admin agents [revoke <id> | revoke --all [--staff] | disable | enable]")
	}
}
