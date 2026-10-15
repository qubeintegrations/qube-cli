package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
	"github.com/qubeintegrations/qube-cli/internal/uuid"
)

// `qube act-as ORG --reason R` (QuBe Sync staff): a short-lived grant to act in a customer's
// organization, asked for with the staff session's token (POST /api/cli/act-as). The grant is
// an overlay on this host's session in the config file (config.ActAs): while it is held,
// every command but `qube admin` and `qube act-as` uses its token instead of the session's,
// and its apps instead of the session's (api.StaffOnly picks the token per route). Each such
// command prints a banner on stderr naming the organization, so even a --json caller sees
// whose organization it is in.
//
// A grant that ends -- its time is up, or the server refuses its token (401) -- is dropped,
// and the command that found out stops with a message rather than run in the person's own
// organization. A person's sandbox grant is made at once (201); a production grant, and any
// grant an agent asks for, waits for approval in the browser (202), and is kept in the config
// as a pending overlay until it is collected.

const actAsUsage = `usage: qube act-as <organization id or name> --reason R [--for 60m] [--production]
                    [--access read_only|configure|read_write] [--write-budget N] [--wait|--no-wait]
       qube act-as --status        the grant this session holds (or waits for)
       qube act-as --resume        wait for a grant asked for with --no-wait
       qube act-as --end           revoke the grant and go back to your own organization`

const actAsHelp = `Acts in a customer's organization (QuBe Sync staff only). Every command but qube admin then
uses the grant: its apps, its access, recorded with your reason. A person's sandbox grant starts at
once (up to 8 h, read and write by default). A production grant (--production: every app) is
approved in the browser with a fresh login (up to 2 h, configure by default). A grant an agent's
session asks for is approved in the browser by its person (sandbox up to 2 h, production up to 1
h, read-only by default; a production grant with more gets a write budget, 25 by default).
--yes only answers qube's own questions: the server holds every grant to its access and write
budget, whatever --yes says.
`

// acting is the grant this session holds, or nil.
func (c *ctx) acting() *config.ActAs {
	return c.cfg.Sessions[c.host].ActingAs
}

// grantClient carries one grant's token as the session token (ending the grant with it).
func (c *ctx) grantClient(token string) *api.Client {
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	cl.Token = token
	return cl
}

// ---------------------------------------------------------------- the overlay's lifecycle

// dropEndedActAs drops a grant whose time is up, and an approval code that expired, when the
// config is read. What ended is kept in c.actAsEnded: a command that would have used the
// grant stops with it instead of running in the person's own organization.
func (c *ctx) dropEndedActAs() {
	s, ok := c.cfg.Sessions[c.host]
	if !ok {
		return
	}
	changed := false
	if a := s.ActingAs; a != nil && a.Expired() {
		c.actAsEnded = fmt.Sprintf("the act-as grant for %s ended at %s", a.Organization, localTime(a.ExpiresAt))
		s.ActingAs = nil
		changed = true
	}
	if p := s.PendingActAs; p != nil && p.Expired() {
		c.actAsEnded = fmt.Sprintf("the grant for %s you asked for (code %s) expired before anyone approved it", orgLabel(p.Organization, p.OrganizationID), p.UserCode)
		s.PendingActAs = nil
		changed = true
	}
	if changed {
		c.cfg.Sessions[c.host] = s
		if err := c.cfg.Save(); err != nil {
			ui.Warn("saving config: %v", err)
		}
	}
}

// enterActAs runs before every command that would use a grant. A grant that ended stops the
// command; a pending one is collected if it was approved, and stops the command otherwise;
// a live one prints the banner.
func (c *ctx) enterActAs() {
	if c.actAsEnded != "" {
		ui.Fail("%s, so qube stopped instead of running this in your own organization. It's dropped: run the command again to use your own session, or `qube act-as` for a new grant.", c.actAsEnded)
	}
	s, ok := c.cfg.Sessions[c.host]
	if !ok {
		return
	}
	if s.ActingAs == nil && s.PendingActAs != nil {
		grant := c.collectPending(false)
		fmt.Fprintf(os.Stderr, "qube: %s\n", approvedLine(grant))
	}
	if a := c.acting(); a != nil {
		fmt.Fprintln(os.Stderr, bannerLine(a))
	}
}

