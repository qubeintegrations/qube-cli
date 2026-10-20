package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// cli is a client for the /api/cli endpoints (and, given an app, /api/v1 and /api/v2),
// carrying this host's session token (if any). While the session holds an act-as grant, the
// grant's token goes instead, on every route but the staff ones (api.StaffOnly), and a 401 to
// it drops the grant (actAsRefused).
func (c *ctx) cli() *api.Client {
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	if s, ok := c.cfg.Sessions[c.host]; ok {
		cl.Token = s.Token
		if s.ActingAs != nil {
			cl.ActAsToken = s.ActingAs.Token
			cl.OnActAsRefused = c.actAsRefused
		}
	}
	return cl
}

func (c *ctx) login(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	name := fs.String("name", "", "a name for this session (default: qube on <hostname>)")
	readOnly := fs.Bool("read-only", false, "ask for a session that can look but not change anything")
	agent := fs.String("agent", "", "an AI agent will hold this session: its name, as the person approving will see it")
	admin := fs.Bool("admin", false, "ask for staff permissions (QuBe Sync staff only)")
	parseAnywhere(fs, args)
	req := loginRequest(*readOnly, *admin, *agent)
	if *name == "" {
		hn, _ := os.Hostname()
		u, _ := user.Current()
		who := ""
		if u != nil {
			who = u.Username + "@"
		}
		*name = fmt.Sprintf("qube %s on %s%s (%s)", api.Version, who, hn, runtime.GOOS)
	}
	req.ClientName = *name
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	start, err := cl.StartDevice(req)
	if asked, dropped := offeredPermissions(err, req.Permissions); asked != nil {
		// a host that predates a permission (builds.manage, say) refuses the whole login and
		// lists what it grants: ask again for those
		ui.Info("(%s doesn't offer %s yet; asking for the rest)", c.host, strings.Join(dropped, ", "))
		req.Permissions = asked
		start, err = cl.StartDevice(req)
	}
	if err != nil {
		fail(fmt.Errorf("starting login at %s: %w", c.host, err))
	}
	ui.Info("Open %s", start.VerificationURIComplete)
	ui.Info("and confirm the code  %s  (it expires in %d minutes).", start.UserCode, start.ExpiresIn/60)
	if *readOnly {
		ui.Info("It asks for a read-only session; whoever approves it can change that.")
	}
	switch {
	case req.AgentName != "":
		ui.Info("It's for an AI agent (%q), which the person approving confirms; an agent's session can revoke only itself.", req.AgentName)
	case len(req.Permissions) > 0:
		ui.Info("It asks for staff permissions, so you'll be asked who will use the session.")
	}
	if ui.OpenBrowser(start.VerificationURIComplete) {
		ui.Info("(opened in your browser)")
	}
	interval := time.Duration(start.Interval) * time.Second
	if interval < approvalPollMin {
		interval = approvalPollDefault
	}
	grant, err := pollUntilApproved(c.bg, cl, start.DeviceCode, interval, time.Duration(start.ExpiresIn)*time.Second)
	if err != nil {
		fail(err)
	}
	// A new session replaces this host's: an act-as grant the old one held is ended first, so
	// it doesn't outlive the session that asked for it on this machine.
	c.endActAsQuietly()
	s := config.Session{
		Token: grant.Token, ExpiresAt: grant.ExpiresAt, Scope: grant.Scope, Access: grant.Access,
		UserEmail: grant.User.Email, Organization: grant.Organization.Name,
		Actor: grant.Actor, AgentName: grant.AgentName, Permissions: grant.Permissions,
	}
	if s.Actor == "human" {
		s.Actor = ""
	}
	c.cfg.Sessions[c.host] = s
	c.cfg.CurrentHost = c.host
	if err := c.cfg.Save(); err != nil {
		fail(fmt.Errorf("saving session: %w", err))
	}
	reach := "sandbox apps"
	if grant.Scope == "all" {
		reach = "every app"
	}
	if ui.JSON {
		out := map[string]interface{}{"host": c.host, "user": grant.User.Email, "organization": grant.Organization.Name, "scope": grant.Scope, "access": accessOf(s), "expires_at": grant.ExpiresAt}
		if req.AgentName != "" || len(req.Permissions) > 0 || len(grant.Permissions) > 0 {
			out["actor"] = actorOf(s)
			out["agent_name"] = s.AgentName
			out["permissions"] = permissionList(s.Permissions)
		}
		ui.PrintJSON(out)
		return
	}
	if s.Agent() {
		fmt.Printf("Logged in to %s as an agent session for %s (%q; %s, %s, %s). Session stored in %s\n", c.host, grant.User.Email, s.AgentName, grant.Organization.Name, reach, s.AccessLabel(), mustPath())
	} else {
		fmt.Printf("Logged in to %s as %s (%s, %s, %s). Session stored in %s\n", c.host, grant.User.Email, grant.Organization.Name, reach, s.AccessLabel(), mustPath())
	}
	if len(req.Permissions) == 0 && len(s.Permissions) == 0 {
		return
	}
	if len(s.Permissions) == 0 {
		ui.Warn("the session holds no staff permissions: the person approving ticked none, or %s doesn't offer staff sessions yet", c.host)
		return
	}
	fmt.Printf("Staff: %s\n", strings.Join(s.Permissions, " "))
	fmt.Printf("This session ends at %s (%s).\n", localTime(s.ExpiresAt), untilLabel(s.ExpiresAt))
}

