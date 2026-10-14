package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A poll asks for gzip without setting Accept-Encoding itself, so Go undoes it; it is held
// longer than the client's Timeout, which polls don't use; and a 204 is no deliveries.
func TestPollDeliveries(t *testing.T) {
	var acceptEncoding, session, path string
	polls := 0
	srv, s := server(t, func(w http.ResponseWriter, r *http.Request) {
		polls++
		acceptEncoding, session, path = r.Header.Get("Accept-Encoding"), r.URL.Query().Get("session"), r.URL.Path
		if polls == 1 {
			w.WriteHeader(204)
			return
		}
		time.Sleep(150 * time.Millisecond) // past the client's 50 ms Timeout
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_ = json.NewEncoder(gz).Encode(map[string]interface{}{"deliveries": []map[string]interface{}{{
			"attempt_id": "att_1",
			"path":       "/runs",
			"headers":    map[string]string{"x-qube-signature": "t=1,v1=abc"},
			"body":       `{"a":"café"}`,
			"summary":    map[string]interface{}{"kind": "request", "id": "r1"},
		}}})
		_ = gz.Close()
	})
	c := New(srv.URL, 50*time.Millisecond)
	c.Token = "qct_x"

	got, err := c.PollDeliveries(context.Background(), "app 1", "rl_x", "sess-1")
	if err != nil || got != nil {
		t.Fatalf("204: %v, %v", got, err)
	}
	got, err = c.PollDeliveries(context.Background(), "app 1", "rl_x", "sess-1")
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if d := got[0]; d.AttemptID != "att_1" || d.Path != "/runs" || d.Body != `{"a":"café"}` || d.Headers["x-qube-signature"] != "t=1,v1=abc" || d.Summary["kind"] != "request" {
		t.Fatalf("delivery = %+v", d)
	}
	if acceptEncoding != "gzip" || session != "sess-1" || path != "/api/cli/apps/app 1/relays/rl_x/deliveries" || s.auth != "Bearer qct_x" {
		t.Fatalf("accept-encoding %q, session %q, path %q, auth %q", acceptEncoding, session, path, s.auth)
	}
}

// A poll's own deadline ends it, as a retryable error.
func TestPollDeliveriesDeadline(t *testing.T) {
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	c := New(srv.URL, 0)
	c.Token = "qct_x"
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.PollDeliveries(ctx, "app1", "rl_x", "s")
	if !errors.Is(err, context.DeadlineExceeded) || !Retryable(err) {
		t.Fatalf("err = %v", err)
	}
}

// A takeover is a 409 relay_taken_over *Error; a 401 gets the `qube login` wording.
func TestPollDeliveriesErrors(t *testing.T) {
	status := 409
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		code := map[int]string{409: "relay_taken_over", 401: "cli_token_invalid"}[status]
		ok(w, map[string]interface{}{"error": map[string]string{"code": code, "message": "x"}})
	})
	c := New(srv.URL, 0)
	c.Token = "qct_x"
	var e *Error
	if _, err := c.PollDeliveries(context.Background(), "app1", "rl_x", "s"); !errors.As(err, &e) || e.Code != "relay_taken_over" || Retryable(err) {
		t.Fatalf("409: %v", err)
	}
	status = 401
	if _, err := c.PollDeliveries(context.Background(), "app1", "rl_x", "s"); err == nil || !strings.Contains(err.Error(), "run `qube login` again") {
		t.Fatalf("401: %v", err)
	}
}

// The other relay calls: their methods, paths and bodies.
func TestRelayCalls(t *testing.T) {
	type call struct{ method, path, query, body string }
	var calls []call
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		calls = append(calls, call{r.Method, r.URL.Path, r.URL.RawQuery, string(body)})
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/relays") {
			ok(w, map[string]interface{}{
				"relay": map[string]string{"id": "rl_x", "url": "https://q.example/relay/rl_x", "app_id": "app1"},
			})
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/relays") {
			ok(w, map[string]interface{}{
				"relay":    map[string]string{"id": "rl_x", "url": "https://q.example/relay/rl_x", "app_id": "app1"},
				"replaced": map[string]interface{}{"id": "rl_old", "url": "https://q.example/relay/rl_old", "last_polled_at": nil},
			})
			return
		}
		w.WriteHeader(204)
	})
	c := New(srv.URL, 0)
	c.Token = "qct_x"
	relay, err := c.FindRelay("app1", "4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5b")
	if err != nil || relay == nil || relay.ID != "rl_x" || relay.URL != "https://q.example/relay/rl_x" || relay.AppID != "app1" {
		t.Fatalf("relay = %+v, %v", relay, err)
	}
	grant, err := c.CreateRelay("app1", "4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5b")
	if err != nil || grant.Relay.ID != "rl_x" || grant.Relay.URL != "https://q.example/relay/rl_x" || grant.Replaced == nil || grant.Replaced.LastPolledAt != "" {
		t.Fatalf("grant = %+v, %v", grant, err)
	}
	if err := c.ReportDelivery(context.Background(), "app1", "rl_x", "att_1", DeliveryResult{Status: 200, Body: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := c.StopListening(context.Background(), "app1", "rl_x", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := c.ForgetRelay("app1", "rl_x"); err != nil {
		t.Fatal(err)
	}
	want := []call{
		{"GET", "/api/cli/apps/app1/relays", "device_id=4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5b", ""},
		{"POST", "/api/cli/apps/app1/relays", "", `{"device_id":"4f1c2a3b-5d6e-4f70-8a9b-0c1d2e3f4a5b"}`},
		{"POST", "/api/cli/apps/app1/relays/rl_x/deliveries/att_1", "", `{"body":"ok","status":200}`},
		{"DELETE", "/api/cli/apps/app1/relays/rl_x/listen", "session=sess-1", ""},
		{"DELETE", "/api/cli/apps/app1/relays/rl_x", "", ""},
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %+v", calls)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Errorf("call %d = %+v, want %+v", i, calls[i], want[i])
		}
	}
}

// FindRelay: no relay for the machine is nil; any other 404 is an *Error, without a Code
// when the server predates the route (Phoenix's own 404).
func TestFindRelayNotFound(t *testing.T) {
	answer := `{"error":{"code":"relay_not_found","message":"This machine has no qube listen URL for this app."}}`
	srv, _ := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, answer)
	})
	c := New(srv.URL, 0)
	c.Token = "qct_x"
	if relay, err := c.FindRelay("app1", "d"); relay != nil || err != nil {
		t.Fatalf("relay_not_found: %+v, %v", relay, err)
	}

	answer = `{"error":{"code":"not_found","message":"no such app in this organization"}}`
	var e *Error
	if relay, err := c.FindRelay("app1", "d"); relay != nil || !errors.As(err, &e) || e.Code != "not_found" {
		t.Fatalf("unknown app: %+v, %v", relay, err)
	}

	answer = `{"errors":{"detail":"Not Found"}}`
	if relay, err := c.FindRelay("app1", "d"); relay != nil || !errors.As(err, &e) || e.Status != 404 || e.Code != "" {
		t.Fatalf("no route: %+v, %v", relay, err)
	}
}