// bannerLine is the line every command prints on stderr while acting.
func bannerLine(a *config.ActAs) string {
	return fmt.Sprintf("(acting as %s · %s · %s · %s left)", a.Organization, reachLabel(a.Scope), grantAccessLabel(a.Access), leftLabel(a.ExpiresAt))
}

// actAsRefused is the client's OnActAsRefused: the server refused the grant's token, so the
// grant has ended. It is dropped, and the error says so; nothing falls back to the session's
// own token.
func (c *ctx) actAsRefused(e *api.Error) {
	org := "the organization"
	c.actAsOnce.Do(func() {
		s := c.cfg.Sessions[c.host]
		if s.ActingAs == nil {
			return
		}
		c.refusedOrg = s.ActingAs.Organization
		s.ActingAs = nil
		c.cfg.Sessions[c.host] = s
		if err := c.cfg.Save(); err != nil {
			ui.Warn("saving config: %v", err)
		}
	})
	if c.refusedOrg != "" {
		org = c.refusedOrg
	}
	e.Message = fmt.Sprintf("the act-as grant for %s has ended (it expired, was revoked, or its staff session ended), so qube stopped instead of using your own session. "+
		"It's dropped: the next command uses your own session, and `qube act-as` asks for a new grant", org)
}

// collectPending redeems the grant waiting for approval: once (wait false), or until it is
// approved, denied or expired. An approved grant becomes the overlay. Anything else stops the
// command, dropping the pending grant when it can no longer be approved.
func (c *ctx) collectPending(wait bool) *config.ActAs {
	s := c.cfg.Sessions[c.host]
	p := s.PendingActAs
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	org := orgLabel(p.Organization, p.OrganizationID)
	var grant *api.TokenGrant
	var err error
	if wait {
		deadline, perr := time.Parse(time.RFC3339, p.ExpiresAt)
		if perr != nil {
			deadline = time.Now().Add(10 * time.Minute)
		}
		interval := time.Duration(p.Interval) * time.Second
		if interval < approvalPollMin {
			interval = approvalPollDefault
		}
		grant, err = pollApproval(c.bg, cl, p.DeviceCode, interval, deadline)
	} else {
		grant, err = cl.PollToken(p.DeviceCode)
	}
	var e *api.Error
	switch {
	case err == nil:
	case errors.Is(err, api.ErrPending):
		ui.Fail("the grant for %s is waiting for approval: open %s (code %s, until %s). Then run this again, or `qube act-as --resume` to wait for it; `qube act-as --end` drops it.",
			org, p.VerificationURIComplete, p.UserCode, localTime(p.ExpiresAt))
	case errors.Is(err, context.Canceled):
		fmt.Fprintf(os.Stderr, "qube: stopped waiting; the grant for %s is still waiting for approval (`qube act-as --resume` picks it up again)\n", org)
		os.Exit(ui.ExitInterrupted)
	case errors.Is(err, errApprovalExpired):
		c.dropPending()
		ui.Fail("the grant for %s expired before anyone approved it; run `qube act-as` again", org)
	case errors.As(err, &e) && e.Code == "access_denied":
		c.dropPending()
		ui.Fail("the grant for %s was denied in the browser", org)
	case errors.As(err, &e) && e.Code == "expired_token":
		c.dropPending()
		ui.Fail("the grant for %s expired before anyone approved it; run `qube act-as` again", org)
	case errors.As(err, &e) && e.Code == "invalid_grant":
		c.dropPending()
		ui.Fail("the grant for %s can't be collected (unknown, or already collected elsewhere); run `qube act-as` again", org)
	default:
		fail(fmt.Errorf("checking the grant for %s: %w", org, err))
	}
	a := &config.ActAs{
		Token:          grant.Token,
		GrantID:        grant.ID,
		OrganizationID: grant.Organization.ID,
		Organization:   grant.Organization.Name,
		Scope:          grant.Scope,
		Access:         grant.Access,
		Reason:         grant.Reason,
		WriteBudget:    grant.WriteBudget,
		ExpiresAt:      grant.ExpiresAt,
	}
	if a.Organization == "" {
		a.Organization = org
	}
	if grant.ApprovedBy != nil {
		a.ApprovedBy = grant.ApprovedBy.Email
	}
	// The token's answer lists the grant's apps, as a 201 does.
	a.Apps = configApps(grant.Apps)
	c.holdGrant(a)
	return a
}

