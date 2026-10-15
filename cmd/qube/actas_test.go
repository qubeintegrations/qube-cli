package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

const reason = "Onboarding: install and test invoice sync"

// orgsHandler answers GET /api/admin/organizations?search= with orgs, and the record of one.
func orgsHandler(orgs ...map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var out []map[string]interface{}
		search := strings.ToLower(r.URL.Query().Get("search"))
		for _, o := range orgs {
			if strings.Contains(strings.ToLower(o["name"].(string)), search) {
				out = append(out, o)
			}
		}
		writeJSON(w, 200, map[string]interface{}{"data": out, "meta": map[string]interface{}{"page": 1, "total_pages": 1}})
	}
}

func org(id, name string) map[string]interface{} {
	return map[string]interface{}{"id": id, "name": name, "owners": []map[string]string{{"email": "owner@" + strings.ToLower(strings.Fields(name)[0]) + ".example"}}}
}

func inAnHour() string { return time.Now().Add(time.Hour).UTC().Format(time.RFC3339) }

// A 201: the grant made at once.
func grantMade(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 201, map[string]interface{}{"data": map[string]interface{}{
		"status": "active", "token": "qct_grant",
		"grant": map[string]interface{}{"id": "grant-1", "kind": "act_as", "scope": "sandbox", "access": "read_write", "reason": reason,
			"expires_at": inAnHour(), "organization": map[string]string{"id": acmeID, "name": "Acme Corp"}, "approved_by": nil},
		"apps": []map[string]interface{}{{"id": "app1", "name": "App", "sandbox": true}},
	}})
}

// A 202: the grant waits for approval.
func grantPending(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 202, map[string]interface{}{"data": map[string]interface{}{
		"status": "pending_approval",
		"approval": map[string]interface{}{"device_code": "qcd_secret", "user_code": "W2HT-9RNC",
			"verification_uri": "https://qubesync.example/app/cli/activate", "verification_uri_complete": "https://qubesync.example/app/cli/activate?code=W2HT-9RNC",
			"expires_in": 600, "interval": 0},
		"request": map[string]interface{}{"id": "req-1", "organization_id": acmeID, "organization": map[string]string{"id": acmeID, "name": "Acme Corp"},
			"scope": "all", "access": "configure", "minutes": 30, "reason": reason, "write_budget": nil},
	}})
}

// tokenAnswers answers POST /api/cli/token with each answer in turn ("pending", "approved",
// "denied", "expired"), the last one from then on, and counts the device codes it was sent.
type tokenAnswers struct {
	mu      sync.Mutex
	answers []string
	codes   []string
}

func (ta *tokenAnswers) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	ta.mu.Lock()
	ta.codes = append(ta.codes, body["device_code"])
	a := ta.answers[0]
	if len(ta.answers) > 1 {
		ta.answers = ta.answers[1:]
	}
	ta.mu.Unlock()
	switch a {
	case "pending":
		writeJSON(w, 400, map[string]interface{}{"error": map[string]interface{}{"code": "authorization_pending", "message": "waiting", "interval": 5}})
	case "denied":
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"code": "access_denied", "message": "the login was denied in the dashboard"}})
	case "expired":
		writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"code": "expired_token", "message": "this login attempt expired"}})
	default:
		writeJSON(w, 200, map[string]interface{}{"id": "grant-2", "token": "qct_grant", "token_type": "Bearer", "expires_at": inAnHour(), "scope": "all",
			"apps":   []map[string]interface{}{{"id": "app1", "name": "App", "sandbox": true}, {"id": "app2", "name": "App Prod", "sandbox": false}},
			"access": "configure", "kind": "act_as", "actor": "human", "permissions": []string{}, "reason": reason, "write_budget": nil,
			"parent_id": "sess-1", "approved_by": map[string]string{"email": "garrett@qubesync.com"},
			"user": map[string]string{"email": "garrett@qubesync.com"}, "organization": map[string]string{"id": acmeID, "name": "Acme Corp"}})
	}
}