// loginRequest is what `qube login` asks for: --read-only, --admin (every staff permission;
// for an agent, the most an agent's session can hold) and --agent NAME.
func loginRequest(readOnly, admin bool, agent string) api.DeviceRequest {
	req := api.DeviceRequest{Access: "read_write"}
	if readOnly {
		req.Access = "read_only"
	}
	agent = strings.TrimSpace(agent)
	if len([]rune(agent)) > 80 {
		ui.Usage("--agent NAME is at most 80 characters")
	}
	req.AgentName = agent
	if admin {
		req.Permissions = api.StaffPermissions
		if agent != "" {
			req.Permissions = api.AgentPermissions
		}
	}
	return req
}

// offeredPermissions reads a refusal of a login's permissions: the asked-for permissions the
// host grants, and the ones it doesn't. The host's list is the error's `permissions` when it
// sends one; a host that predates that field names them only in its message ("permissions are
// some of: ..."), which is read instead. Nil when err isn't that refusal, or when nothing would
// be left to ask for or nothing is dropped.
func offeredPermissions(err error, asked []string) (offered, dropped []string) {
	var e *api.Error
	if !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Code != "invalid_request" {
		return nil, nil
	}
	granted := e.Permissions
	if len(granted) == 0 {
		const lead = "permissions are some of: "
		i := strings.Index(e.Message, lead)
		if i < 0 {
			return nil, nil
		}
		granted = strings.Split(e.Message[i+len(lead):], ",")
	}
	listed := map[string]bool{}
	for _, p := range granted {
		listed[strings.TrimSpace(p)] = true
	}
	for _, p := range asked {
		if listed[p] {
			offered = append(offered, p)
		} else {
			dropped = append(dropped, p)
		}
	}
	if len(offered) == 0 || len(dropped) == 0 {
		return nil, nil
	}
	return offered, dropped
}

// poller is what pollUntilApproved needs from the client (a seam for tests).
type poller interface {
	PollToken(deviceCode string) (*api.TokenGrant, error)
}

// pollUntilApproved asks every `interval` until the code is approved, denied or expired.
// A transient failure (network, 5xx, 429) is retried with a growing pause instead of
// aborting a login that is still perfectly valid; five in a row give up.
func pollUntilApproved(bg interface{ Done() <-chan struct{} }, cl poller, deviceCode string, interval, ttl time.Duration) (*api.TokenGrant, error) {
	grant, err := pollApproval(bg, cl, deviceCode, interval, time.Now().Add(ttl))
	switch {
	case err == nil:
		return grant, nil
	case errors.Is(err, errApprovalExpired):
		return nil, fmt.Errorf("the code expired before it was approved; run `qube login` again")
	case errors.Is(err, context.Canceled):
		return nil, fmt.Errorf("interrupted")
	}
	return nil, fmt.Errorf("login failed: %w", err)
}

// How often a code waiting for approval is asked about (a login's, or an act-as grant's): the
// server's interval, or the default when it names one under the minimum (variables so tests
// can shorten them).
var (
	approvalPollMin     = 2 * time.Second
	approvalPollDefault = 5 * time.Second
)

// errApprovalExpired is pollApproval's answer when the deadline passes with the code still
// waiting.
var errApprovalExpired = errors.New("the code expired before it was approved")

