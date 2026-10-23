package main

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

const (
	acmeID      = "6f1c2c1e-1111-4000-8000-000000000001"
	acmeOtherID = "6f1c2c1e-1111-4000-8000-000000000002"
	acmeWestID  = "6f1c2c1e-1111-4000-8000-000000000003"
)

// staffCtx is newAPITestApp for a person's staff session holding every staff permission. Its
// config is saved in a temp QUBE_CONFIG; HOME, XDG_CONFIG_HOME and AppData point at a temp
// directory; no browser opens; and approval polls don't wait.
func staffCtx(t *testing.T, sandbox bool, handlers map[string]http.HandlerFunc) *ctx {
	t.Helper()
	withDeviceDir(t)
	setenv(t, "QUBE_NO_BROWSER", "1")
	fastApprovals(t)
	c := newAPITestApp(t, sandbox, handlers)
	s := c.cfg.Sessions[c.host]
	s.Token, s.UserEmail, s.Organization = "qct_staff", "garrett@qubesync.com", "QuBe Sync"
	s.ExpiresAt = time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339)
	s.Permissions = api.StaffPermissions
	c.cfg.Sessions[c.host] = s
	withConfig(t, c)
	return c
}

func fastApprovals(t *testing.T) {
	min, def := approvalPollMin, approvalPollDefault
	approvalPollMin, approvalPollDefault = time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { approvalPollMin, approvalPollDefault = min, def })
}

// actingAs gives c's session a grant in Acme Corp's sandbox apps, with an hour and a half left.
func actingAs(t *testing.T, c *ctx, access string) {
	t.Helper()
	s := c.cfg.Sessions[c.host]
	s.ActingAs = &config.ActAs{
		Token: "qct_grant", GrantID: "grant-1", OrganizationID: acmeID, Organization: "Acme Corp",
		Scope: "sandbox", Access: access, Reason: "Onboarding: install invoice sync",
		ExpiresAt: time.Now().Add(90*time.Minute + 30*time.Second).UTC().Format(time.RFC3339),
		Apps:      []config.App{{ID: "app1", Name: "App", Sandbox: true}}, DefaultApp: "app1", DefaultAppName: "App",
	}
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		t.Fatal(err)
	}
}

// saved is this host's session as the config file now holds it.
func saved(t *testing.T, c *ctx) config.Session {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Sessions[c.host]
}

// inChild runs body in a fresh copy of the test binary, where ui.Fail and os.Exit end only
// that copy, against the parent's fake server and config (QUBE_CONFIG). In the child it runs
// body and exits, so a test calls it first; in the parent it returns false.
func inChild(t *testing.T, body func(c *ctx)) bool {
	if os.Getenv("QUBE_TEST_CHILD") != t.Name() {
		return false
	}
	cfg, err := config.Load()
	if err != nil {
		panic(err)
	}
	c := &ctx{host: os.Getenv("QUBE_TEST_HOST"), cfg: cfg, bg: context.Background(), yes: os.Getenv("QUBE_TEST_YES") == "1"}
	ui.JSON = os.Getenv("QUBE_TEST_JSON") == "1"
	approvalPollMin, approvalPollDefault = time.Millisecond, 5*time.Millisecond
	c.dropEndedActAs()
	body(c)
	os.Exit(0)
	return true
}

// runChild runs the current test in a child (see inChild): its exit code and everything it
// printed.
func runChild(t *testing.T, c *ctx, env ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "QUBE_TEST_CHILD="+t.Name(), "QUBE_TEST_HOST="+c.host, "QUBE_CACHE_DIR="+t.TempDir())
	if ui.JSON {
		cmd.Env = append(cmd.Env, "QUBE_TEST_JSON=1")
	}
	if c.yes {
		cmd.Env = append(cmd.Env, "QUBE_TEST_YES=1")
	}
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ---------------------------------------------------------------- login

// What each flag combination asks for: --admin every staff permission, --agent NAME an
// agent's session, and both the agent ceiling.
func TestLoginRequestByFlags(t *testing.T) {
	cases := []struct {
		readOnly, admin bool
		agent           string
		want            api.DeviceRequest
	}{
		{false, false, "", api.DeviceRequest{Access: "read_write"}},
		{true, false, "", api.DeviceRequest{Access: "read_only"}},
		{false, true, "", api.DeviceRequest{Access: "read_write", Permissions: api.StaffPermissions}},
		{false, false, " Claude Code ", api.DeviceRequest{Access: "read_write", AgentName: "Claude Code"}},
		{false, true, "Claude Code", api.DeviceRequest{Access: "read_write", AgentName: "Claude Code", Permissions: api.AgentPermissions}},
		{true, true, "Claude Code", api.DeviceRequest{Access: "read_only", AgentName: "Claude Code", Permissions: api.AgentPermissions}},
	}
	for _, tc := range cases {
		if got := loginRequest(tc.readOnly, tc.admin, tc.agent); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("read-only %v admin %v agent %q: %+v, want %+v", tc.readOnly, tc.admin, tc.agent, got, tc.want)
		}
	}
	if len(api.AgentPermissions) != 5 || contains(api.AgentPermissions, "admin.read_audit") || contains(api.AgentPermissions, "agents.manage") {
		t.Fatalf("the agent ceiling is wrong: %v", api.AgentPermissions)
	}
	// builds.manage is offered to a person and to an agent; approving a gate is a permission
	// no session is ever asked for
	for _, list := range [][]string{api.StaffPermissions, api.AgentPermissions} {
		if !contains(list, "builds.manage") || contains(list, "admin.write") {
			t.Errorf("%v: want builds.manage and never admin.write", list)
		}
	}
}

