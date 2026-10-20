package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// While a grant is held, its token goes everywhere but the staff routes: this table is the
// one place a mistake would send the staff token where the grant's belongs, or the reverse.
func TestActAsTokenRouting(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	apps := map[string]string{}
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Method+" "+r.URL.Path] = r.Header.Get("Authorization")
		apps[r.Method+" "+r.URL.Path] = r.Header.Get("X-Qube-App")
		mu.Unlock()
		ok(w, map[string]string{})
	})
	c := New(srv.URL, 0)
	c.Token, c.ActAsToken, c.AppID = "qct_staff", "qct_grant", "app-1"
	routes := []struct{ method, path, want string }{
		{"GET", "/api/admin/status", "Bearer qct_staff"},
		{"GET", "/api/admin/events", "Bearer qct_staff"},
		{"DELETE", "/api/admin/grants/g1", "Bearer qct_staff"},
		{"POST", "/api/admin/agents/revoke", "Bearer qct_staff"},
		{"POST", "/api/cli/act-as", "Bearer qct_staff"},
		{"GET", "/api/cli/me", "Bearer qct_grant"},
		{"GET", "/api/cli/apps", "Bearer qct_grant"},
		{"GET", "/api/cli/sessions", "Bearer qct_grant"},
		{"DELETE", "/api/cli/session", "Bearer qct_grant"},
		{"GET", "/api/cli/apps/app-1/relays", "Bearer qct_grant"},
		{"GET", "/api/v2/connections", "Bearer qct_grant"},
		{"POST", "/api/v2/workflows", "Bearer qct_grant"},
		{"GET", "/api/v1/connections", "Bearer qct_grant"},
		{"POST", "/api/cli/device", ""},
		{"POST", "/api/cli/token", ""},
		{"GET", "/api/v2/openapi.json", ""},
	}
	for _, r := range routes {
		if err := c.Do(r.method, r.path, nil, nil, nil); err != nil {
			t.Fatalf("%s %s: %v", r.method, r.path, err)
		}
		if a := got[r.method+" "+r.path]; a != r.want {
			t.Errorf("%s %s carried %q, want %q", r.method, r.path, a, r.want)
		}
	}
	for _, p := range []string{"GET /api/v2/connections", "GET /api/v1/connections"} {
		if apps[p] != "app-1" {
			t.Errorf("%s: X-Qube-App = %q", p, apps[p])
		}
	}
	for _, p := range []string{"GET /api/admin/status", "POST /api/cli/act-as", "GET /api/cli/me"} {
		if apps[p] != "" {
			t.Errorf("%s sent an app: %q", p, apps[p])
		}
	}

	// without a grant, everything authenticated carries the session's own token
	c.ActAsToken = ""
	for _, p := range []string{"/api/cli/me", "/api/v2/connections", "/api/admin/status"} {
		if err := c.Do("GET", p, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if a := got["GET "+p]; a != "Bearer qct_staff" {
			t.Errorf("%s carried %q without a grant", p, a)
		}
	}
}

func TestStaffOnly(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/admin": true, "/api/admin/status": true, "/api/admin/grants/x": true,
		"/api/cli/act-as": true, "/api/cli/act-as/x": true,
		"/api/administrator": false, "/api/cli/act-asx": false, "/api/cli/me": false, "/api/v2/admin": false,
	} {
		if StaffOnly(path) != want {
			t.Errorf("StaffOnly(%q) = %v", path, !want)
		}
	}
}

