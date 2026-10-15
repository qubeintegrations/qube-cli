package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// `qube admin tail` and `qube admin events`: the events the team hears about in Slack, and
// more (sandbox ones, throttled ones, staff activity), kept by QuBe Sync and read with a
// cursor (GET /api/admin/events). `tail` long-polls: the server answers as soon as there are
// events, or 204 after about 25 s; every answer carries the next cursor in x-qube-cursor, and
// the next poll passes it back as `after`. A network error, a 5xx or a 429 backs off (1 s,
// doubling to 30 s, as `qube listen` does) and polls again from the same cursor, so nothing
// is missed or repeated. Ctrl-C stops: adminTail returns context.Canceled, and the command
// exits 130.

// tailWait is the long poll's wait, in seconds, and tailPollDeadline how long one poll may
// take in all (variables so tests can shorten them).
var (
	tailWait         = 25
	tailPollDeadline = 35 * time.Second
)

type eventFilters struct {
	kinds      string
	org        *string // --org: an id or exact name
	production bool
	detail     bool
}

func (c *ctx) eventFlags(name string) (*flag.FlagSet, *eventFilters, *string) {
	fs, org := adminFlags(name)
	f := &eventFilters{org: org}
	fs.StringVar(&f.kinds, "kind", "", "only these kinds, comma-separated (webhook_failed,act_as.started,...)")
	fs.BoolVar(&f.production, "production", false, "only events about production apps' real connections")
	fs.BoolVar(&f.detail, "detail", false, "add each event's detail: endpoint answers, run errors, reasons (customer data: needs admin.read_customer_data)")
	since := fs.String("since", "", "start from this long ago (10m, 24h, 7d) or an ISO 8601 time")
	return fs, f, since
}

func (c *ctx) eventQuery(cl *api.Client, f *eventFilters) url.Values {
	q := url.Values{}
	setIf(q, "kind", strings.ReplaceAll(f.kinds, " ", ""))
	setIf(q, "organization", c.orgFilter(cl, *f.org))
	if f.production {
		q.Set("production", "true")
	}
	if f.detail {
		q.Set("include", "detail")
	}
	return q
}

func (c *ctx) adminTail(cl *api.Client, args []string) error {
	fs, f, since := c.eventFlags("tail")
	parseAnywhere(fs, args)
	if fs.NArg() > 0 {
		ui.Usage("usage: qube admin tail [--kind K,...] [--org ORG] [--production] [--since 10m] [--detail]")
	}
	base := c.eventQuery(cl, f)
	base.Set("wait", strconv.Itoa(tailWait))
	// Without a cursor the stream starts now (or at --since); after the first answer, its
	// cursor (0 when there is no event at all yet) goes back as `after`.
	start := *since
	if start == "" {
		ui.Info("Watching events. Ctrl-C stops.")
	} else {
		ui.Info("Watching events since %s. Ctrl-C stops.", start)
	}
	cursor := ""
	var pause time.Duration
	for {
		q := url.Values{}
		for k, v := range base {
			q[k] = v
		}
		if cursor != "" {
			q.Set("after", cursor)
		} else if start != "" {
			q.Set("since", start)
		}
		pollCtx, cancel := context.WithTimeout(c.bg, tailPollDeadline)
		page, err := cl.AdminEvents(pollCtx, q)
		cancel()
		if c.bg.Err() != nil {
			return context.Canceled
		}
		if err != nil {
			if !retryablePoll(err) && !errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			pause = nextPause(pause, err)
			ui.Info("Lost the connection to %s (%s); reconnecting in %s...", c.host, truncateRunes(shortError(err), 200), pause)
			select {
			case <-c.bg.Done():
				return context.Canceled
			case <-time.After(pause):
			}
			continue
		}
		if pause > 0 {
			ui.Info("Reconnected.")
			pause = 0
		}
		if page.Cursor != "" {
			cursor = page.Cursor
		}
		for _, e := range page.Events {
			printEvent(e, f.detail, false)
		}
	}
}

func (c *ctx) adminEvents(cl *api.Client, args []string) {
	fs, f, since := c.eventFlags("events")
	after := fs.String("after", "", "the cursor a previous page ended with")
	limit := fs.Int("limit", 0, "events per page (at most 200)")
	parseAnywhere(fs, args)
	q := c.eventQuery(cl, f)
	switch {
	case *after != "":
		q.Set("after", *after)
	case *since != "":
		q.Set("since", *since)
	default:
		q.Set("since", "24h")
	}
	setInt(q, "limit", *limit)
	page, err := cl.AdminEvents(c.bg, q)
	if err != nil {
		fail(staffError(c.host, err))
	}
	if ui.JSON {
		events := page.Events
		if events == nil {
			events = []map[string]interface{}{}
		}
		ui.PrintJSON(map[string]interface{}{"data": events, "meta": map[string]interface{}{"cursor": page.Cursor}})
		return
	}
	if len(page.Events) == 0 {
		fmt.Println("No events match.")
	}
	for _, e := range page.Events {
		printEvent(e, f.detail, true)
	}
	if page.Cursor != "" && page.Cursor != "0" {
		ui.Info("(next: --after %s)", page.Cursor)
	}
}

