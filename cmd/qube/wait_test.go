package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func fastWait(t *testing.T) {
	old := waitFirstDelay
	waitFirstDelay = time.Millisecond
	t.Cleanup(func() { waitFirstDelay = old })
}

// replies answers each GET with the next of its bodies, repeating the last.
func replies(bodies ...interface{}) http.HandlerFunc {
	n := 0
	return func(w http.ResponseWriter, r *http.Request) {
		b := bodies[len(bodies)-1]
		if n < len(bodies) {
			b = bodies[n]
		}
		n++
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": b})
	}
}

func TestTakeWait(t *testing.T) {
	rest, w, err := takeWait([]string{"c1", "--wait", "--max-returned", "5"})
	if err != nil || !w.on || w.limit != defaultWaitLimit || strings.Join(rest, " ") != "c1 --max-returned 5" {
		t.Fatalf("rest=%v wait=%+v err=%v", rest, w, err)
	}
	if _, w, _ = takeWait([]string{"c1", "--wait=90s"}); !w.on || w.limit != 90*time.Second {
		t.Fatalf("--wait=90s: %+v", w)
	}
	if _, w, _ = takeWait([]string{"c1", "--", "--wait"}); w.on {
		t.Fatal("--wait after -- is an argument, not the flag")
	}
	if _, _, err = takeWait([]string{"--wait=soon"}); err == nil {
		t.Fatal("want an error for --wait=soon")
	}
}

func TestPacerStopsAtTheLimitAndOnCancel(t *testing.T) {
	fastWait(t)
	if err := newPacer(context.Background(), time.Nanosecond).next(); err != errWaitLimit {
		t.Fatalf("limit: %v", err)
	}
	bg, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newPacer(bg, 0).next(); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestQBWaitPrintsTheAnswer(t *testing.T) {
	fastWait(t)
	setenv(t, "QUBE_CACHE_DIR", t.TempDir())
	// the operation queues req_1; the show route then answers it
	c2 := newAPITestApp(t, true, map[string]http.HandlerFunc{
		"/api/v2/openapi.json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(qbSpec)) },
		"/api/v2/connections/conn_1/customers": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "req_1", "state": "waiting"}})
		},
		"/api/v2/connections/conn_1/queued-requests/req_1": replies(
			map[string]interface{}{"id": "req_1", "state": "sent"},
			map[string]interface{}{"id": "req_1", "state": "response_received", "iteration_state": "not_applicable", "response_json": map[string]interface{}{"status": "success", "data": []string{"Northwind"}}},
		),
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c2.qb([]string{"customers", "list", "conn_1", "--wait"}) })
	if !strings.Contains(out, `"status": "success"`) || !strings.Contains(out, "Northwind") {
		t.Fatalf("output = %q", out)
	}
}

func TestWaitFollowsEveryPage(t *testing.T) {
	fastWait(t)
	p1 := map[string]interface{}{"id": "p1", "page": 1, "state": "response_received", "iteration_state": "continue", "response_json": map[string]interface{}{"data": []string{"A"}}}
	p2wait := map[string]interface{}{"id": "p2", "page": 2, "state": "waiting"}
	p2 := map[string]interface{}{"id": "p2", "page": 2, "state": "response_received", "iteration_state": "done", "response_json": map[string]interface{}{"data": []string{"B"}}}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/queued-requests/p1/pages": replies([]interface{}{p1, p2wait}),
		"/api/v2/connections/conn_1/queued-requests/p2":       replies(p2),
		"/api/v2/connections/conn_1/queued-requests/p2/pages": replies([]interface{}{p1, p2}),
	})
	cl, _ := c.appClient()
	pages, ok := c.waitForRequest(cl, "conn_1", p1, time.Minute)
	if !ok || len(pages) != 2 || pages[1]["id"] != "p2" {
		t.Fatalf("ok=%v pages=%v", ok, pages)
	}
}

func TestWaitReportsAFailure(t *testing.T) {
	fastWait(t)
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/queued-requests/req_1": replies(
			map[string]interface{}{"id": "req_1", "state": "retryable", "error": map[string]interface{}{"user_message": "QuickBooks is not running"}},
			map[string]interface{}{"id": "req_1", "state": "error", "error": map[string]interface{}{"error_type": "quickbooks_business_error", "error_code": "3100"}},
		),
	})
	cl, _ := c.appClient()
	pages, ok := c.waitForRequest(cl, "conn_1", map[string]interface{}{"id": "req_1", "state": "waiting"}, time.Minute)
	if ok || len(pages) != 1 || pages[0]["state"] != "error" {
		t.Fatalf("ok=%v pages=%v", ok, pages)
	}
	if got := errorLine(pages[0]["error"].(map[string]interface{})); got != "quickbooks_business_error: 3100" {
		t.Fatalf("errorLine = %q", got)
	}
}

func TestWorkflowRunWaitStopsForADecision(t *testing.T) {
	fastWait(t)
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "run_1", "workflow": "sync", "state": "running"}})
		},
		"/api/v2/connections/conn_1/workflow-runs/run_1": replies(
			map[string]interface{}{"id": "run_1", "state": "running"},
			map[string]interface{}{"id": "run_1", "state": "awaiting_input", "prompt": map[string]interface{}{"id": "name_taken", "options": []string{"rename", "use_existing"}}},
		),
	})
	setJSON(t, false)
	c.connection = "conn_1"
	out := captureStdout(t, func() { c.workflows([]string{"run", "sync", "--wait"}) })
	if !strings.Contains(out, "Run run_1 is waiting for a decision:") || !strings.Contains(out, "name_taken") || !strings.Contains(out, "qube workflows decide conn_1 run_1 <option>") {
		t.Fatalf("output = %q", out)
	}
}