// `qube login --agent NAME --admin` sends the agent and its ceiling, and keeps who holds the
// session and what it holds.
func TestLoginAgentAdminStoresTheActor(t *testing.T) {
	var device map[string]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/device": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&device)
			writeJSON(w, 200, map[string]interface{}{"device_code": "qcd_login", "user_code": "HQ7M-2KDP",
				"verification_uri_complete": "https://qubesync.example/app/cli/activate?code=HQ7M-2KDP", "expires_in": 600, "interval": 0})
		},
		"/api/cli/token": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]interface{}{"token": "qct_agent", "expires_at": time.Now().Add(4 * time.Hour).UTC().Format(time.RFC3339),
				"scope": "sandbox", "access": "read_write", "kind": "login", "actor": "agent", "agent_name": "Claude Code",
				"permissions": []string{"admin.read"}, "user": map[string]string{"email": "garrett@qubesync.com"},
				"organization": map[string]string{"id": "o", "name": "QuBe Sync"}})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.login([]string{"--agent", "Claude Code", "--admin", "--name", "agent box"}) })
	want := map[string]interface{}{"client_name": "agent box", "access": "read_write", "actor": "agent", "agent_name": "Claude Code",
		"permissions": []interface{}{"admin.read", "admin.read_customer_data", "org.act", "org.act_production", "builds.manage"}}
	if !reflect.DeepEqual(device, want) {
		t.Fatalf("device body = %v", device)
	}
	s := saved(t, c)
	if s.Token != "qct_agent" || !s.Agent() || s.AgentName != "Claude Code" || strings.Join(s.Permissions, ",") != "admin.read" {
		t.Fatalf("saved %+v", s)
	}
	if !strings.Contains(out, `as an agent session for garrett@qubesync.com ("Claude Code"`) || !strings.Contains(out, "Staff: admin.read\n") || !strings.Contains(out, "This session ends at") {
		t.Fatalf("output = %q", out)
	}
}

// A host that predates a permission this CLI asks for (builds.manage, say) refuses the whole
// login and lists what it grants; qube asks again for the rest instead of failing. This one is
// an older host, which lists them only in its message.
func TestLoginAsksAgainForWhatTheHostOffers(t *testing.T) {
	var asked [][]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/device": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = append(asked, body["permissions"].([]interface{}))
			if len(asked) == 1 {
				writeJSON(w, 400, map[string]interface{}{"error": map[string]string{"code": "invalid_request",
					"message": "permissions are some of: admin.read, admin.read_customer_data, admin.read_audit, org.act, org.act_production, agents.manage"}})
				return
			}
			writeJSON(w, 200, map[string]interface{}{"device_code": "qcd_login", "user_code": "HQ7M-2KDP",
				"verification_uri_complete": "https://qubesync.example/app/cli/activate?code=HQ7M-2KDP", "expires_in": 600, "interval": 0})
		},
		"/api/cli/token": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]interface{}{"token": "qct_staff2", "expires_at": time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339),
				"scope": "sandbox", "access": "read_write", "kind": "login", "actor": "human", "permissions": []string{"admin.read", "agents.manage"},
				"user": map[string]string{"email": "garrett@qubesync.com"}, "organization": map[string]string{"id": "o", "name": "QuBe Sync"}})
		},
	})
	setJSON(t, false)
	var out string
	note := captureStderr(t, func() { out = captureStdout(t, func() { c.login([]string{"--admin", "--name", "laptop"}) }) })
	if len(asked) != 2 || !containsAny(asked[0], "builds.manage") || containsAny(asked[1], "builds.manage") || len(asked[1]) != len(asked[0])-1 {
		t.Fatalf("asked %v", asked)
	}
	if !strings.Contains(note, "doesn't offer builds.manage yet; asking for the rest") || !strings.Contains(out, "Staff: admin.read agents.manage") {
		t.Fatalf("stderr %q, stdout %q", note, out)
	}
}

// The same refusal from a host that sends the `permissions` list: qube asks again for what the
// list grants, and doesn't need the message to say so (this one doesn't).
func TestLoginAsksAgainForTheHostsPermissionsList(t *testing.T) {
	var asked [][]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/cli/device": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = append(asked, body["permissions"].([]interface{}))
			if len(asked) == 1 {
				writeJSON(w, 400, map[string]interface{}{"error": map[string]interface{}{"code": "invalid_request",
					"message":     "some of those permissions aren't granted here",
					"permissions": []string{"admin.read", "admin.read_customer_data", "admin.read_audit", "org.act", "org.act_production", "agents.manage"}}})
				return
			}
			writeJSON(w, 200, map[string]interface{}{"device_code": "qcd_login", "user_code": "HQ7M-2KDP",
				"verification_uri_complete": "https://qubesync.example/app/cli/activate?code=HQ7M-2KDP", "expires_in": 600, "interval": 0})
		},
		"/api/cli/token": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]interface{}{"token": "qct_staff3", "expires_at": time.Now().Add(12 * time.Hour).UTC().Format(time.RFC3339),
				"scope": "sandbox", "access": "read_write", "kind": "login", "actor": "human", "permissions": []string{"admin.read", "agents.manage"},
				"user": map[string]string{"email": "garrett@qubesync.com"}, "organization": map[string]string{"id": "o", "name": "QuBe Sync"}})
		},
	})
	setJSON(t, false)
	var out string
	note := captureStderr(t, func() { out = captureStdout(t, func() { c.login([]string{"--admin", "--name", "laptop"}) }) })
	if len(asked) != 2 || !containsAny(asked[0], "builds.manage") || containsAny(asked[1], "builds.manage") || len(asked[1]) != len(asked[0])-1 {
		t.Fatalf("asked %v", asked)
	}
	if !strings.Contains(note, "doesn't offer builds.manage yet; asking for the rest") || !strings.Contains(out, "Staff: admin.read agents.manage") {
		t.Fatalf("stderr %q, stdout %q", note, out)
	}
}