// pollApproval asks every `interval` until the code is approved (the grant), denied or
// otherwise refused (the server's *api.Error), the deadline passes (errApprovalExpired) or
// bg is done (context.Canceled). A transient failure (network, 5xx, 429) is retried with a
// growing pause; five in a row give up. `qube login` and `qube act-as` both wait this way.
func pollApproval(bg interface{ Done() <-chan struct{} }, cl poller, deviceCode string, interval time.Duration, deadline time.Time) (*api.TokenGrant, error) {
	pause := interval
	failures := 0
	for time.Now().Before(deadline) {
		select {
		case <-bg.Done():
			return nil, context.Canceled
		case <-time.After(pause):
		}
		grant, err := cl.PollToken(deviceCode)
		switch {
		case err == nil:
			return grant, nil
		case err == api.ErrPending:
			pause, failures = interval, 0
		case errors.Is(err, context.Canceled):
			return nil, context.Canceled
		case api.Retryable(err):
			failures++
			if failures >= 5 {
				return nil, fmt.Errorf("gave up after %d attempts: %w", failures, err)
			}
			pause = interval * time.Duration(failures+1)
			if e, ok := err.(*api.Error); ok && e.RetryAfter > 0 {
				pause = time.Duration(e.RetryAfter) * time.Second
			}
			ui.Info("(%v; retrying in %s)", err, pause)
		default:
			return nil, err
		}
	}
	return nil, errApprovalExpired
}

func mustPath() string {
	p, _ := config.Path()
	return p
}

func (c *ctx) logout() {
	c.endActAsQuietly()
	cl := c.staffClient()
	if cl.Token != "" {
		if err := cl.Do("DELETE", "/api/cli/session", nil, nil, nil); err != nil {
			ui.Info("(the server could not be told: %v -- the session is dropped locally; revoke it under CLI sessions in the dashboard)", err)
		}
	}
	delete(c.cfg.Sessions, c.host)
	if c.cfg.CurrentHost == c.host {
		c.cfg.CurrentHost = ""
	}
	if err := c.cfg.Save(); err != nil {
		fail(fmt.Errorf("saving config: %w", err))
	}
	ui.Info("Logged out of %s.", c.host)
}

// status is what `qube` knows without asking the server.
func (c *ctx) status() {
	s, ok := c.cfg.Sessions[c.host]
	if ui.JSON {
		out := map[string]interface{}{"host": c.host, "logged_in": ok, "config": mustPath()}
		if ok {
			out["user"] = s.UserEmail
			out["organization"] = s.Organization
			out["scope"] = s.Scope
			out["access"] = accessOf(s)
			out["expires_at"] = s.ExpiresAt
			out["expired"] = s.Expired()
			out["default_app"] = map[string]string{"id": s.DefaultApp, "name": s.DefaultAppName}
			if cn, ok := s.DefaultConnections[s.DefaultApp]; ok {
				out["default_connection"] = cn
			}
			out["actor"] = actorOf(s)
			if s.AgentName != "" {
				out["agent_name"] = s.AgentName
			}
			out["permissions"] = permissionList(s.Permissions)
			if s.ActingAs != nil {
				out["acting_as"] = actAsJSON(s.ActingAs)
			}
			if s.PendingActAs != nil {
				out["pending_act_as"] = pendingJSON(s.PendingActAs)
			}
		}
		out["hosts"] = hostList(c.cfg)
		ui.PrintJSON(out)
		return
	}
	if !ok {
		fmt.Printf("Not logged in to %s (run `qube login`). Config: %s\n", c.host, mustPath())
		if len(c.cfg.Sessions) > 0 {
			fmt.Printf("Sessions exist for: %v -- pick one with --host or `qube use --host H`.\n", hostList(c.cfg))
		}
		return
	}
	fmt.Printf("Host:         %s\n", c.host)
	fmt.Printf("User:         %s (%s)\n", s.UserEmail, s.Organization)
	if s.Agent() {
		fmt.Printf("Actor:        agent %q (the server holds it to an agent's limits)\n", s.AgentName)
	}
	fmt.Printf("Scope:        %s\n", s.Scope)
	fmt.Printf("Access:       %s\n", s.AccessLabel())
	if s.Staff() {
		fmt.Printf("Staff:        %s\n", strings.Join(s.Permissions, " "))
	}
	app := "(none: run `qube use <app>`)"
	switch {
	case s.DefaultApp != "" && s.DefaultAppName != "":
		app = s.DefaultAppName + " (" + s.DefaultApp + ")"
	case s.DefaultApp != "":
		app = s.DefaultApp
	}
	fmt.Printf("Default app:  %s\n", app)
	if cn, ok := s.DefaultConnections[s.DefaultApp]; ok && s.DefaultApp != "" {
		fmt.Printf("Connection:   %s\n", connectionLabel(cn))
	}
	exp := s.ExpiresAt
	if s.Expired() {
		exp += "  EXPIRED -- run `qube login`"
	}
	fmt.Printf("Session ends: %s\n", exp)
	if a := s.ActingAs; a != nil {
		fmt.Printf("Acting as:    %s (%s, %s) until %s, %s\n", a.Organization, reachLabel(a.Scope), grantAccessLabel(a.Access), localTime(a.ExpiresAt), untilLabel(a.ExpiresAt))
		if a.DefaultAppName != "" {
			fmt.Printf("  Its app:    %s (%s)\n", a.DefaultAppName, a.DefaultApp)
		}
		fmt.Printf("  Reason:     %s\n", a.Reason)
	}
	if p := s.PendingActAs; p != nil {
		fmt.Printf("Waiting:      a grant for %s, until someone approves code %s at %s (expires %s)\n", orgLabel(p.Organization, p.OrganizationID), p.UserCode, p.VerificationURIComplete, localTime(p.ExpiresAt))
	}
	if len(c.cfg.Sessions) > 1 {
		fmt.Printf("Other hosts:  %v\n", hostList(c.cfg))
	}
	fmt.Printf("Config:       %s\n", mustPath())
}

