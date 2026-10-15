package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
	"github.com/qubeintegrations/qube-cli/internal/uuid"
)

// What `qube admin` and `qube act-as` share (QuBe Sync staff): the staff session's own client,
// the check that a session holds staff permissions, organizations by name, and the words both
// use for sessions, grants and times.

// staffClient carries the session's own token only, never a grant's: for logging out,
// asking for a grant and `qube admin` (whose routes would carry it anyway).
func (c *ctx) staffClient() *api.Client {
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	cl.Token = c.cfg.Sessions[c.host].Token
	return cl
}

// requireStaff stops a command that needs staff permissions when the session holds none.
func (c *ctx) requireStaff(what string) config.Session {
	s, ok := c.cfg.Sessions[c.host]
	if !ok {
		ui.Fail("not logged in to %s: QuBe Sync staff run `qube login --admin`", c.host)
	}
	if !s.Staff() {
		ui.Fail("this session holds no staff permissions, so it can't %s. QuBe Sync staff log in with `qube login --admin`.", what)
	}
	return s
}

// staffError puts a refusal from a staff route in words: a 404 without the /api/cli
// envelope's code is a server from before the staff API.
func staffError(host string, err error) error {
	var e *api.Error
	if errors.As(err, &e) && e.Status == http.StatusNotFound && e.Code == "" {
		return fmt.Errorf("this QuBe Sync (%s) doesn't have the staff API yet", host)
	}
	return err
}

// resolveOrganization turns an organization id or exact name into its id (and name, when it
// can be looked up). A name is looked up with GET /api/admin/organizations?search=: one exact
// match (any case) is used, and anything else is refused with the candidates listed.
func (c *ctx) resolveOrganization(cl *api.Client, ref string) (id, name string) {
	ref = strings.TrimSpace(ref)
	if uuid.Valid(ref) {
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", "/api/admin/organizations/"+url.PathEscape(ref), nil, nil, &out); err == nil {
			name = str(out.Data["name"])
		}
		return ref, name
	}
	var out struct {
		Data []map[string]interface{} `json:"data"`
	}
	q := url.Values{"search": {ref}, "page_size": {"100"}}
	if err := cl.Do("GET", "/api/admin/organizations", q, nil, &out); err != nil {
		fail(fmt.Errorf("looking up organization %q (or pass its id): %w", ref, staffError(c.host, err)))
	}
	var exact []map[string]interface{}
	for _, o := range out.Data {
		if strings.EqualFold(strings.TrimSpace(str(o["name"])), ref) {
			exact = append(exact, o)
		}
	}
	if len(exact) == 1 {
		return str(exact[0]["id"]), str(exact[0]["name"])
	}
	var why string
	candidates := exact
	switch {
	case len(exact) > 1:
		why = fmt.Sprintf("%d organizations are named %q", len(exact), ref)
	case len(out.Data) == 0:
		ui.Fail("no organization matches %q (`qube admin orgs --search TEXT` lists them)", ref)
	default:
		why = fmt.Sprintf("no organization is named %q exactly", ref)
		candidates = out.Data
	}
	if ui.JSON {
		var list []map[string]interface{}
		for _, o := range candidates {
			list = append(list, map[string]interface{}{"id": o["id"], "name": o["name"], "owners": o["owners"], "created_at": o["created_at"]})
		}
		ui.PrintJSON(map[string]interface{}{"error": "organization_ambiguous", "message": why, "candidates": list})
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s; use the id:\n", why)
	for _, o := range candidates {
		fmt.Fprintf(&b, "  %s  %s%s\n", str(o["id"]), str(o["name"]), ownersNote(o["owners"]))
	}
	ui.Fail("%s", strings.TrimRight(b.String(), "\n"))
	return "", ""
}

func ownersNote(v interface{}) string {
	var emails []string
	list, _ := v.([]interface{})
	for _, o := range list {
		if m, ok := o.(map[string]interface{}); ok && str(m["email"]) != "" {
			emails = append(emails, str(m["email"]))
		}
	}
	if len(emails) == 0 {
		return ""
	}
	return "  (owners: " + strings.Join(emails, ", ") + ")"
}

// ---------------------------------------------------------------- words

// actorOf is who holds a session: "human" or "agent".
func actorOf(s config.Session) string {
	if s.Agent() {
		return "agent"
	}
	return "human"
}

// permissionList is a list for JSON: [] rather than null.
func permissionList(p []string) []string {
	if p == nil {
		return []string{}
	}
	return p
}

func orEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func reachLabel(scope string) string {
	if scope == "all" {
		return "every app"
	}
	return "sandbox apps"
}

func grantAccessLabel(access string) string {
	switch access {
	case "read_only":
		return "read-only"
	case "configure":
		return "configure"
	case "read_write":
		return "read and write"
	}
	return orEmpty(access, "access as approved")
}

func orgLabel(name, id string) string {
	if name != "" {
		return name
	}
	if id != "" {
		return "organization " + id
	}
	return "the organization"
}

// localTime is an RFC 3339 time as local "15:04", with the date when it isn't today.
func localTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	t = t.Local()
	if y, m, d := t.Date(); y == time.Now().Year() && m == time.Now().Month() && d == time.Now().Day() {
		return t.Format("15:04")
	}
	return t.Format("2006-01-02 15:04")
}

// untilLabel is how long until an RFC 3339 time: "in 1h 52m", or "ended".
func untilLabel(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "until " + s
	}
	d := time.Until(t)
	if d <= 0 {
		return "ended"
	}
	return "in " + durationWords(d)
}

// leftLabel is the banner's time left: "1h 52m".
func leftLabel(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return "?"
	}
	d := time.Until(t)
	if d <= 0 {
		return "no time"
	}
	return durationWords(d)
}

func durationWords(d time.Duration) string {
	m := int(d.Round(time.Minute) / time.Minute)
	switch {
	case m < 1:
		return "under a minute"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	}
	return fmt.Sprintf("%dh %dm", m/60, m%60)
}