// The structured list is preferred over the message, and the message is only the fallback when
// there is no list.
func TestOfferedPermissionsPrefersTheList(t *testing.T) {
	asked := []string{"admin.read", "org.act", "builds.manage"}
	message := "permissions are some of: admin.read, org.act, builds.manage"
	refusal := func(status int, code, message string, list []string) error {
		return &api.Error{Status: status, Code: code, Message: message, Permissions: list}
	}

	// the list wins over a message that says otherwise
	offered, dropped := offeredPermissions(refusal(400, "invalid_request", message, []string{"admin.read", "org.act"}), asked)
	if !reflect.DeepEqual(offered, []string{"admin.read", "org.act"}) || !reflect.DeepEqual(dropped, []string{"builds.manage"}) {
		t.Fatalf("list beside a message: offered %v, dropped %v", offered, dropped)
	}
	// a message that isn't in the older wording doesn't matter when there is a list
	offered, dropped = offeredPermissions(refusal(400, "invalid_request", "unknown permission", []string{"builds.manage", "org.act"}), asked)
	if !reflect.DeepEqual(offered, []string{"org.act", "builds.manage"}) || !reflect.DeepEqual(dropped, []string{"admin.read"}) {
		t.Fatalf("list without the old wording: offered %v, dropped %v", offered, dropped)
	}
	// no list: the message is read, as an older host sends it
	offered, dropped = offeredPermissions(refusal(400, "invalid_request", "permissions are some of: admin.read", nil), asked)
	if !reflect.DeepEqual(offered, []string{"admin.read"}) || !reflect.DeepEqual(dropped, []string{"org.act", "builds.manage"}) {
		t.Fatalf("message only: offered %v, dropped %v", offered, dropped)
	}
	// an empty list is no list
	offered, dropped = offeredPermissions(refusal(400, "invalid_request", "permissions are some of: admin.read", []string{}), asked)
	if !reflect.DeepEqual(offered, []string{"admin.read"}) || !reflect.DeepEqual(dropped, []string{"org.act", "builds.manage"}) {
		t.Fatalf("empty list: offered %v, dropped %v", offered, dropped)
	}
	// nothing to ask again with, or nothing dropped, or not this refusal at all
	for name, err := range map[string]error{
		"all of them granted": refusal(400, "invalid_request", "", append([]string{"x"}, asked...)),
		"none of them":        refusal(400, "invalid_request", "", []string{"admin.write"}),
		"another status":      refusal(403, "invalid_request", "", []string{"admin.read"}),
		"another code":        refusal(400, "staff_access_disabled", "", []string{"admin.read"}),
		"no list, no wording": refusal(400, "invalid_request", "access is read_only or read_write", nil),
	} {
		if offered, dropped := offeredPermissions(err, asked); offered != nil || dropped != nil {
			t.Errorf("%s: offered %v, dropped %v", name, offered, dropped)
		}
	}
}