func hostList(cfg *config.File) []string {
	var hosts []string
	for h := range cfg.Sessions {
		hosts = append(hosts, h)
	}
	return hosts
}

func (c *ctx) whoami() {
	var out struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := c.cli().Do("GET", "/api/cli/me", nil, nil, &out); err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	u, _ := out.Data["user"].(map[string]interface{})
	o, _ := out.Data["organization"].(map[string]interface{})
	s, _ := out.Data["session"].(map[string]interface{})
	who := str(u["email"])
	if str(s["actor"]) == "agent" {
		who = fmt.Sprintf("agent %q for %s", str(s["agent_name"]), who)
	}
	if str(s["kind"]) == "act_as" {
		fmt.Printf("%s, acting as %s on %s (act-as grant: %s, %s, expires %v; reason %q)\n", who, str(o["name"]), c.host, reachLabel(str(s["scope"])), grantAccessLabel(str(s["access"])), s["expires_at"], str(s["reason"]))
		return
	}
	access := "read and write"
	if str(s["access"]) == "read_only" {
		access = "read-only"
	}
	fmt.Printf("%s in %s on %s (scope: %v, %s, session %q expires %v)\n", who, str(o["name"]), c.host, s["scope"], access, str(s["name"]), s["expires_at"])
	if perms := strs(s["permissions"]); len(perms) > 0 {
		fmt.Printf("Staff: %s\n", strings.Join(perms, " "))
		if eff := strs(s["effective_permissions"]); len(eff) < len(perms) {
			fmt.Printf("Holding now: %s (less than approved: a role change, or staff access switched off)\n", orEmpty(strings.Join(eff, " "), "none"))
		}
	}
}

func (c *ctx) sessions(args []string) {
	cl := c.cli()
	if len(args) >= 1 && args[0] == "revoke" {
		if len(args) < 2 {
			ui.Usage("usage: qube sessions revoke <id>")
		}
		if err := cl.Do("DELETE", "/api/cli/sessions/"+url.PathEscape(args[1]), nil, nil, nil); err != nil {
			fail(err)
		}
		ui.Info("Revoked.")
		return
	}
	var out struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := cl.Do("GET", "/api/cli/sessions", nil, nil, &out); err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	// The actor, kind and staff columns appear only when some session has them, so an
	// ordinary account's list stays as it was.
	var agents, grants, staff bool
	for _, s := range out.Data {
		agents = agents || str(s["actor"]) == "agent"
		grants = grants || str(s["kind"]) == "act_as"
		staff = staff || len(strs(s["permissions"])) > 0
	}
	header := []string{"", "ID", "NAME"}
	if grants {
		header = append(header, "KIND")
	}
	if agents {
		header = append(header, "ACTOR")
	}
	header = append(header, "SCOPE", "ACCESS")
	if staff {
		header = append(header, "STAFF")
	}
	header = append(header, "EXPIRES", "STATUS", "LAST USED")
	var rows [][]string
	for _, s := range out.Data {
		status := "active"
		if s["revoked_at"] != nil {
			status = "revoked"
		} else if s["active"] != true {
			status = "expired"
		}
		cur := ""
		if s["current"] == true {
			cur = "*"
		}
		access := "read-write"
		switch str(s["access"]) {
		case "read_only":
			access = "read-only"
		case "configure":
			access = "configure"
		}
		row := []string{cur, str(s["id"]), str(s["name"])}
		if grants {
			row = append(row, strings.Replace(orEmpty(str(s["kind"]), "login"), "_", "-", 1))
		}
		if agents {
			actor := "person"
			if str(s["actor"]) == "agent" {
				actor = fmt.Sprintf("agent %q", str(s["agent_name"]))
			}
			row = append(row, actor)
		}
		row = append(row, str(s["scope"]), access)
		if staff {
			row = append(row, orEmpty(strings.Join(strs(s["permissions"]), ","), "-"))
		}
		row = append(row, str(s["expires_at"]), status, str(s["last_used_at"]))
		rows = append(rows, row)
	}
	ui.Table(os.Stdout, header, rows, "No sessions.")
}