// A 401 to a request that carried the grant tells the hook (which drops the grant) and is
// marked as the grant's; one that carried the staff token is not.
func TestActAsRefusedHook(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		if strings.HasPrefix(r.URL.Path, "/api/v2") {
			_, _ = w.Write([]byte(`{"errors":{"detail":"This CLI session has expired or was revoked.","code":"cli_token_invalid"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"code":"cli_token_expired","message":"expired"}}`))
	})
	c := New(srv.URL, 0)
	c.Token, c.ActAsToken, c.AppID = "qct_staff", "qct_grant", "app-1"
	calls := 0
	c.OnActAsRefused = func(e *Error) {
		calls++
		e.Message = "the grant ended"
	}
	err := c.Do("GET", "/api/v2/connections", nil, nil, nil)
	var e *Error
	if !errors.As(err, &e) || !e.ActAs || e.Message != "the grant ended" || calls != 1 {
		t.Fatalf("v2: err = %#v, calls = %d", err, calls)
	}
	if _, status, err := c.Raw("GET", "/api/v2/connections", nil, nil); err != nil || status != 401 || calls != 2 {
		t.Fatalf("raw: status %d err %v calls %d", status, err, calls)
	}
	err = c.Do("GET", "/api/admin/status", nil, nil, nil)
	if !errors.As(err, &e) || e.ActAs || calls != 2 || !strings.Contains(e.Message, "qube login --admin") {
		t.Fatalf("admin: err = %#v, calls = %d", err, calls)
	}
}

// Each refusal of the staff features reads as what happened and what to do next.
func TestStaffRefusalMessages(t *testing.T) {
	cases := []struct {
		code, body string
		status     int
		want       []string
	}{
		{"act_as_access", `{"errors":{"detail":"This act-as grant is read-only: it can look but not change anything.","code":"act_as_access"}}`, 403,
			[]string{"HTTP 403 act_as_access", "is read-only", "qube act-as --end"}},
		{"write_budget_spent", `{"errors":{"detail":"This act-as grant has used its write budget.","code":"write_budget_spent"}}`, 403,
			[]string{"used its write budget", "qube act-as --end", "new grant"}},
		{"rate_limited", `{"error":{"code":"rate_limited","message":"This staff session is over its rate limit (a guard against loops). Retry in 30 s."}}`, 429,
			[]string{"rate limit", "Retry in 30 s"}},
		{"audit_unavailable", `{"errors":{"detail":"The staff audit log can't be written, so this request was refused.","code":"audit_unavailable"}}`, 503,
			[]string{"audit log", "Nothing was done"}},
		{"permission_required", `{"error":{"code":"permission_required","message":"This session doesn't hold admin.read_audit. Log in with ` + "`qube login --admin`" + `."}}`, 403,
			[]string{"admin.read_audit", "qube login --admin"}},
		{"staff_access_disabled", `{"error":{"code":"staff_access_disabled","message":"Staff sessions are switched off on this server (:cli_staff_access)."}}`, 403,
			[]string{"switched off", ":cli_staff_access", "/admin/feature-flags"}},
		{"act_as_no_credentials", `{"error":{"code":"act_as_no_credentials","message":"An act-as grant can't read an app's API key."}}`, 403,
			[]string{"`qube env` doesn't work while acting", "qube act-as --end"}},
		{"bare permission_required", `{"error":{"code":"permission_required"}}`, 403,
			[]string{"doesn't hold the staff permission"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Retry-After", "30")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := New(srv.URL, 0)
			c.Token, c.AppID = "qct_x", "app-1"
			err := c.Do("POST", "/api/v2/workflows", nil, map[string]string{}, nil)
			if err == nil {
				t.Fatal("no error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("%q lacks %q", err.Error(), w)
				}
			}
		})
	}
}