func containsAny(list []interface{}, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestOfferedPermissions(t *testing.T) {
	refusal := func(status int, code, message string) error {
		return &api.Error{Status: status, Code: code, Message: message}
	}
	list := "permissions are some of: admin.read, org.act"
	asked := []string{"admin.read", "org.act", "builds.manage"}
	offered, dropped := offeredPermissions(refusal(400, "invalid_request", list), asked)
	if !reflect.DeepEqual(offered, []string{"admin.read", "org.act"}) || !reflect.DeepEqual(dropped, []string{"builds.manage"}) {
		t.Fatalf("offered %v, dropped %v", offered, dropped)
	}
	for name, err := range map[string]error{
		"no error":            nil,
		"another error":       errors.New("boom"),
		"another status":      refusal(403, "invalid_request", list),
		"another code":        refusal(400, "staff_access_disabled", list),
		"another message":     refusal(400, "invalid_request", "access is read_only or read_write"),
		"all of them offered": refusal(400, "invalid_request", list+", builds.manage"),
		"none of them":        refusal(400, "invalid_request", "permissions are some of: admin.write"),
	} {
		if offered, dropped := offeredPermissions(err, asked); offered != nil || dropped != nil {
			t.Errorf("%s: offered %v, dropped %v", name, offered, dropped)
		}
	}
}

// `qube status` shows the actor, agent, permissions and expiry, and the grant held; --json
// never shows the grant's token.
func TestStatusShowsStaffAndTheGrant(t *testing.T) {
	c := staffCtx(t, true, nil)
	s := c.cfg.Sessions[c.host]
	s.Actor, s.AgentName = "agent", "Claude Code"
	c.cfg.Sessions[c.host] = s
	actingAs(t, c, "configure")
	setJSON(t, false)
	out := captureStdout(t, c.status)
	for _, want := range []string{`Actor:        agent "Claude Code"`, "Staff:        admin.read admin.read_customer_data", "Session ends:",
		"Acting as:    Acme Corp (sandbox apps, configure) until", "Reason:     Onboarding: install invoice sync"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	setJSON(t, true)
	out = captureStdout(t, c.status)
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	acting, _ := got["acting_as"].(map[string]interface{})
	if got["actor"] != "agent" || got["agent_name"] != "Claude Code" || len(got["permissions"].([]interface{})) != len(api.StaffPermissions) || acting["access"] != "configure" {
		t.Fatalf("json = %v", got)
	}
	if strings.Contains(out, "qct_") {
		t.Fatalf("a token was printed: %s", out)
	}
}

// `qube sessions` shows kind, actor, staff permissions and expiry when sessions have them, and
// an ordinary account's table only gains EXPIRES.
func TestSessionsShowActorsAndPermissions(t *testing.T) {
	rows := []map[string]interface{}{
		{"id": "s1", "name": "laptop", "scope": "sandbox", "access": "read_write", "active": true, "current": true, "kind": "login", "actor": "human",
			"permissions": []string{"admin.read", "org.act"}, "expires_at": "2026-10-15T02:14:00Z"},
		{"id": "s2", "name": "agent box", "scope": "sandbox", "access": "read_write", "active": true, "kind": "login", "actor": "agent", "agent_name": "Claude Code",
			"permissions": []string{"admin.read"}, "expires_at": "2026-10-14T18:14:00Z"},
		{"id": "g1", "name": "act-as: Acme Corp", "scope": "sandbox", "access": "configure", "active": true, "kind": "act_as", "actor": "human",
			"permissions": []string{}, "expires_at": "2026-10-14T16:05:00Z"},
	}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/cli/sessions": func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]interface{}{"data": rows}) },
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.sessions(nil) })
	for _, want := range []string{"KIND", "ACTOR", "STAFF", "EXPIRES", `agent "Claude Code"`, "act-as", "configure", "admin.read,org.act", "2026-10-15T02:14:00Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("sessions lacks %q:\n%s", want, out)
		}
	}
	rows = rows[:1]
	rows[0]["permissions"] = []string{}
	out = captureStdout(t, func() { c.sessions(nil) })
	if strings.Contains(out, "ACTOR") || strings.Contains(out, "STAFF") || strings.Contains(out, "KIND") || !strings.Contains(out, "EXPIRES") {
		t.Fatalf("an ordinary list:\n%s", out)
	}
}

// ---------------------------------------------------------------- qube admin

// A session without staff permissions is told so before anything is sent.
func TestAdminNeedsAStaffSession(t *testing.T) {
	if inChild(t, func(c *ctx) { c.admin([]string{"status"}) }) {
		return
	}
	var sent []string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/admin/": func(w http.ResponseWriter, r *http.Request) { sent = append(sent, r.URL.Path) },
	})
	withConfig(t, c)
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "holds no staff permissions") || !strings.Contains(out, "qube login --admin") || len(sent) > 0 {
		t.Fatalf("exit %d, sent %v:\n%s", code, sent, out)
	}
}

func statusSnapshot(attention bool) map[string]interface{} {
	return map[string]interface{}{
		"generated_at":   "2026-10-14T14:02:11Z",
		"attention":      attention,
		"errors":         map[string]interface{}{},
		"nodes":          map[string]interface{}{"count": 3, "names": []string{"a", "b", "c"}, "release": "deployment-01JA2", "version": "0.9.1", "attention": false},
		"soap_sessions":  map[string]interface{}{"live": 37, "attention": false},
		"web_connectors": map[string]interface{}{"seen": 212, "window_minutes": 10, "attention": false},
		"queues": map[string]interface{}{"attention": false, "queues": []map[string]interface{}{
			{"queue": "xml", "available": 0, "executing": 2, "retryable": 0, "scheduled": 0},
			{"queue": "webhooks", "available": 4, "executing": 3, "retryable": 1, "scheduled": 12},
		}},
		"failed_jobs": map[string]interface{}{"discarded": 3, "hours": 24, "attention": attention, "by_worker": []map[string]interface{}{
			{"worker": "Qube.Connections.WebhookWorker", "count": 2}, {"worker": "Qube.NameIndex.BuildWorker", "count": 1},
		}},
		"webhooks_failed": map[string]interface{}{"count": 5, "production_apps": 3, "hours": 24, "attention": attention, "by_app": []map[string]interface{}{
			{"app_name": "Globex Prod", "production": true, "count": 5}, {"app_name": "Initech Sandbox", "production": false, "count": 2},
		}},
		"requests_waiting": map[string]interface{}{"connections": 0, "requests": 0, "older_than_minutes": 60, "stalled_connections": 0, "top": []interface{}{}, "attention": false},
		"stuck_runs":       map[string]interface{}{"running_without_progress": 1, "minutes": 30, "run_ids": []string{"run-9"}, "steps_swept_last_hour": 0, "attention": attention},
		"events":           map[string]interface{}{"count": 41, "hours": 24, "attention": false, "last": map[string]interface{}{"kind": "webhook_failed", "at": "2026-10-14T13:58:00Z", "production": true, "organization_name": "Globex"}},
	}
}