// holdGrant makes a the overlay, with its only app as its default, and drops any pending
// grant.
func (c *ctx) holdGrant(a *config.ActAs) {
	if len(a.Apps) == 1 {
		a.DefaultApp, a.DefaultAppName = a.Apps[0].ID, a.Apps[0].Name
	}
	s := c.cfg.Sessions[c.host]
	s.ActingAs, s.PendingActAs = a, nil
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		fail(fmt.Errorf("saving the grant: %w", err))
	}
}

func (c *ctx) dropPending() {
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = nil
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		ui.Warn("saving config: %v", err)
	}
}

// endActAsQuietly ends the grant this session holds, if any, and drops a pending one: before
// `qube login` replaces the session and when `qube logout` revokes it.
func (c *ctx) endActAsQuietly() {
	s, ok := c.cfg.Sessions[c.host]
	if !ok || (s.ActingAs == nil && s.PendingActAs == nil) {
		return
	}
	if a := s.ActingAs; a != nil {
		if err := c.revokeGrant(a); err != nil {
			ui.Warn("%v", err)
		} else {
			ui.Info("Ended the act-as grant for %s.", a.Organization)
		}
	}
	s.ActingAs, s.PendingActAs = nil, nil
	c.cfg.Sessions[c.host] = s
}

// revokeGrant ends a grant on the server (DELETE /api/cli/session with its own token). A 401
// means it had already ended.
func (c *ctx) revokeGrant(a *config.ActAs) error {
	err := c.grantClient(a.Token).Do("DELETE", "/api/cli/session", nil, nil, nil)
	var e *api.Error
	if err == nil || (errors.As(err, &e) && e.Status == http.StatusUnauthorized) {
		return nil
	}
	how := "`qube admin grants` lists it, and `qube admin grants revoke <id>` ends it"
	if a.GrantID != "" {
		how = "`qube admin grants revoke " + a.GrantID + "` ends it"
	}
	return fmt.Errorf("the server could not be told to end the grant for %s (%v): it stays live until %s unless revoked (%s); it's dropped here", a.Organization, err, localTime(a.ExpiresAt), how)
}

// ---------------------------------------------------------------- qube act-as

func (c *ctx) actAs(args []string) {
	fs := flag.NewFlagSet("act-as", flag.ExitOnError)
	reason := fs.String("reason", "", "why (10 to 500 characters): recorded with every request the grant makes (required)")
	forDur := fs.String("for", "", "how long: 30m, 2h, or minutes (default 60m; at most 8h for a sandbox grant, 2h for production)")
	production := fs.Bool("production", false, "reach every app, production ones included (approved in the browser)")
	access := fs.String("access", "", "read_only, configure or read_write (default: read_write for a sandbox grant, configure for production, read_only for an agent)")
	budget := fs.Int("write-budget", -1, "the most changes in production apps the grant may make (0 to 200)")
	wait := fs.Bool("wait", false, "wait here for a grant that needs approval (the default)")
	noWait := fs.Bool("no-wait", false, "don't wait for approval: print the link and exit; `qube act-as --resume` (or the next command) collects it")
	resume := fs.Bool("resume", false, "wait for the grant asked for with --no-wait")
	end := fs.Bool("end", false, "revoke the grant and go back to your own organization")
	status := fs.Bool("status", false, "the grant this session holds, or waits for (offline)")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "%s\n\n%s\nflags:\n", actAsUsage, actAsHelp)
		fs.PrintDefaults()
	}
	parseAnywhere(fs, args)
	if *wait && *noWait {
		ui.Usage("--wait and --no-wait can't both be given")
	}
	asking := *reason != "" || *forDur != "" || *production || *access != "" || *budget >= 0 || *noWait
	switch {
	case fs.NArg() > 1:
		ui.Usage("one organization at a time (quote a name with spaces)\n%s", actAsUsage)
	case (*end || *status || *resume) && (fs.NArg() > 0 || asking):
		ui.Usage("--end, --status and --resume take no organization or other flags\n%s", actAsUsage)
	case fs.NArg() == 0 && asking:
		ui.Usage("which organization? name it (its id, or its exact name)\n%s", actAsUsage)
	}
	switch {
	case *end:
		c.endActAs()
	case *status || (fs.NArg() == 0 && !*resume && !*wait):
		c.actAsStatus()
	case *resume || (*wait && fs.NArg() == 0):
		c.resumeActAs()
	case fs.NArg() == 1:
		minutes := 0
		if *forDur != "" {
			minutes = parseMinutes(*forDur)
		}
		var wb *int
		if *budget >= 0 {
			wb = budget
		}
		c.startActAs(fs.Arg(0), actAsOptions{
			reason: strings.TrimSpace(*reason), minutes: minutes, production: *production,
			access: *access, writeBudget: wb, noWait: *noWait,
		})
	default:
		ui.Usage("%s", actAsUsage)
	}
}

