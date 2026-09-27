package main

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"strings"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

const qbSpec = `{
  "info": {"version": "2.0.0"},
  "paths": {
    "/connections/{connection_id}/customers": {
      "get": {
        "operationId": "listCustomers", "summary": "List customers",
        "parameters": [
          {"in": "path", "name": "connection_id", "required": true, "schema": {"type": "string"}},
          {"in": "query", "name": "max_returned", "schema": {"type": "integer"}},
          {"in": "query", "name": "webhook_url", "schema": {"type": "string"}}
        ],
        "callbacks": {"answered": {}}
      },
      "post": {
        "operationId": "createCustomer", "summary": "Create customer",
        "parameters": [{"in": "path", "name": "connection_id", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {"name": {"type": "string"}}}}}},
        "callbacks": {"answered": {}}
      }
    }
  }
}`

type qbSeen struct {
	method, path, query, body string
	specFetches               int
}

// qbTest serves the spec and the customers operation for one app (sandbox or production)
// and returns a ctx pointed at it, with its own cache directory.
func qbTest(t *testing.T, sandbox bool) (*ctx, *qbSeen) {
	t.Helper()
	seen := &qbSeen{}
	c := newAPITestApp(t, sandbox, qbHandlers(t, seen))
	setenv(t, "QUBE_CACHE_DIR", t.TempDir())
	return c, seen
}

func qbHandlers(t *testing.T, seen *qbSeen) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/api/v2/openapi.json": func(w http.ResponseWriter, r *http.Request) {
			seen.specFetches++
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the spec was fetched with credentials")
			}
			_, _ = w.Write([]byte(qbSpec))
		},
		"/api/v2/connections/conn_1/customers": func(w http.ResponseWriter, r *http.Request) {
			body, _ := ioutil.ReadAll(r.Body)
			seen.method, seen.path, seen.query, seen.body = r.Method, r.URL.Path, r.URL.RawQuery, string(body)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "req_1", "state": "waiting", "request_types": []string{"CustomerQueryRq"}}})
		},
	}
}

func TestQBListQueuesARequest(t *testing.T) {
	c, seen := qbTest(t, true)
	ui.JSON = false
	out := captureStdout(t, func() { c.qb([]string{"customers", "list", "conn_1", "--max-returned", "5"}) })
	if seen.method != "GET" || seen.path != "/api/v2/connections/conn_1/customers" || seen.query != "max_returned=5" {
		t.Fatalf("server saw %s %s?%s", seen.method, seen.path, seen.query)
	}
	if !strings.Contains(out, "Queued req_1 on connection conn_1: CustomerQueryRq (waiting).") {
		t.Fatalf("output = %q", out)
	}
	// the second call reads the cached list
	_ = captureStdout(t, func() { c.qb([]string{"customers", "list", "conn_1"}) })
	if seen.specFetches != 1 {
		t.Fatalf("spec fetched %d times, want 1", seen.specFetches)
	}
}

func TestQBWriteInASandboxAppDoesNotAsk(t *testing.T) {
	c, seen := qbTest(t, true)
	ui.JSON = true
	defer func() { ui.JSON = false }()
	out := captureStdout(t, func() { c.qb([]string{"customers", "create", "conn_1", "--name", "Northwind"}) })
	if seen.method != "POST" || seen.body != `{"name":"Northwind"}` {
		t.Fatalf("server saw %s %s", seen.method, seen.body)
	}
	if !strings.Contains(out, `"id": "req_1"`) {
		t.Fatalf("output = %q", out)
	}
}

func TestQBWriteInAProductionAppGoesAheadWithYes(t *testing.T) {
	c, seen := qbTest(t, false)
	c.yes = true
	ui.JSON = true
	defer func() { ui.JSON = false }()
	_ = captureStdout(t, func() { c.qb([]string{"customers", "create", "conn_1", "--data", `{"name":"Northwind"}`}) })
	if seen.method != "POST" {
		t.Fatalf("a production write with --yes should be sent; server saw %q", seen.method)
	}
}

func TestQBReadInAProductionAppDoesNotAsk(t *testing.T) {
	c, seen := qbTest(t, false)
	ui.JSON = true
	defer func() { ui.JSON = false }()
	_ = captureStdout(t, func() { c.qb([]string{"customers", "list", "conn_1"}) })
	if seen.method != "GET" {
		t.Fatalf("a production read should not ask; server saw %q", seen.method)
	}
}

func TestQBListsResourcesAndHelp(t *testing.T) {
	c, _ := qbTest(t, true)
	ui.JSON = false
	out := captureStdout(t, func() { c.qb(nil) })
	if !strings.Contains(out, "customers  list create") {
		t.Fatalf("listing = %q", out)
	}
	out = captureStdout(t, func() { c.qb([]string{"customers", "create", "--help"}) })
	if !strings.Contains(out, "Usage: qube qb customers create <connection> [flags]") || !strings.Contains(out, "--name STRING") {
		t.Fatalf("help = %q", out)
	}
	out = captureStdout(t, func() { c.qbComplete([]string{"customers"}) })
	if out != "list\ncreate\n" {
		t.Fatalf("completion = %q", out)
	}
}