// `qube admin status --check` exits 1 when attention is set, 0 when it isn't, and the table
// marks the sections that need it.
func TestAdminStatusCheckExitCodes(t *testing.T) {
	attention := true
	var auth string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/status": func(w http.ResponseWriter, r *http.Request) {
			auth = r.Header.Get("Authorization")
			writeJSON(w, 200, map[string]interface{}{"data": statusSnapshot(attention), "meta": map[string]interface{}{}})
		},
	})
	actingAs(t, c, "read_write") // a grant never reaches /api/admin
	setJSON(t, false)
	var code int
	out := captureStdout(t, func() { code = c.adminStatus(c.staffClient(), []string{"--check"}) })
	if code != 1 || auth != "Bearer qct_staff" {
		t.Fatalf("attention: exit %d, auth %q", code, auth)
	}
	for _, want := range []string{"QuBe Sync · " + c.host + " · 3 nodes · release deployment-01JA2 · version 0.9.1 · 14:02:11 UTC",
		"  Web Connector  212 connections seen in the last 10 min · 37 SOAP sessions live",
		"  Queues         available  executing  retryable  scheduled",
		"    webhooks             4          3          1         12",
		"! Failed jobs    3 discarded in 24 h (2 Connections.WebhookWorker, 1 NameIndex.BuildWorker)",
		"! Webhooks       5 failed for good in 24 h, in 3 production apps (and 2 in sandbox apps)",
		"  Requests       no connection has had requests waiting over 1 h",
		"! Runs           1 running with no progress in 30 min · 0 steps swept by the stuck-run reaper in 1 h · run-9",
		"  Events         41 in 24 h · last: webhook_failed at "} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if code = c.adminStatus(c.staffClient(), nil); code != 0 {
		t.Fatalf("without --check: exit %d", code)
	}
	attention = false
	_ = captureStdout(t, func() { code = c.adminStatus(c.staffClient(), []string{"--check"}) })
	if code != 0 {
		t.Fatalf("no attention: exit %d", code)
	}
	setJSON(t, true)
	attention = true
	out = captureStdout(t, func() { code = c.adminStatus(c.staffClient(), []string{"--check"}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["attention"] != true || code != 1 {
		t.Fatalf("json: exit %d %v %s", code, err, out)
	}
}

// A section the server couldn't read shows as unavailable, and the rest still print.
func TestStatusTextUnavailableSection(t *testing.T) {
	d := statusSnapshot(false)
	d["queues"] = nil
	d["errors"] = map[string]interface{}{"queues": "timed out after 5000 ms"}
	out := statusText("https://qubesync.com", d)
	if !strings.Contains(out, "? Queues         unavailable: timed out after 5000 ms") || !strings.Contains(out, "Failed jobs") {
		t.Fatalf("%s", out)
	}
}

// The tail follows x-qube-cursor across a 204, and after a 503 reconnects from the same cursor.
func TestAdminTailFollowsTheCursor(t *testing.T) {
	fastListen(t)
	var mu sync.Mutex
	var seen []url.Values
	ctx, cancel := context.WithCancel(context.Background())
	event := func(id int, kind, org string) map[string]interface{} {
		return map[string]interface{}{"id": id, "kind": kind, "at": "2026-10-14T14:02:19Z", "production": true,
			"summary":    map[string]interface{}{"organization_name": org, "app_name": org + " Prod", "webhook_kind": "request", "host": "hooks.example"},
			"subject_id": "01JA00000000000000000000000"}
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/events": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.Query())
			n := len(seen)
			mu.Unlock()
			switch n {
			case 1:
				w.Header().Set("x-qube-cursor", "5")
				writeJSON(w, 200, map[string]interface{}{"data": []interface{}{event(5, "webhook_failed", "Globex")}, "meta": map[string]interface{}{"cursor": 5}})
			case 2:
				w.Header().Set("x-qube-cursor", "7") // events the filters dropped
				w.WriteHeader(204)
			case 3:
				writeJSON(w, 503, map[string]interface{}{"error": map[string]string{"code": "unavailable", "message": "The database didn't answer within 5 s."}})
			case 4:
				w.Header().Set("x-qube-cursor", "9")
				writeJSON(w, 200, map[string]interface{}{"data": []interface{}{event(9, "webhook_failed", "Initech")}, "meta": map[string]interface{}{"cursor": 9}})
			default:
				cancel()
				w.Header().Set("x-qube-cursor", "9")
				w.WriteHeader(204)
			}
		},
	})
	c.bg = ctx
	setJSON(t, false)
	var err error
	out := captureStdout(t, func() {
		err = c.adminTail(c.staffClient(), []string{"--kind", "webhook_failed", "--production", "--detail"})
	})
	if err != context.Canceled {
		t.Fatalf("err = %v", err)
	}
	var afters []string
	for _, q := range seen {
		afters = append(afters, q.Get("after"))
		if q.Get("wait") != "25" || q.Get("kind") != "webhook_failed" || q.Get("production") != "true" || q.Get("include") != "detail" {
			t.Errorf("query = %v", q)
		}
	}
	if strings.Join(afters, ",") != ",5,7,7,9" {
		t.Fatalf("afters = %q", afters)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "webhook_failed              Globex / Globex Prod · request 01JA0000… · to hooks.example · production") || !strings.Contains(lines[1], "Initech") {
		t.Fatalf("output:\n%s", out)
	}
}