// printEvent prints one event: under --json as a line of JSON, else as a line for a person
// (with the date when long), and its detail beneath when asked for.
func printEvent(e map[string]interface{}, detail, long bool) {
	if ui.JSON {
		printJSONLine(e)
		return
	}
	fmt.Println(eventLine(e, long))
	if !detail {
		return
	}
	d, _ := e["detail"].(map[string]interface{})
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := d[k]; v != nil && str(v) != "" {
			fmt.Printf("          %s: %s\n", k, truncate(str(v), 500))
		}
	}
}

// eventLine is an event as a person reads it: when, the kind, and what it is about.
func eventLine(e map[string]interface{}, long bool) string {
	when := clockSeconds(str(e["at"]))
	if long {
		when = stamp(str(e["at"]))
	}
	return fmt.Sprintf("%s  %-27s %s", when, str(e["kind"]), eventSummary(e))
}

func clockSeconds(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.Local().Format("15:04:05")
}

// eventSummary is what an event is about, from its summary: names, ids and counts, never
// customer text (that is --detail).
func eventSummary(e map[string]interface{}) string {
	s, _ := e["summary"].(map[string]interface{})
	get := func(k string) string { return str(s[k]) }
	var parts []string
	add := func(p ...string) {
		for _, x := range p {
			if x != "" {
				parts = append(parts, x)
			}
		}
	}
	where := joinNames(get("organization_name"), get("app_name"))
	subject := str(e["subject_id"])
	staff := func() string {
		who := get("actor_email")
		if n := get("agent_name"); n != "" {
			who += fmt.Sprintf(" (agent %q)", n)
		}
		return who
	}
	kind := str(e["kind"])
	switch {
	case strings.HasPrefix(kind, "act_as."):
		who := staff()
		if org := get("organization_name"); org != "" {
			who += " in " + org
		}
		add(who)
		if sc := get("scope"); sc != "" {
			add(strings.TrimSuffix(reachLabel(sc), " apps"))
		}
		add(get("access"))
		if exp := get("expires_at"); exp != "" {
			add("until " + clock(exp))
		}
	case kind == "staff_login.approved":
		add(staff())
		if exp := get("expires_at"); exp != "" {
			add("until " + clock(exp))
		}
	case kind == "agents.revoked":
		add(staff(), "revoked "+get("count"), get("scope_label"))
	case kind == "agents.flag":
		state := "off"
		if s["enabled"] == true {
			state = "on"
		}
		add(staff(), "agent staff access "+state)
	case kind == "webhook_failed":
		add(where, strings.TrimSpace(get("webhook_kind")+" "+short(subject)))
		if h := get("host"); h != "" {
			add("to " + h)
		}
	case kind == "connection_created" || kind == "connection_first_connected" || kind == "name_index_gave_up":
		add(joinNames(where, get("connection_name")), get("quickbooks_product_name"))
		if st := get("step"); st != "" {
			add("step " + st)
		}
	case kind == "workflow_run_failed":
		add(where, strings.TrimSpace("run "+short(subject)+" "+get("workflow_key")), get("fault"))
	case kind == "stuck_steps":
		add(get("count")+" steps swept", strings.Join(strs(s["run_ids"]), ", "))
	case kind == "qbxml_parse_failed" || kind == "answer_too_large":
		add(where, "request "+short(subject), strings.Join(strs(s["request_types"]), ","))
		if b := get("bytes"); b != "" {
			add(b + " bytes")
		}
	case kind == "user_registered":
		add(get("email"))
	case kind == "integration_request_submitted":
		add(get("company_name"), get("user_email"), get("starting_point"))
	case kind == "agent_feedback":
		add(get("kind"), get("subject"), get("outcome"))
	case kind == "rate_limit_not_enforced":
		add(get("flag"), "limit "+get("limit"))
	case kind == "stripe_webhook_failed":
		add(get("stripe_event_type"), get("stage"))
	default:
		add(where)
		keys := make([]string, 0, len(s))
		for k := range s {
			if k != "organization_name" && k != "app_name" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(k + "=" + str(s[k]))
		}
	}
	if e["production"] == true {
		add("production")
	}
	return strings.Join(parts, " · ")
}