// A person's sandbox grant is made at once: the name resolves to one exact match, the staff
// token asks, and the grant becomes the overlay with its apps.
func TestActAsSandboxGrant(t *testing.T) {
	var auth string
	var body map[string]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/organizations": orgsHandler(org(acmeID, "Acme Corp"), org(acmeWestID, "Acme Corp West")),
		"/api/cli/act-as": func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			_ = json.NewDecoder(r.Body).Decode(&body)
			grantMade(w, r)
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.actAs([]string{"acme corp", "--reason", reason, "--for", "2h"}) })
	if auth != "Bearer qct_staff" {
		t.Fatalf("asked with %q", auth)
	}
	if body["organization_id"] != acmeID || body["reason"] != reason || body["minutes"] != float64(120) || body["scope"] != "sandbox" || body["access"] != nil || body["write_budget"] != nil {
		t.Fatalf("body = %v", body)
	}
	a := saved(t, c).ActingAs
	if a == nil || a.Token != "qct_grant" || a.GrantID != "grant-1" || a.Organization != "Acme Corp" || a.DefaultApp != "app1" || len(a.Apps) != 1 {
		t.Fatalf("overlay = %+v", a)
	}
	if !strings.HasPrefix(out, "Acting as Acme Corp (sandbox apps, read and write) until ") || !strings.Contains(out, "Default app: App.") {
		t.Fatalf("output = %q", out)
	}
	if strings.Contains(out, "qct_") {
		t.Fatalf("the token was printed: %q", out)
	}
}