// Under --json the tail prints each event as a line of JSON.
func TestAdminTailJSONLines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/events": func(w http.ResponseWriter, r *http.Request) {
			n++
			if n > 1 {
				cancel()
				w.WriteHeader(204)
				return
			}
			w.Header().Set("x-qube-cursor", "3")
			writeJSON(w, 200, map[string]interface{}{"data": []interface{}{
				map[string]interface{}{"id": 2, "kind": "act_as.started", "summary": map[string]interface{}{"actor_email": "garrett@qubesync.com"}},
				map[string]interface{}{"id": 3, "kind": "agents.flag", "summary": map[string]interface{}{"enabled": false}},
			}})
		},
	})
	c.bg = ctx
	setJSON(t, true)
	out := captureStdout(t, func() { _ = c.adminTail(c.staffClient(), nil) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var first map[string]interface{}
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &first) != nil || first["kind"] != "act_as.started" {
		t.Fatalf("output:\n%s", out)
	}
}

// Ctrl-C ends `qube admin tail` with exit 130.
func TestAdminTailExits130OnInterrupt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no SIGINT to send on Windows")
	}
	if os.Getenv("QUBE_TEST_CHILD") == t.Name() {
		cfg, _ := config.Load()
		bg, stop := signalContext()
		defer stop()
		c := &ctx{host: os.Getenv("QUBE_TEST_HOST"), cfg: cfg, bg: bg}
		c.admin([]string{"tail"})
		os.Exit(0)
	}
	polled := make(chan struct{}, 10)
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/events": func(w http.ResponseWriter, r *http.Request) {
			polled <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			w.WriteHeader(204)
		},
	})
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(), "QUBE_TEST_CHILD="+t.Name(), "QUBE_TEST_HOST="+c.host)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-polled:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("the tail never polled")
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != ui.ExitInterrupted {
		t.Fatalf("want exit %d, got %v", ui.ExitInterrupted, err)
	}
}

// Customer data is asked for only with its flag, as include=.
func TestAdminCustomerDataFlags(t *testing.T) {
	got := map[string]url.Values{}
	record := func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Path] = r.URL.Query()
		writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"id": "x"}, "meta": map[string]interface{}{}})
	}
	list := func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Path] = r.URL.Query()
		writeJSON(w, 200, map[string]interface{}{"data": []interface{}{}, "meta": map[string]interface{}{}})
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/requests/r1": record, "/api/admin/runs/run1": record, "/api/admin/jobs": list, "/api/admin/events": list,
	})
	setJSON(t, true)
	cl := c.staffClient()
	_ = captureStdout(t, func() {
		c.adminRequest(cl, []string{"r1"})
		c.adminRun(cl, []string{"run1"})
	})
	if got["/api/admin/requests/r1"].Get("include") != "" || got["/api/admin/runs/run1"].Get("include") != "" {
		t.Fatalf("include sent without a flag: %v", got)
	}
	_ = captureStdout(t, func() {
		c.adminRequest(cl, []string{"r1", "--xml"})
		c.adminRun(cl, []string{"run1", "--events", "--io"})
		c.adminJobs(cl, []string{"--state", "discarded", "--args"})
		c.adminEvents(cl, []string{"--detail", "--kind", "webhook_failed"})
	})
	if got["/api/admin/requests/r1"].Get("include") != "xml" || got["/api/admin/runs/run1"].Get("include") != "events,io" ||
		got["/api/admin/jobs"].Get("include") != "args" || got["/api/admin/jobs"].Get("state") != "discarded" ||
		got["/api/admin/events"].Get("include") != "detail" || got["/api/admin/events"].Get("since") != "24h" {
		t.Fatalf("queries: %v", got)
	}
}