type actAsOptions struct {
	reason      string
	minutes     int
	production  bool
	access      string
	writeBudget *int
	noWait      bool
}

// parseMinutes reads --for: a Go duration (30m, 2h, 1h30m) or a number of minutes, rounded up
// to whole minutes.
func parseMinutes(s string) int {
	s = strings.TrimSpace(s)
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		ui.Usage("--for is a duration like 30m or 2h, or a number of minutes (got %q)", s)
	}
	return int((d + time.Minute - time.Second) / time.Minute)
}

func (c *ctx) startActAs(ref string, o actAsOptions) {
	s := c.requireStaff("act as an organization")
	if a := s.ActingAs; a != nil {
		ui.Fail("this session already acts as %s, until %s: `qube act-as --end` first", a.Organization, localTime(a.ExpiresAt))
	}
	if o.reason == "" {
		ui.Usage("--reason is required: why you're acting (10 to 500 characters), recorded with every request the grant makes\n%s", actAsUsage)
	}
	switch o.access {
	case "", "read_only", "configure", "read_write":
	default:
		ui.Usage("--access is read_only, configure or read_write (got %q)", o.access)
	}
	if s.PendingActAs != nil {
		ui.Info("(dropping the grant for %s that was waiting for approval)", orgLabel(s.PendingActAs.Organization, s.PendingActAs.OrganizationID))
		s.PendingActAs = nil
		c.cfg.Sessions[c.host] = s
	}
	cl := c.staffClient()
	// An id goes as is: the answer names the organization.
	orgID, orgName := strings.TrimSpace(ref), ""
	if !uuid.Valid(orgID) {
		orgID, orgName = c.resolveOrganization(cl, ref)
	}
	req := api.ActAsRequest{
		OrganizationID: orgID, Reason: o.reason, Minutes: o.minutes, Scope: "sandbox",
		Access: o.access, WriteBudget: o.writeBudget,
	}
	if o.production {
		req.Scope = "all"
	}
	ans, err := cl.RequestActAs(req)
	if err != nil {
		fail(staffError(c.host, err))
	}
	switch ans.Status {
	case "active":
		c.startedGrant(ans)
	case "pending_approval":
		c.pendingGrant(ans, orgID, orgName, o.noWait)
	default:
		ui.Fail("unexpected answer from %s (status %q): is qube up to date?", c.host, ans.Status)
	}
}

// startedGrant holds a grant made at once (201).
func (c *ctx) startedGrant(ans *api.ActAsAnswer) {
	if ans.Grant == nil || ans.Token == "" {
		ui.Fail("the grant's answer had no token: is qube up to date?")
	}
	g := ans.Grant
	a := &config.ActAs{
		Token: ans.Token, GrantID: g.ID, OrganizationID: g.Organization.ID, Organization: g.Organization.Name,
		Scope: g.Scope, Access: g.Access, Reason: g.Reason, WriteBudget: g.WriteBudget, ExpiresAt: g.ExpiresAt,
		Apps: configApps(ans.Apps),
	}
	if g.ApprovedBy != nil {
		a.ApprovedBy = g.ApprovedBy.Email
	}
	c.holdGrant(a)
	c.printActing(a, "")
}