// Under --json the 201 prints the grant, never its token.
func TestActAsJSONShape(t *testing.T) {
	c := staffCtx(t, true, map[string]http.HandlerFunc{"/api/cli/act-as": grantMade})
	setJSON(t, true)
	out := captureStdout(t, func() { c.actAs([]string{acmeID, "--reason", reason, "--access", "read_write", "--write-budget", "0"}) })
	var got struct {
		Status   string                 `json:"status"`
		ActingAs map[string]interface{} `json:"acting_as"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.Status != "active" || got.ActingAs["access"] != "read_write" || got.ActingAs["scope"] != "sandbox" {
		t.Fatalf("%v %s", err, out)
	}
	if strings.Contains(out, "qct_") {
		t.Fatalf("the token was printed: %s", out)
	}
	out = captureStdout(t, func() { c.actAs([]string{"--status"}) })
	var status map[string]interface{}
	if err := json.Unmarshal([]byte(out), &status); err != nil || status["acting_as"] == nil || status["pending_act_as"] != nil || strings.Contains(out, "qct_") {
		t.Fatalf("status: %v %s", err, out)
	}
}

// A production grant waits for approval: the link goes to stderr, the CLI polls
// /api/cli/token until it's approved, and keeps the id and apps the token's answer carries.
func TestActAsProductionWaitsForApproval(t *testing.T) {
	ta := &tokenAnswers{answers: []string{"pending", "pending", "approved"}}
	var body map[string]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/act-as": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&body)
			grantPending(w, r)
		},
		"/api/cli/token": ta.handle,
	})
	setJSON(t, false)
	var out string
	errOut := captureStderr(t, func() {
		out = captureStdout(t, func() { c.actAs([]string{acmeID, "--production", "--for", "30m", "--reason", reason}) })
	})
	if body["scope"] != "all" || body["minutes"] != float64(30) {
		t.Fatalf("body = %v", body)
	}
	if len(ta.codes) != 3 || ta.codes[2] != "qcd_secret" {
		t.Fatalf("token polls = %v", ta.codes)
	}
	if !strings.Contains(errOut, "A production grant is approved in the browser") || !strings.Contains(errOut, "https://qubesync.example/app/cli/activate?code=W2HT-9RNC") {
		t.Fatalf("stderr = %q", errOut)
	}
	if !strings.HasPrefix(out, "Approved by garrett@qubesync.com: Acting as Acme Corp (every app, configure) until ") {
		t.Fatalf("stdout = %q", out)
	}
	s := saved(t, c)
	if s.PendingActAs != nil || s.ActingAs == nil || s.ActingAs.ApprovedBy != "garrett@qubesync.com" || s.ActingAs.Access != "configure" ||
		s.ActingAs.GrantID != "grant-2" || len(s.ActingAs.Apps) != 2 || s.ActingAs.Apps[1].Name != "App Prod" {
		t.Fatalf("saved %+v %+v", s.PendingActAs, s.ActingAs)
	}
	if strings.Contains(out+errOut, "qcd_secret") {
		t.Fatal("the device code was printed")
	}
}

// A denied grant stops the command and is dropped.
func TestActAsDenied(t *testing.T) {
	if inChild(t, func(c *ctx) { c.actAs([]string{acmeID, "--production", "--reason", reason}) }) {
		return
	}
	ta := &tokenAnswers{answers: []string{"pending", "denied"}}
	c := staffCtx(t, true, map[string]http.HandlerFunc{"/api/cli/act-as": grantPending, "/api/cli/token": ta.handle})
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "the grant for Acme Corp was denied in the browser") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if s := saved(t, c); s.PendingActAs != nil || s.ActingAs != nil {
		t.Fatalf("kept %+v %+v", s.PendingActAs, s.ActingAs)
	}
}

// A grant nobody approved in time stops the command and is dropped.
func TestActAsExpiredBeforeApproval(t *testing.T) {
	if inChild(t, func(c *ctx) { c.actAs([]string{"--resume"}) }) {
		return
	}
	ta := &tokenAnswers{answers: []string{"expired"}}
	c := staffCtx(t, true, map[string]http.HandlerFunc{"/api/cli/token": ta.handle})
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = &config.PendingActAs{DeviceCode: "qcd_secret", UserCode: "W2HT-9RNC", VerificationURIComplete: "https://qubesync.example/a", ExpiresAt: inAnHour(), OrganizationID: acmeID, Organization: "Acme Corp", Scope: "all"}
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "the grant for Acme Corp expired before anyone approved it") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if saved(t, c).PendingActAs != nil {
		t.Fatal("the expired grant was kept")
	}
}

// --no-wait keeps the grant pending (never printing its device code), and --resume collects it.
func TestActAsNoWaitThenResume(t *testing.T) {
	ta := &tokenAnswers{answers: []string{"pending", "approved"}}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/organizations/" + acmeID: func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]interface{}{"data": org(acmeID, "Acme Corp")})
		},
		"/api/cli/act-as": grantPending,
		"/api/cli/token":  ta.handle,
	})
	s := c.cfg.Sessions[c.host]
	s.Actor, s.AgentName = "agent", "Claude Code"
	c.cfg.Sessions[c.host] = s
	setJSON(t, false)
	out := captureStdout(t, func() {
		c.actAs([]string{acmeID, "--production", "--reason", reason, "--access", "configure", "--no-wait"})
	})
	for _, want := range []string{"Asked to act as Acme Corp (every app, configure, 30 min).", "This is an agent's session, so its person approves the grant.",
		"Ask garrett@qubesync.com to open https://qubesync.example/app/cli/activate?code=W2HT-9RNC and approve code W2HT-9RNC", "qube act-as --resume"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "qcd_secret") || len(ta.codes) != 0 {
		t.Fatalf("printed the device code or polled: %s %v", out, ta.codes)
	}
	p := saved(t, c).PendingActAs
	if p == nil || p.DeviceCode != "qcd_secret" || p.RequestID != "req-1" || p.Organization != "Acme Corp" || p.OrganizationID != acmeID {
		t.Fatalf("pending = %+v", p)
	}
	out = captureStdout(t, func() { _ = captureStderr(t, func() { c.actAs([]string{"--resume"}) }) })
	if !strings.HasPrefix(out, "Approved by garrett@qubesync.com: Acting as Acme Corp") || len(ta.codes) != 2 {
		t.Fatalf("resume: %q, polls %v", out, ta.codes)
	}
	if s := saved(t, c); s.PendingActAs != nil || s.ActingAs == nil {
		t.Fatalf("after resume: %+v %+v", s.PendingActAs, s.ActingAs)
	}
}

// The next command collects an approved pending grant, says so on stderr, and uses it.
func TestNextCommandCollectsAnApprovedGrant(t *testing.T) {
	ta := &tokenAnswers{answers: []string{"approved"}}
	var auth string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/token": ta.handle,
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			connectionsHandler(w, r)
		},
	})
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = &config.PendingActAs{DeviceCode: "qcd_secret", UserCode: "W2HT-9RNC", ExpiresAt: inAnHour(), OrganizationID: acmeID, Organization: "Acme Corp", Scope: "all"}
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	setJSON(t, true)
	errOut := captureStderr(t, c.enterActAs)
	if !strings.Contains(errOut, "Approved by garrett@qubesync.com: acting as Acme Corp (every app, configure)") || !strings.Contains(errOut, "(acting as Acme Corp · every app · configure · ") {
		t.Fatalf("stderr = %q", errOut)
	}
	_ = captureStdout(t, func() { c.connections([]string{"list"}) })
	if auth != "Bearer qct_grant" {
		t.Fatalf("the command carried %q", auth)
	}
}

// A grant still waiting for approval stops a command rather than letting it run in the
// person's own organization.
func TestPendingGrantStopsCommands(t *testing.T) {
	if inChild(t, func(c *ctx) {
		c.enterActAs()
		c.connections([]string{"list"})
	}) {
		return
	}
	ta := &tokenAnswers{answers: []string{"pending"}}
	var sent []string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/token":      ta.handle,
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) { sent = append(sent, r.URL.Path) },
	})
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = &config.PendingActAs{DeviceCode: "qcd_secret", UserCode: "W2HT-9RNC", VerificationURIComplete: "https://qubesync.example/a?code=W2HT-9RNC", ExpiresAt: inAnHour(), OrganizationID: acmeID, Organization: "Acme Corp"}
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "the grant for Acme Corp is waiting for approval") || len(sent) > 0 {
		t.Fatalf("exit %d, sent %v:\n%s", code, sent, out)
	}
	if saved(t, c).PendingActAs == nil {
		t.Fatal("a grant that may still be approved was dropped")
	}
}

// A name several organizations have is refused, listing them, and no grant is asked for.
func TestActAsRefusesAnAmbiguousName(t *testing.T) {
	if inChild(t, func(c *ctx) { c.actAs([]string{"Acme Corp", "--reason", reason}) }) {
		return
	}
	var asked bool
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/organizations": orgsHandler(org(acmeID, "Acme Corp"), org(acmeOtherID, "ACME Corp"), org(acmeWestID, "Acme Corp West")),
		"/api/cli/act-as":          func(w http.ResponseWriter, r *http.Request) { asked = true },
	})
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, `2 organizations are named "Acme Corp"; use the id:`) ||
		!strings.Contains(out, acmeID+"  Acme Corp  (owners: owner@acme.example)") || !strings.Contains(out, acmeOtherID) || strings.Contains(out, acmeWestID) || asked {
		t.Fatalf("exit %d, asked %v:\n%s", code, asked, out)
	}
}

// A name no organization has exactly lists the near matches.
func TestActAsRefusesAnInexactName(t *testing.T) {
	if inChild(t, func(c *ctx) { c.actAs([]string{"Acme", "--reason", reason}) }) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/organizations": orgsHandler(org(acmeID, "Acme Corp"), org(acmeWestID, "Acme Corp West")),
	})
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, `no organization is named "Acme" exactly`) || !strings.Contains(out, acmeWestID) {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// While a grant is held, the grant's token goes to v1/v2 and the rest of /api/cli, with the
// grant's app; /api/admin and POST /api/cli/act-as carry the staff token.
func TestActAsRoutesTokens(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	record := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization") + " app=" + r.Header.Get("X-Qube-App")
		mu.Unlock()
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) { record(w, r); connectionsHandler(w, r) },
		"/api/cli/me": func(w http.ResponseWriter, r *http.Request) {
			record(w, r)
			writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"user": map[string]string{"email": "garrett@qubesync.com"},
				"organization": map[string]string{"name": "Acme Corp"}, "session": map[string]interface{}{"kind": "act_as", "scope": "sandbox", "access": "read_write", "reason": reason}}})
		},
		"/api/admin/status": func(w http.ResponseWriter, r *http.Request) {
			record(w, r)
			writeJSON(w, 200, map[string]interface{}{"data": statusSnapshot(false)})
		},
		"/api/cli/act-as": func(w http.ResponseWriter, r *http.Request) { record(w, r); grantMade(w, r) },
	})
	actingAs(t, c, "read_write")
	setJSON(t, false)
	out := captureStdout(t, func() {
		c.connections([]string{"list"})
		c.whoami()
		_ = c.adminStatus(c.staffClient(), nil)
		_ = c.cli().Do("GET", "/api/admin/status", nil, nil, nil)
		_ = c.cli().Do("POST", "/api/cli/act-as", nil, map[string]string{}, nil)
	})
	want := map[string]string{
		"GET /api/v2/connections": "Bearer qct_grant app=app1",
		"GET /api/cli/me":         "Bearer qct_grant app=",
		"GET /api/admin/status":   "Bearer qct_staff app=",
		"POST /api/cli/act-as":    "Bearer qct_staff app=",
	}
	for route, w := range want {
		if got[route] != w {
			t.Errorf("%s carried %q, want %q", route, got[route], w)
		}
	}
	if !strings.Contains(out, "acting as Acme Corp") {
		t.Fatalf("whoami: %s", out)
	}
}

// `qube act-as --end` revokes the grant with its own token and drops it.
func TestActAsEnd(t *testing.T) {
	var auth, method string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/session": func(w http.ResponseWriter, r *http.Request) {
			auth, method = r.Header.Get("Authorization"), r.Method
			w.WriteHeader(204)
		},
	})
	actingAs(t, c, "read_write")
	setJSON(t, false)
	out := captureStdout(t, func() { c.actAs([]string{"--end"}) })
	if method != "DELETE" || auth != "Bearer qct_grant" {
		t.Fatalf("%s with %q", method, auth)
	}
	if saved(t, c).ActingAs != nil || out != "Stopped acting as Acme Corp. Commands use your own session again (QuBe Sync).\n" {
		t.Fatalf("output %q", out)
	}
	out = captureStdout(t, func() { c.actAs([]string{"--end"}) })
	if out != "Not acting as any organization.\n" {
		t.Fatalf("second end: %q", out)
	}
}

// A grant the server refuses (401) is dropped and the command stops: nothing is sent again
// with the staff token.
func TestRevokedGrantStopsAndIsDropped(t *testing.T) {
	if inChild(t, func(c *ctx) { c.connections([]string{"list"}) }) {
		return
	}
	var mu sync.Mutex
	var auths []string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			auths = append(auths, r.Header.Get("Authorization"))
			mu.Unlock()
			writeJSON(w, 401, map[string]interface{}{"errors": map[string]string{"detail": "This CLI session has expired or was revoked.", "code": "cli_token_invalid"}})
		},
	})
	actingAs(t, c, "read_write")
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "the act-as grant for Acme Corp has ended") || !strings.Contains(out, "instead of using your own session") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if strings.Join(auths, ",") != "Bearer qct_grant" {
		t.Fatalf("sent with %v", auths)
	}
	if s := saved(t, c); s.ActingAs != nil || s.Token != "qct_staff" {
		t.Fatalf("after the 401: %+v", s)
	}
}

// A grant whose time is up is dropped when the config is read, and the next command that
// would have used it stops instead of running in the person's own organization.
func TestExpiredGrantStopsTheNextCommand(t *testing.T) {
	if inChild(t, func(c *ctx) {
		c.enterActAs()
		c.connections([]string{"list"})
	}) {
		return
	}
	var sent []string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) { sent = append(sent, r.Header.Get("Authorization")) },
	})
	actingAs(t, c, "read_write")
	s := c.cfg.Sessions[c.host]
	s.ActingAs.ExpiresAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "the act-as grant for Acme Corp ended at") || len(sent) > 0 {
		t.Fatalf("exit %d, sent %v:\n%s", code, sent, out)
	}
	if saved(t, c).ActingAs != nil {
		t.Fatal("the expired grant was kept")
	}
}

// The banner names the organization, access and time left, on stderr, under --json too.
func TestBannerOnStderrUnderJSON(t *testing.T) {
	c := staffCtx(t, true, nil)
	actingAs(t, c, "read_write")
	setJSON(t, true)
	var out string
	errOut := captureStderr(t, func() { out = captureStdout(t, c.enterActAs) })
	if errOut != "(acting as Acme Corp · sandbox apps · read and write · 1h 30m left)\n" || out != "" {
		t.Fatalf("stderr %q, stdout %q", errOut, out)
	}
	for args, want := range map[string]bool{"admin status": false, "act-as --end": false, "status": false, "login": false,
		"qb --complete customers": false, "connections list": true, "qb customers list": true, "whoami": true, "env": true} {
		if got := usesActAs(strings.Fields(args)); got != want {
			t.Errorf("usesActAs(%q) = %v", args, got)
		}
	}
}

// A read-only grant refuses a write before sending it (exit 1); reads go.
func TestReadOnlyGrantRefusesWrites(t *testing.T) {
	if args := os.Getenv("QUBE_TEST_READONLY_ARGS"); args != "" {
		if inChild(t, func(c *ctx) {
			c.yes = true
			a := strings.Split(args, " ")
			switch a[0] {
			case "connections":
				c.connections(a[1:])
			case "qb":
				c.qb(a[1:])
			case "workflows":
				c.workflows(a[1:])
			}
		}) {
			return
		}
	}
	for _, args := range []string{
		"connections create --simulated",
		"connections update conn_1 --name Riverbend",
		"connections delete conn_1",
		"qb customers create conn_1 --name Northwind",
		"workflows publish invoice_sync",
	} {
		var mu sync.Mutex
		var sent []string
		c := staffCtx(t, true, map[string]http.HandlerFunc{
			"/api/v2/": func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				if r.Method != "GET" {
					sent = append(sent, r.Method+" "+r.URL.Path)
				}
				mu.Unlock()
				writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"id": "conn_1", "name": "Riverbend"}})
			},
			"/api/v2/openapi.json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(qbSpec)) },
		})
		actingAs(t, c, "read_only")
		code, out := runChild(t, c, "QUBE_TEST_READONLY_ARGS="+args)
		if code != ui.ExitFailure || !strings.Contains(out, "this act-as grant for Acme Corp is read-only") || len(sent) > 0 {
			t.Errorf("%s: exit %d, sent %v:\n%s", args, code, sent, out)
		}
	}
	var method string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) { method = r.Method; connectionsHandler(w, r) },
	})
	actingAs(t, c, "read_only")
	setJSON(t, true)
	_ = captureStdout(t, func() { c.connections([]string{"list"}) })
	if method != "GET" {
		t.Fatalf("a read-only grant should still read; server saw %q", method)
	}
}

// `qube env` explains why it doesn't work while acting, without asking for the credentials.
func TestEnvRefusedWhileActing(t *testing.T) {
	if inChild(t, func(c *ctx) { c.env([]string{"--write", os.Getenv("QUBE_TEST_ENV_FILE")}) }) {
		return
	}
	var asked bool
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/apps/app1/credentials": func(w http.ResponseWriter, r *http.Request) { asked = true },
	})
	actingAs(t, c, "read_write")
	envFile := t.TempDir() + "/.env"
	code, out := runChild(t, c, "QUBE_TEST_ENV_FILE="+envFile)
	if code != ui.ExitFailure || !strings.Contains(out, "qube env doesn't work while acting as Acme Corp") || asked {
		t.Fatalf("exit %d, asked %v:\n%s", code, asked, out)
	}
	if _, err := os.Stat(envFile); err == nil {
		t.Fatal("an env file was written")
	}
}

// Inside a grant, a production confirmation names the customer's organization.
func TestProductionConfirmationNamesTheCustomer(t *testing.T) {
	if inChild(t, func(c *ctx) { c.connections([]string{"create", "--simulated"}) }) {
		return
	}
	var sent bool
	c := staffCtx(t, false, map[string]http.HandlerFunc{
		"/api/v2/connections": func(w http.ResponseWriter, r *http.Request) { sent = r.Method == "POST" },
	})
	actingAs(t, c, "configure")
	s := c.cfg.Sessions[c.host]
	s.ActingAs.Scope = "all"
	s.ActingAs.Apps[0].Sandbox = false
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, `Create a connection in Acme Corp's production app "App"? Pass --yes`) || sent {
		t.Fatalf("exit %d, sent %v:\n%s", code, sent, out)
	}
}