// A read refused with 429 is sent once more after its Retry-After; a write isn't, and a
// second 429 is the answer.
func TestRateLimitedReadIsRetriedOnce(t *testing.T) {
	old := retryAfterUnit
	retryAfterUnit = time.Millisecond
	t.Cleanup(func() { retryAfterUnit = old })
	var mu sync.Mutex
	calls := map[string]int{}
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls[r.Method]++
		n := calls[r.Method]
		mu.Unlock()
		if r.URL.Query().Get("always") == "" && n > 1 {
			ok(w, map[string]string{"x": "y"})
			return
		}
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limited","message":"over"}}`))
	})
	c := New(srv.URL, 0)
	c.Token = "qct_x"
	if err := c.Do("GET", "/api/admin/status", nil, nil, nil); err != nil || calls["GET"] != 2 {
		t.Fatalf("read: err %v, calls %d", err, calls["GET"])
	}
	if err := c.Do("POST", "/api/admin/agents/revoke", nil, map[string]bool{"all": true}, nil); err == nil || calls["POST"] != 1 {
		t.Fatalf("write: err %v, calls %d", err, calls["POST"])
	}
	mu.Lock()
	calls["GET"] = 0
	mu.Unlock()
	err := c.Do("GET", "/api/admin/status", url.Values{"always": {"1"}}, nil, nil)
	var e *Error
	if !errors.As(err, &e) || e.Code != "rate_limited" || calls["GET"] != 2 {
		t.Fatalf("read twice refused: err %v, calls %d", err, calls["GET"])
	}
	mu.Lock()
	calls["GET"] = 0
	mu.Unlock()
	if _, status, err := c.Raw("GET", "/api/admin/status", nil, nil); err != nil || status != 200 || calls["GET"] != 2 {
		t.Fatalf("raw read: status %d err %v calls %d", status, err, calls["GET"])
	}
}

// What each `qube login` asks for: an ordinary login sends what it always did.
func TestStartDevicePayloads(t *testing.T) {
	var body map[string]interface{}
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		ok(w, map[string]interface{}{"device_code": "dc", "user_code": "ABCD-EFGH", "expires_in": 600, "interval": 5})
	})
	c := New(srv.URL, 0)
	cases := []struct {
		req  DeviceRequest
		want map[string]interface{}
	}{
		{DeviceRequest{ClientName: "n", Access: "read_write"},
			map[string]interface{}{"client_name": "n", "access": "read_write"}},
		{DeviceRequest{ClientName: "n", Access: "read_write", Permissions: StaffPermissions},
			map[string]interface{}{"client_name": "n", "access": "read_write", "permissions": []interface{}{"admin.read", "admin.read_customer_data", "admin.read_audit", "org.act", "org.act_production", "agents.manage", "builds.manage"}}},
		{DeviceRequest{ClientName: "n", Access: "read_write", AgentName: "Claude Code"},
			map[string]interface{}{"client_name": "n", "access": "read_write", "actor": "agent", "agent_name": "Claude Code"}},
		{DeviceRequest{ClientName: "n", Access: "read_only", AgentName: "Claude Code", Permissions: AgentPermissions},
			map[string]interface{}{"client_name": "n", "access": "read_only", "actor": "agent", "agent_name": "Claude Code", "permissions": []interface{}{"admin.read", "admin.read_customer_data", "org.act", "org.act_production", "builds.manage"}}},
	}
	for _, tc := range cases {
		if _, err := c.StartDevice(tc.req); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(body, tc.want) {
			t.Errorf("sent %v, want %v", body, tc.want)
		}
	}
}

// The event stream's cursor comes from x-qube-cursor, on a 204 too.
func TestAdminEventsCursor(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("wait") != "" {
			w.Header().Set("x-qube-cursor", "42")
			w.WriteHeader(204)
			return
		}
		w.Header().Set("x-qube-cursor", "7")
		ok(w, map[string]interface{}{"data": []map[string]interface{}{{"id": 7, "kind": "webhook_failed"}}, "meta": map[string]interface{}{"cursor": 7}})
	})
	c := New(srv.URL, 0)
	c.Token, c.ActAsToken = "qct_staff", "qct_grant"
	page, err := c.AdminEvents(context.Background(), url.Values{"wait": {"25"}})
	if err != nil || page.Status != 204 || page.Cursor != "42" || len(page.Events) != 0 {
		t.Fatalf("204: %+v %v", page, err)
	}
	page, err = c.AdminEvents(context.Background(), url.Values{"since": {"24h"}})
	if err != nil || page.Status != 200 || page.Cursor != "7" || len(page.Events) != 1 {
		t.Fatalf("200: %+v %v", page, err)
	}
}