// pendingGrant keeps a grant waiting for approval (202) and, unless --no-wait, waits for it.
func (c *ctx) pendingGrant(ans *api.ActAsAnswer, orgID, orgName string, noWait bool) {
	ap := ans.Approval
	if ap == nil || ap.DeviceCode == "" {
		ui.Fail("the grant's answer had no approval code: is qube up to date?")
	}
	ttl := ap.ExpiresIn
	if ttl <= 0 {
		ttl = 600
	}
	p := &config.PendingActAs{
		DeviceCode: ap.DeviceCode, UserCode: ap.UserCode, VerificationURIComplete: ap.VerificationURIComplete,
		ExpiresAt: time.Now().Add(time.Duration(ttl) * time.Second).UTC().Format(time.RFC3339),
		Interval:  ap.Interval, OrganizationID: orgID, Organization: orgName,
	}
	if r := ans.Request; r != nil {
		p.RequestID, p.Scope, p.Access, p.Minutes, p.Reason = r.ID, r.Scope, r.Access, r.Minutes, r.Reason
		if r.Organization != nil && r.Organization.Name != "" {
			p.Organization = r.Organization.Name
		}
	}
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = p
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		fail(fmt.Errorf("saving the grant waiting for approval: %w", err))
	}
	why := "A production grant is approved in the browser, with a login under 10 minutes old."
	open := fmt.Sprintf("Open %s and approve code %s (it expires in %d minutes; you may be asked to log in again).", p.VerificationURIComplete, p.UserCode, ttl/60)
	if s.Agent() {
		why = "This is an agent's session, so its person approves the grant."
		open = fmt.Sprintf("Ask %s to open %s and approve code %s (it expires in %d minutes).", orEmpty(s.UserEmail, "the session's person"), p.VerificationURIComplete, p.UserCode, ttl/60)
	}
	summary := fmt.Sprintf("Asked to act as %s (%s, %s, %d min).", orgLabel(p.Organization, p.OrganizationID), reachLabel(p.Scope), grantAccessLabel(p.Access), p.Minutes)
	if noWait {
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"status": "pending_approval", "pending_act_as": pendingJSON(p)})
			return
		}
		fmt.Println(summary)
		fmt.Println(why)
		fmt.Println(open)
		fmt.Println("Then run `qube act-as --resume` (or any command, once it's approved).")
		return
	}
	// The link has to reach whoever approves, so it is printed under --json too (stderr).
	fmt.Fprintln(os.Stderr, summary)
	fmt.Fprintln(os.Stderr, why)
	fmt.Fprintln(os.Stderr, open)
	if !s.Agent() && ui.OpenBrowser(p.VerificationURIComplete) {
		fmt.Fprintln(os.Stderr, "(opened in your browser)")
	}
	fmt.Fprintln(os.Stderr, "Waiting for approval... (Ctrl-C stops waiting; `qube act-as --resume` picks it up again)")
	a := c.collectPending(true)
	c.printActing(a, approvedPrefix(a))
}

func (c *ctx) resumeActAs() {
	s, ok := c.cfg.Sessions[c.host]
	if !ok {
		ui.Fail("not logged in to %s", c.host)
	}
	if s.PendingActAs == nil {
		if s.ActingAs != nil {
			c.printActing(s.ActingAs, "Already approved: ")
			return
		}
		if c.actAsEnded != "" {
			ui.Fail("%s", c.actAsEnded)
		}
		ui.Fail("no grant is waiting for approval (`qube act-as ORG --reason R` asks for one)")
	}
	p := s.PendingActAs
	fmt.Fprintf(os.Stderr, "Waiting for approval of the grant for %s: %s (code %s, until %s). Ctrl-C stops waiting.\n", orgLabel(p.Organization, p.OrganizationID), p.VerificationURIComplete, p.UserCode, localTime(p.ExpiresAt))
	a := c.collectPending(true)
	c.printActing(a, approvedPrefix(a))
}

