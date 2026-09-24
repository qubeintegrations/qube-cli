package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"runtime"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// cli is a client for the /api/cli endpoints, carrying this host's session token (if any).
func (c *ctx) cli() *api.Client {
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	if s, ok := c.cfg.Sessions[c.host]; ok {
		cl.Token = s.Token
	}
	return cl
}

func (c *ctx) login(args []string) {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	name := fs.String("name", "", "a name for this session (default: qube on <hostname>)")
	parseAnywhere(fs, args)
	if *name == "" {
		hn, _ := os.Hostname()
		u, _ := user.Current()
		who := ""
		if u != nil {
			who = u.Username + "@"
		}
		*name = fmt.Sprintf("qube %s on %s%s (%s)", api.Version, who, hn, runtime.GOOS)
	}
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	start, err := cl.StartDevice(*name)
	if err != nil {
		fail(fmt.Errorf("starting login at %s: %w", c.host, err))
	}
	ui.Info("Open %s", start.VerificationURIComplete)
	ui.Info("and confirm the code  %s  (it expires in %d minutes).", start.UserCode, start.ExpiresIn/60)
	if ui.OpenBrowser(start.VerificationURIComplete) {
		ui.Info("(opened in your browser)")
	}
	interval := time.Duration(start.Interval) * time.Second
	if interval < 2*time.Second {
		interval = 5 * time.Second
	}
	grant, err := pollUntilApproved(c.bg, cl, start.DeviceCode, interval, time.Duration(start.ExpiresIn)*time.Second)
	if err != nil {
		fail(err)
	}
	c.cfg.Sessions[c.host] = config.Session{
		Token: grant.Token, ExpiresAt: grant.ExpiresAt, Scope: grant.Scope,
		UserEmail: grant.User.Email, Organization: grant.Organization.Name,
	}
	c.cfg.CurrentHost = c.host
	if err := c.cfg.Save(); err != nil {
		fail(fmt.Errorf("saving session: %w", err))
	}
	reach := "sandbox apps"
	if grant.Scope == "all" {
		reach = "every app"
	}
	if ui.JSON {
		ui.PrintJSON(map[string]interface{}{"host": c.host, "user": grant.User.Email, "organization": grant.Organization.Name, "scope": grant.Scope, "expires_at": grant.ExpiresAt})
		return
	}
	fmt.Printf("Logged in to %s as %s (%s, %s). Session stored in %s\n", c.host, grant.User.Email, grant.Organization.Name, reach, mustPath())
}

// poller is what pollUntilApproved needs from the client (a seam for tests).
type poller interface {
	PollToken(deviceCode string) (*api.TokenGrant, error)
}

// pollUntilApproved asks every `interval` until the code is approved, denied or expired.
// A transient failure (network, 5xx, 429) is retried with a growing pause instead of
// aborting a login that is still perfectly valid; five in a row give up.
func pollUntilApproved(bg interface{ Done() <-chan struct{} }, cl poller, deviceCode string, interval, ttl time.Duration) (*api.TokenGrant, error) {
	deadline := time.Now().Add(ttl)
	pause := interval
	failures := 0
	for time.Now().Before(deadline) {
		select {
		case <-bg.Done():
			return nil, fmt.Errorf("interrupted")
		case <-time.After(pause):
		}
		grant, err := cl.PollToken(deviceCode)
		switch {
		case err == nil:
			return grant, nil
		case err == api.ErrPending:
			pause, failures = interval, 0
		case api.Retryable(err):
			failures++
			if failures >= 5 {
				return nil, fmt.Errorf("login failed after %d attempts: %w", failures, err)
			}
			pause = interval * time.Duration(failures+1)
			if e, ok := err.(*api.Error); ok && e.RetryAfter > 0 {
				pause = time.Duration(e.RetryAfter) * time.Second
			}
			ui.Info("(%v; retrying in %s)", err, pause)
		default:
			return nil, fmt.Errorf("login failed: %w", err)
		}
	}
	return nil, fmt.Errorf("the code expired before it was approved; run `qube login` again")
}

func mustPath() string {
	p, _ := config.Path()
	return p
}

func (c *ctx) logout() {
	cl := c.cli()
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
			out["expires_at"] = s.ExpiresAt
			out["expired"] = s.Expired()
			out["default_app"] = map[string]string{"id": s.DefaultApp, "name": s.DefaultAppName}
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
	fmt.Printf("Scope:        %s\n", s.Scope)
	app := "(none: run `qube use <app>`)"
	switch {
	case s.DefaultApp != "" && s.DefaultAppName != "":
		app = s.DefaultAppName + " (" + s.DefaultApp + ")"
	case s.DefaultApp != "":
		app = s.DefaultApp
	}
	fmt.Printf("Default app:  %s\n", app)
	exp := s.ExpiresAt
	if s.Expired() {
		exp += "  EXPIRED -- run `qube login`"
	}
	fmt.Printf("Session ends: %s\n", exp)
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
	fmt.Printf("%s in %s on %s (scope: %v, session %q expires %v)\n", str(u["email"]), str(o["name"]), c.host, s["scope"], str(s["name"]), s["expires_at"])
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
		rows = append(rows, []string{cur, str(s["id"]), str(s["name"]), str(s["scope"]), status, str(s["last_used_at"])})
	}
	ui.Table(os.Stdout, []string{"", "ID", "NAME", "SCOPE", "STATUS", "LAST USED"}, rows, "No sessions.")
}