// `qube use` while acting changes the grant's default app, not the session's own.
func TestUseWhileActingKeepsTheSessionsDefaults(t *testing.T) {
	c := staffCtx(t, true, map[string]http.HandlerFunc{"/api/v2/connections": connectionsHandler})
	s := c.cfg.Sessions[c.host]
	s.DefaultApp, s.DefaultAppName = "own-app", "Own App"
	c.cfg.Sessions[c.host] = s
	actingAs(t, c, "read_write")
	setJSON(t, true)
	c.connection = "Northwind"
	_ = captureStderr(t, func() { c.use([]string{"App"}) })
	got := saved(t, c)
	if got.DefaultApp != "own-app" || got.ActingAs.DefaultApp != "app1" || got.ActingAs.DefaultConnections["app1"].ID != "conn_2" || len(got.DefaultConnections) != 0 {
		t.Fatalf("session %+v, grant %+v", got, got.ActingAs)
	}
}

// One grant at a time: asking for another while one is held says to end it first.
func TestActAsRefusesASecondGrant(t *testing.T) {
	if inChild(t, func(c *ctx) { c.actAs([]string{acmeWestID, "--reason", reason}) }) {
		return
	}
	var asked bool
	c := staffCtx(t, true, map[string]http.HandlerFunc{"/api/cli/act-as": func(w http.ResponseWriter, r *http.Request) { asked = true }})
	actingAs(t, c, "read_write")
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "already acts as Acme Corp") || !strings.Contains(out, "qube act-as --end") || asked {
		t.Fatalf("exit %d, asked %v:\n%s", code, asked, out)
	}
}