func (c *ctx) endActAs() {
	s, ok := c.cfg.Sessions[c.host]
	if !ok || (s.ActingAs == nil && s.PendingActAs == nil) {
		if ui.JSON {
			ui.PrintJSON(map[string]interface{}{"ended": nil})
			return
		}
		fmt.Println("Not acting as any organization.")
		return
	}
	a, p := s.ActingAs, s.PendingActAs
	var revokeErr error
	if a != nil {
		revokeErr = c.revokeGrant(a)
	}
	// A grant still waiting for approval is withdrawn on the server, so its code can't be
	// approved later; without a request id (an older server) it just expires unused.
	withdrawn := false
	if p != nil && p.RequestID != "" {
		err := c.staffClient().Do("DELETE", "/api/admin/grants/"+url.PathEscape(p.RequestID), nil, nil, nil)
		var e *api.Error
		switch {
		case err == nil:
			withdrawn = true
		case errors.As(err, &e) && e.Status == http.StatusNotFound:
			// Already answered or expired: nothing left to withdraw.
		default:
			ui.Warn("couldn't withdraw the grant waiting for approval (its code expires unused): %v", staffError(c.host, err))
		}
	}
	s.ActingAs, s.PendingActAs = nil, nil
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		fail(fmt.Errorf("saving config: %w", err))
	}
	if revokeErr != nil {
		ui.Warn("%v", revokeErr)
	}
	if ui.JSON {
		out := map[string]interface{}{}
		if a != nil {
			out["ended"] = actAsJSON(a)
			out["revoked"] = revokeErr == nil
		}
		if p != nil {
			out["dropped_pending"] = pendingJSON(p)
			out["withdrawn"] = withdrawn
		}
		ui.PrintJSON(out)
		return
	}
	if p != nil && withdrawn {
		fmt.Printf("Withdrew the grant for %s that was waiting for approval: its code can't be approved now.\n", orgLabel(p.Organization, p.OrganizationID))
	} else if p != nil {
		fmt.Printf("Dropped the grant for %s that was waiting for approval (its code expires unused).\n", orgLabel(p.Organization, p.OrganizationID))
	}
	if a != nil {
		fmt.Printf("Stopped acting as %s. Commands use your own session again (%s).\n", a.Organization, s.Organization)
	}
}

func (c *ctx) actAsStatus() {
	s, ok := c.cfg.Sessions[c.host]
	if ui.JSON {
		out := map[string]interface{}{"host": c.host, "acting_as": nil, "pending_act_as": nil}
		if ok && s.ActingAs != nil {
			out["acting_as"] = actAsJSON(s.ActingAs)
		}
		if ok && s.PendingActAs != nil {
			out["pending_act_as"] = pendingJSON(s.PendingActAs)
		}
		ui.PrintJSON(out)
		return
	}
	switch {
	case !ok:
		fmt.Printf("Not logged in to %s.\n", c.host)
	case s.ActingAs != nil:
		a := s.ActingAs
		fmt.Printf("Acting as %s (%s, %s) until %s, %s.\n", a.Organization, reachLabel(a.Scope), grantAccessLabel(a.Access), localTime(a.ExpiresAt), untilLabel(a.ExpiresAt))
		fmt.Printf("Reason:      %s\n", a.Reason)
		if a.ApprovedBy != "" {
			fmt.Printf("Approved by: %s\n", a.ApprovedBy)
		}
		if a.WriteBudget != nil {
			fmt.Printf("Write budget: %d changes in production apps when it was granted (the server counts it down)\n", *a.WriteBudget)
		}
		fmt.Printf("Apps:        %s\n", appNames(a.Apps))
		if a.DefaultApp != "" {
			fmt.Printf("Default app: %s (%s)\n", a.DefaultAppName, a.DefaultApp)
		}
		if a.GrantID != "" {
			fmt.Printf("Grant:       %s\n", a.GrantID)
		}
		fmt.Println("`qube act-as --end` stops.")
	case s.PendingActAs != nil:
		p := s.PendingActAs
		fmt.Printf("A grant for %s (%s, %s) is waiting for approval: %s (code %s, until %s).\n", orgLabel(p.Organization, p.OrganizationID), reachLabel(p.Scope), grantAccessLabel(p.Access), p.VerificationURIComplete, p.UserCode, localTime(p.ExpiresAt))
		fmt.Println("`qube act-as --resume` waits for it; `qube act-as --end` drops it.")
	default:
		fmt.Println("Not acting as any organization.")
	}
}