// `qube admin requests` needs a connection, app or organization; --connection and --app are
// the global flags, passed as the route's filters.
func TestAdminRequestsFilters(t *testing.T) {
	if inChild(t, func(c *ctx) { c.admin([]string{"requests", "--state", "error"}) }) {
		return
	}
	var q url.Values
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/requests": func(w http.ResponseWriter, r *http.Request) {
			q = r.URL.Query()
			writeJSON(w, 200, map[string]interface{}{"data": []map[string]interface{}{
				{"id": "req-1", "state": "error", "webhook_state": "failed", "request_types": []string{"CustomerAddRq"},
					"error": map[string]interface{}{"type": "quickbooks_error", "code": 3100}, "connection_id": "conn-1", "created_at": "2026-10-14T13:00:00Z"},
			}, "meta": map[string]interface{}{"next_cursor": "MTIz"}})
		},
	})
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, "one of --connection, --app or --org is required") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	setJSON(t, false)
	c.connection = "conn-1"
	out = captureStdout(t, func() { c.adminRequests(c.staffClient(), []string{"--state", "error", "--since", "1h"}) })
	if q.Get("connection") != "conn-1" || q.Get("state") != "error" || q.Get("since") != "1h" {
		t.Fatalf("query = %v", q)
	}
	if !strings.Contains(out, "req-1") || !strings.Contains(out, "quickbooks_error 3100") {
		t.Fatalf("output:\n%s", out)
	}
	setJSON(t, true)
	out = captureStdout(t, func() { c.adminRequests(c.staffClient(), nil) })
	var got struct {
		Data []map[string]interface{} `json:"data"`
		Meta map[string]interface{}   `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Data) != 1 || got.Meta["next_cursor"] != "MTIz" {
		t.Fatalf("json: %v %s", err, out)
	}
}

// Grants and the kill switch send what the routes take.
func TestAdminGrantsAndAgents(t *testing.T) {
	var calls []string
	var bodies []map[string]interface{}
	record := func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		var b map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/grants": func(w http.ResponseWriter, r *http.Request) {
			record(w, r)
			writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{
				"active": []map[string]interface{}{{"id": "grant-1", "actor": "agent", "agent_name": "Claude Code", "user": map[string]string{"email": "garrett@qubesync.com"},
					"organization": map[string]string{"name": "Acme Corp"}, "scope": "all", "access": "configure", "write_budget": 25, "expires_at": "2026-10-14T15:10:00Z", "reason": "Install invoice sync"}},
				"pending": []map[string]interface{}{{"user_code": "W2HT-9RNC", "organization": map[string]string{"name": "Globex"},
					"requested_by": map[string]string{"email": "garrett@qubesync.com", "actor": "human"}, "scope": "all", "access": "configure", "minutes": 30, "reason": "Publish"}},
			}})
		},
		"/api/admin/grants/grant-1": func(w http.ResponseWriter, r *http.Request) {
			record(w, r)
			writeJSON(w, 200, map[string]interface{}{"data": map[string]interface{}{"id": "grant-1", "organization": map[string]string{"name": "Acme Corp"}}})
		},
		"/api/admin/agents/revoke": func(w http.ResponseWriter, r *http.Request) {
			record(w, r)
			writeJSON(w, 200, map[string]interface{}{"data": map[string]int{"revoked": 3}})
		},
		"/api/admin/agents/access": func(w http.ResponseWriter, r *http.Request) {
			record(w, r)
			writeJSON(w, 200, map[string]interface{}{"data": map[string]bool{"enabled": false}})
		},
	})
	setJSON(t, false)
	cl := c.staffClient()
	out := captureStdout(t, func() {
		c.adminGrants(cl, nil)
		c.adminGrants(cl, []string{"revoke", "grant-1"})
		c.adminAgents(cl, []string{"revoke", "--all"})
		c.adminAgents(cl, []string{"revoke", "sess-9"})
		c.adminAgents(cl, []string{"disable"})
	})
	want := []string{"GET /api/admin/grants", "DELETE /api/admin/grants/grant-1", "POST /api/admin/agents/revoke", "POST /api/admin/agents/revoke", "PUT /api/admin/agents/access"}
	if strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v", calls)
	}
	if !reflect.DeepEqual(bodies[2], map[string]interface{}{"all": true, "staff": false}) || !reflect.DeepEqual(bodies[3], map[string]interface{}{"id": "sess-9"}) ||
		!reflect.DeepEqual(bodies[4], map[string]interface{}{"enabled": false}) {
		t.Fatalf("bodies = %v", bodies)
	}
	for _, w := range []string{`garrett@qubesync.com (agent "Claude Code")`, "W2HT-9RNC", "Revoked grant grant-1 (Acme Corp).", "Revoked 3 sessions and grants.", "Agent staff access is off"} {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

// The organizations list prints the server's answer under --json, and a table otherwise.
func TestAdminOrgsShapes(t *testing.T) {
	var q url.Values
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/organizations": func(w http.ResponseWriter, r *http.Request) {
			q = r.URL.Query()
			writeJSON(w, 200, map[string]interface{}{"data": []map[string]interface{}{{"id": acmeID, "name": "Acme Corp",
				"owners": []map[string]string{{"email": "owner@acme.example"}}, "sandbox_status": "active", "production_connection_count": 2}},
				"meta": map[string]interface{}{"page": 1, "page_size": 20, "total_count": 1, "total_pages": 1, "has_next_page": false}})
		},
	})
	setJSON(t, true)
	out := captureStdout(t, func() { c.adminOrgs(c.staffClient(), []string{"--search", "acme"}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["data"] == nil || got["meta"] == nil || q.Get("search") != "acme" {
		t.Fatalf("json: %v %s %v", err, out, q)
	}
	setJSON(t, false)
	out = captureStdout(t, func() { c.adminOrgs(c.staffClient(), nil) })
	if !strings.Contains(out, "owner@acme.example") || !strings.Contains(out, "PROD CONNECTIONS") {
		t.Fatalf("table:\n%s", out)
	}
}

// A stalled connection shows when it stalled and why on one row; one that isn't stalled (both
// fields null) has no such row. --json passes the fields through.
func TestAdminConnectionShowsTheStall(t *testing.T) {
	answer := map[string]interface{}{"id": "conn_1", "name": "Acme", "type": "standard", "stalled_at": nil, "stall_reason": nil,
		"organization": map[string]interface{}{"name": "Acme Corp"}, "app": map[string]interface{}{"name": "Acme App", "sandbox": false}}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/connections/conn_1": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]interface{}{"data": answer})
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminConnection(c.staffClient(), []string{"conn_1"}) })
	if strings.Contains(out, "stalled") || !strings.Contains(out, "Acme Corp / Acme App") {
		t.Fatalf("not stalled:\n%s", out)
	}

	answer["stalled_at"], answer["stall_reason"] = "2026-10-22T14:05:00Z", "not_checking_in"
	out = captureStdout(t, func() { c.adminConnection(c.staffClient(), []string{"conn_1"}) })
	if !regexp.MustCompile(`(?m)^stalled +since 2026-10-22T14:05:00Z \(not_checking_in\)$`).MatchString(out) {
		t.Fatalf("stalled:\n%s", out)
	}

	answer["stall_reason"] = "quickbooks_unavailable"
	setJSON(t, true)
	out = captureStdout(t, func() { c.adminConnection(c.staffClient(), []string{"conn_1"}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["stall_reason"] != "quickbooks_unavailable" || got["stalled_at"] != "2026-10-22T14:05:00Z" {
		t.Fatalf("--json: %v %s", err, out)
	}
	if got := stalledText(map[string]interface{}{"stalled_at": "2026-10-22T14:05:00Z"}); got != "since 2026-10-22T14:05:00Z" {
		t.Errorf("without a reason: %q", got)
	}
	if got := stalledText(map[string]interface{}{"stalled_at": nil, "stall_reason": "not_checking_in"}); got != "not_checking_in" {
		t.Errorf("without a time: %q", got)
	}
}

// Every command main dispatches, and every admin subcommand, is one completion knows: read
// from the switches themselves, so a new command can't be left out.
func TestCompletionKnowsEveryCommand(t *testing.T) {
	aliases := map[string]bool{"quickbooks": true, "organizations": true, "organization": true}
	for _, tc := range []struct{ file, fn, tree string }{{"main.go", "main", ""}, {"admin.go", "admin", "admin"}} {
		cases := switchCases(t, tc.file, tc.fn)
		if len(cases) < 5 {
			t.Fatalf("%s: found only %v", tc.file, cases)
		}
		for _, c := range cases {
			if !aliases[c] && !contains(commandTree[tc.tree], c) {
				t.Errorf("%s dispatches %q, which completion's commandTree[%q] lacks", tc.file, c, tc.tree)
			}
		}
	}
	for _, f := range []string{"--reason", "--for", "--production", "--access", "--write-budget", "--wait", "--no-wait", "--resume", "--end", "--status"} {
		if !contains(commandTree["act-as"], f) {
			t.Errorf("completion lacks act-as %s", f)
		}
	}
	for _, f := range []string{"--admin", "--agent"} {
		if !contains(commandTree["login"], f) {
			t.Errorf("completion lacks login %s", f)
		}
	}
}

// switchCases is every string literal a `case` of a switch in function fn of file names.
func switchCases(t *testing.T, file, fn string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range f.Decls {
		fd, ok := d.(*ast.FuncDecl)
		if !ok || fd.Name.Name != fn {
			continue
		}
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok || sw.Tag == nil {
				return true
			}
			for _, st := range sw.Body.List {
				for _, e := range st.(*ast.CaseClause).List {
					if s, ok := stringLit(e); ok {
						out = append(out, s)
					}
				}
			}
			return false
		})
	}
	return out
}

// `qube help` points at the staff commands only for a session that holds staff permissions;
// `qube help staff` lists them.
func TestHelpPointsAtStaffCommandsOnlyForStaff(t *testing.T) {
	withDeviceDir(t)
	c := newAPITest(t, nil)
	withConfig(t, c)
	setenv(t, "QUBE_HOST", c.host)
	setenv(t, "QUBE_CACHE_DIR", t.TempDir())
	out := captureStdout(t, usage)
	if strings.Contains(out, "help staff") || strings.Contains(out, "act-as") || strings.Contains(out, "admin status") {
		t.Fatalf("an ordinary session's help mentions staff commands:\n%s", out)
	}
	s := c.cfg.Sessions[c.host]
	s.Permissions = api.StaffPermissions
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if out = captureStdout(t, usage); !strings.Contains(out, "QuBe Sync staff: `qube help staff` lists the admin and act-as commands.") {
		t.Fatalf("a staff session's help:\n%s", out)
	}
	out = captureStdout(t, staffUsage)
	for _, want := range []string{"login --admin", "admin status [--check]", "admin tail", "act-as ORG --reason R", "act-as --end", "admin agents disable | enable", "whatever --yes says"} {
		if !strings.Contains(out, want) {
			t.Errorf("staff help lacks %q", want)
		}
	}
}

// A cursor of 0 (no event yet) goes back as after=0 rather than falling back to a time.
func TestAdminTailPassesACursorOfZero(t *testing.T) {
	fastListen(t)
	var mu sync.Mutex
	var seen []url.Values
	ctx, cancel := context.WithCancel(context.Background())
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/events": func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, r.URL.Query())
			n := len(seen)
			mu.Unlock()
			if n >= 2 {
				cancel()
			}
			w.Header().Set("x-qube-cursor", "0")
			w.WriteHeader(204)
		},
	})
	c.bg = ctx
	setJSON(t, false)
	_ = captureStdout(t, func() { _ = c.adminTail(c.staffClient(), nil) })
	if len(seen) < 2 || seen[0].Get("after") != "" || seen[1].Get("after") != "0" || seen[1].Get("since") != "" {
		t.Fatalf("queries = %v", seen)
	}
}
