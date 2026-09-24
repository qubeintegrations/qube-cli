package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type seen struct {
	auth, ua, ct, path string
}

func server(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *seen) {
	t.Helper()
	s := &seen{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.auth, s.ua, s.ct, s.path = r.Header.Get("Authorization"), r.Header.Get("User-Agent"), r.Header.Get("Content-Type"), r.URL.Path
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, s
}

func ok(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestSessionTokenOnlyGoesToCLIEndpoints(t *testing.T) {
	srv, s := server(t, func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]string{"x": "y"}) })
	c := New(srv.URL, 0)
	c.Token = "qct_secret"
	c.APIKey = "sk_key"

	if err := c.Do("GET", "/api/cli/me", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if s.auth != "Bearer qct_secret" {
		t.Fatalf("cli endpoint auth = %q", s.auth)
	}
	if !strings.HasPrefix(s.ua, "qube-cli/") {
		t.Fatalf("user agent = %q", s.ua)
	}

	if err := c.Do("GET", "/api/v1/connections", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.auth, "Basic ") || strings.Contains(s.auth, "qct_") {
		t.Fatalf("api endpoint auth = %q", s.auth)
	}
}

func TestDeviceFlowNeedsNoTokenButOtherCLIEndpointsDo(t *testing.T) {
	srv, s := server(t, func(w http.ResponseWriter, r *http.Request) { ok(w, map[string]string{}) })
	c := New(srv.URL, 0)
	if err := c.Do("POST", "/api/cli/device", nil, map[string]string{"client_name": "t"}, nil); err != nil {
		t.Fatal(err)
	}
	if s.auth != "" || s.ct != "application/json" {
		t.Fatalf("device call auth=%q content-type=%q", s.auth, s.ct)
	}
	err := c.Do("GET", "/api/cli/apps", nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "qube login") {
		t.Fatalf("expected a not-logged-in error, got %v", err)
	}
}

func TestErrorEnvelopes(t *testing.T) {
	cases := []struct {
		name, body, path string
		status           int
		wantCode, want   string
	}{
		{"cli envelope", `{"error":{"code":"access_denied","message":"the login was denied"}}`, "/api/cli/token", 403, "access_denied", "the login was denied"},
		{"phoenix detail", `{"errors":{"detail":"Not Found"}}`, "/api/v1/connections/x", 404, "", "Not Found"},
		{"changeset", `{"errors":{"type":["is invalid"]}}`, "/api/v1/connections", 422, "", "type [is invalid]"},
		{"expired session", `{"error":{"code":"cli_token_expired","message":"expired"}}`, "/api/cli/me", 401, "cli_token_expired", "run `qube login` again"},
		{"revoked session", `{"error":{"code":"unauthorized","message":"nope"}}`, "/api/cli/apps", 401, "unauthorized", "no longer valid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			c := New(srv.URL, 0)
			c.Token = "qct_x"
			err := c.Do("GET", tc.path, nil, nil, nil)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("expected *Error, got %v", err)
			}
			if e.Status != tc.status || e.Code != tc.wantCode || !strings.Contains(e.Message, tc.want) {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

func TestPollToken(t *testing.T) {
	calls := 0
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"authorization_pending","message":"waiting"}}`))
			return
		}
		ok(w, map[string]interface{}{"token": "qct_new", "scope": "sandbox", "user": map[string]string{"email": "a@b.c"}})
	})
	c := New(srv.URL, 0)
	if _, err := c.PollToken("dc"); err != ErrPending {
		t.Fatalf("first poll: %v", err)
	}
	grant, err := c.PollToken("dc")
	if err != nil || grant.Token != "qct_new" || grant.User.Email != "a@b.c" {
		t.Fatalf("second poll: %v %+v", err, grant)
	}
}

func TestRetryable(t *testing.T) {
	if !Retryable(&Error{Status: 503}) || !Retryable(&Error{Status: 429}) {
		t.Fatal("5xx and 429 are retryable")
	}
	if Retryable(&Error{Status: 401}) || Retryable(&Error{Status: 422}) || Retryable(nil) {
		t.Fatal("4xx is not retryable")
	}
	c := New("http://127.0.0.1:1", 0) // nothing listens there
	err := c.Do("GET", "/api/v1/connections", nil, nil, nil)
	if !Retryable(err) {
		t.Fatalf("a refused connection is retryable, got %v", err)
	}
	if Retryable(context.Canceled) {
		t.Fatal("an interrupt is not retryable")
	}
}

func TestRetryAfterHeader(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"errors":{"detail":"Too many requests"}}`))
	})
	err := New(srv.URL, 0).Do("POST", "/api/cli/device", nil, map[string]string{}, nil)
	var e *Error
	if !errors.As(err, &e) || e.RetryAfter != 7 || e.Status != 429 {
		t.Fatalf("got %v", err)
	}
}

func TestCancelledContextIsReportedAsSuch(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		ok(w, map[string]string{})
	})
	ctx, cancel := context.WithCancel(context.Background())
	c := New(srv.URL, 0)
	c.Ctx = ctx
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	err := c.Do("GET", "/api/v1/connections", nil, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestTimeoutIsApplied(t *testing.T) {
	c := New("https://example.invalid", 3*time.Second)
	if c.HTTP.Timeout != 3*time.Second {
		t.Fatalf("timeout = %v", c.HTTP.Timeout)
	}
	if New("x", 0).HTTP.Timeout != DefaultTimeout {
		t.Fatal("zero means the default")
	}
}

func TestResolveApp(t *testing.T) {
	apps := []App{{ID: "1", Name: "My App Dev"}, {ID: "2", Name: "My App"}, {ID: "3", Name: "Other"}}
	for ref, want := range map[string]string{"1": "1", "my app": "2", "oth": "3", "My App Dev": "1"} {
		a, err := ResolveApp(apps, ref)
		if err != nil || a.ID != want {
			t.Fatalf("%q -> %v %v", ref, a, err)
		}
	}
	if _, err := ResolveApp(apps, "my"); err == nil || !strings.Contains(err.Error(), "several") {
		t.Fatalf("ambiguous prefix: %v", err)
	}
	if _, err := ResolveApp(apps, ""); err == nil {
		t.Fatal("no ref with several apps must fail")
	}
	if a, err := ResolveApp(apps[:1], ""); err != nil || a.ID != "1" {
		t.Fatal("no ref with one app picks it")
	}
	if _, err := ResolveApp(nil, ""); err == nil {
		t.Fatal("no apps at all is an error")
	}
}