// printActing says the grant is held: on stdout, or as JSON.
func (c *ctx) printActing(a *config.ActAs, prefix string) {
	if ui.JSON {
		ui.PrintJSON(map[string]interface{}{"status": "active", "acting_as": actAsJSON(a)})
		return
	}
	fmt.Printf("%sActing as %s (%s, %s) until %s. Every request is recorded with your reason.\n", prefix, a.Organization, reachLabel(a.Scope), grantAccessLabel(a.Access), localTime(a.ExpiresAt))
	switch {
	case a.DefaultApp != "":
		fmt.Printf("Default app: %s. `qube act-as --end` stops.\n", a.DefaultAppName)
	case len(a.Apps) == 0:
		fmt.Println("It reaches no apps yet. `qube act-as --end` stops.")
	default:
		fmt.Printf("Its apps: %s; pick one with `qube use <app>` or --app. `qube act-as --end` stops.\n", appNames(a.Apps))
	}
	if a.WriteBudget != nil {
		fmt.Printf("Write budget: %d changes in production apps.\n", *a.WriteBudget)
	}
}

func approvedPrefix(a *config.ActAs) string {
	if a.ApprovedBy == "" {
		return "Approved: "
	}
	return "Approved by " + a.ApprovedBy + ": "
}

func approvedLine(a *config.ActAs) string {
	return fmt.Sprintf("%sacting as %s (%s, %s) until %s", approvedPrefix(a), a.Organization, reachLabel(a.Scope), grantAccessLabel(a.Access), localTime(a.ExpiresAt))
}

// ---------------------------------------------------------------- words

func appNames(apps []config.App) string {
	if len(apps) == 0 {
		return "none"
	}
	var names []string
	for _, a := range apps {
		kind := "production"
		if a.Sandbox {
			kind = "sandbox"
		}
		names = append(names, fmt.Sprintf("%s (%s)", a.Name, kind))
	}
	return strings.Join(names, ", ")
}

func configApps(apps []api.App) []config.App {
	out := make([]config.App, 0, len(apps))
	for _, a := range apps {
		out = append(out, config.App{ID: a.ID, Name: a.Name, Sandbox: a.Sandbox})
	}
	return out
}

func apiApps(apps []config.App) []api.App {
	out := make([]api.App, 0, len(apps))
	for _, a := range apps {
		out = append(out, api.App{ID: a.ID, Name: a.Name, Sandbox: a.Sandbox})
	}
	return out
}

// actAsJSON describes a grant for --json, never with its token.
func actAsJSON(a *config.ActAs) map[string]interface{} {
	apps := a.Apps
	if apps == nil {
		apps = []config.App{}
	}
	out := map[string]interface{}{
		"organization": map[string]string{"id": a.OrganizationID, "name": a.Organization},
		"grant_id":     a.GrantID,
		"scope":        a.Scope,
		"access":       a.Access,
		"reason":       a.Reason,
		"write_budget": a.WriteBudget,
		"expires_at":   a.ExpiresAt,
		"approved_by":  a.ApprovedBy,
		"apps":         apps,
		"default_app":  nil,
	}
	if a.DefaultApp != "" {
		out["default_app"] = map[string]string{"id": a.DefaultApp, "name": a.DefaultAppName}
	}
	return out
}

// pendingJSON describes a grant waiting for approval, never with its device code.
func pendingJSON(p *config.PendingActAs) map[string]interface{} {
	return map[string]interface{}{
		"organization":              map[string]string{"id": p.OrganizationID, "name": p.Organization},
		"user_code":                 p.UserCode,
		"verification_uri_complete": p.VerificationURIComplete,
		"expires_at":                p.ExpiresAt,
		"scope":                     p.Scope,
		"access":                    p.Access,
		"minutes":                   p.Minutes,
		"reason":                    p.Reason,
	}
}