// The server's refusals of a grant reach the person in words: a configure grant asked to
// queue qbXML, and a spent write budget.
func TestGrantRefusalsReadClearly(t *testing.T) {
	if inChild(t, func(c *ctx) {
		c.yes = true
		c.workflows([]string{"publish", "invoice_sync"})
	}) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/v2/workflows/invoice_sync/publish": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 403, map[string]interface{}{"errors": map[string]string{"code": "write_budget_spent",
				"detail": "This act-as grant has used its write budget. Ask for a new grant (qube act-as) to make more changes."}})
		},
	})
	actingAs(t, c, "configure")
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "write_budget_spent") || !strings.Contains(out, "used its write budget") || !strings.Contains(out, "`qube act-as --end`") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A grant approved in the browser keeps the id and apps the token's answer carries, with no
// extra call to list the apps.
func TestApprovedGrantKeepsItsIDAndApps(t *testing.T) {
	ta := &tokenAnswers{answers: []string{"approved"}}
	listed := false
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/token": ta.handle,
		"/api/cli/apps": func(w http.ResponseWriter, r *http.Request) {
			listed = true
			writeJSON(w, 200, map[string]interface{}{"data": []interface{}{}})
		},
	})
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = &config.PendingActAs{RequestID: "req-1", DeviceCode: "qcd_secret", UserCode: "W2HT-9RNC", VerificationURIComplete: "https://qubesync.example/a", ExpiresAt: inAnHour(), OrganizationID: acmeID, Organization: "Acme Corp", Scope: "all"}
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	setJSON(t, false)
	_ = captureStdout(t, func() { _ = captureStderr(t, func() { c.actAs([]string{"--resume"}) }) })
	a := saved(t, c).ActingAs
	if a == nil || a.GrantID != "grant-2" || len(a.Apps) != 2 || listed {
		t.Fatalf("overlay = %+v, listed apps separately: %v", a, listed)
	}
}

// --end withdraws a grant still waiting for approval on the server, with the staff token.
func TestActAsEndWithdrawsAPendingGrant(t *testing.T) {
	var method, auth string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/grants/req-1": func(w http.ResponseWriter, r *http.Request) {
			method, auth = r.Method, r.Header.Get("Authorization")
			writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"id": "req-1", "status": "cancelled"}})
		},
	})
	s := c.cfg.Sessions[c.host]
	s.PendingActAs = &config.PendingActAs{RequestID: "req-1", DeviceCode: "qcd_secret", UserCode: "W2HT-9RNC", VerificationURIComplete: "https://qubesync.example/a", ExpiresAt: inAnHour(), OrganizationID: acmeID, Organization: "Acme Corp", Scope: "all"}
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	setJSON(t, false)
	out := captureStdout(t, func() { c.actAs([]string{"--end"}) })
	if method != "DELETE" || auth != "Bearer qct_staff" {
		t.Fatalf("withdrew with %s %q", method, auth)
	}
	if !strings.Contains(out, "Withdrew the grant for Acme Corp that was waiting for approval") {
		t.Fatalf("output = %q", out)
	}
	if saved(t, c).PendingActAs != nil {
		t.Fatal("the pending grant was kept")
	}
}
