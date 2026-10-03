package main

import (
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os/exec"
	"runtime"
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
          {"in": "query", "name": "active_status", "schema": {"type": "string", "enum": ["ActiveOnly", "All"]}},
          {"in": "query", "name": "webhook_url", "schema": {"type": "string"}}
        ],
        "callbacks": {"answered": {}}
      },
      "post": {
        "operationId": "createCustomer", "summary": "Create customer",
        "parameters": [{"in": "path", "name": "connection_id", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {"example": {"name": "Northwind"}, "schema": {"type": "object", "properties": {"name": {"type": "string"}}}}}},
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

func TestQBExamplePrintsTheSpecsBody(t *testing.T) {
	c, seen := qbTest(t, true)
	ui.JSON = false
	out := captureStdout(t, func() { c.qb([]string{"customers", "create", "--example"}) })
	if out != "{\n  \"name\": \"Northwind\"\n}\n" {
		t.Fatalf("--example printed %q", out)
	}
	if seen.method != "" {
		t.Fatalf("--example sent a request: %s %s", seen.method, seen.path)
	}
	out = captureStdout(t, func() { c.qb([]string{"customers", "create", "--help"}) })
	if !strings.HasSuffix(out, "Example:\n  qube qb customers create <connection> --data '{\"name\":\"Northwind\"}'\n") {
		t.Fatalf("help should end with the example: %q", out)
	}
}

func TestQBCompletesFlagsAndValues(t *testing.T) {
	c, _ := qbTest(t, true)
	ui.JSON = false
	_ = captureStdout(t, func() { c.qb([]string{"customers"}) }) // reads the list into the cache
	if out := captureStdout(t, func() { c.qbComplete([]string{"customers", "list", "conn_1"}) }); out != "--max-returned\n--active-status\n--webhook-url\n--data\n--wait\n--help\n" {
		t.Fatalf("flags = %q", out)
	}
	if out := captureStdout(t, func() { c.qbComplete([]string{"customers", "list", "--active-status"}) }); out != "ActiveOnly\nAll\n" {
		t.Fatalf("values = %q", out)
	}
	if out := captureStdout(t, func() { c.qbComplete([]string{"customers", "list", "--max-returned"}) }); out != "" {
		t.Fatalf("a number has nothing to offer: %q", out)
	}
	if out := captureStdout(t, func() { c.qbComplete([]string{"customers", "create"}) }); !strings.HasSuffix(out, "--example\n") {
		t.Fatalf("create flags = %q", out)
	}
}

// The scripts are valid in the shells at hand, and ask `qube qb --complete` for an
// operation's flags.
func TestCompletionScripts(t *testing.T) {
	for _, shell := range []string{"bash", "zsh", "fish"} {
		script := captureStdout(t, func() { completion([]string{shell}) })
		if !strings.Contains(script, "qube qb --complete") || strings.Count(script, "--complete") < 3 {
			t.Errorf("%s: the script doesn't complete qb resources, verbs and flags:\n%s", shell, script)
		}
		path, err := exec.LookPath(shell)
		if err != nil || shell == "fish" || runtime.GOOS == "windows" { // there, bash may be WSL's stub
			continue
		}
		cmd := exec.Command(path, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s -n: %v\n%s", shell, err, out)
		}
	}
}

// subsetSpec serves the qube spec's subset internal/ops tests use (data-exts update among
// it), and records the request an operation sends.
func subsetSpecTest(t *testing.T) (*ctx, *qbSeen) {
	t.Helper()
	spec, err := ioutil.ReadFile("../../internal/ops/testdata/qbxml_subset.json")
	if err != nil {
		t.Fatal(err)
	}
	seen := &qbSeen{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/openapi.json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(spec) },
		"/api/v2/connections/conn_1/data-exts": func(w http.ResponseWriter, r *http.Request) {
			body, _ := ioutil.ReadAll(r.Body)
			seen.method, seen.path, seen.body = r.Method, r.URL.Path, string(body)
			w.WriteHeader(201)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "req_1", "state": "waiting", "request_types": []string{"DataExtModRq"}}})
		},
	})
	setenv(t, "QUBE_CACHE_DIR", t.TempDir())
	setJSON(t, false)
	return c, seen
}

// The help of an operation whose body is a oneOf lists the flags every shape takes, then
// each shape's flags as an alternative of an "Exactly one of" block.
func TestQBHelpShowsEachShapeOfAOneOf(t *testing.T) {
	c, _ := subsetSpecTest(t)
	help := captureStdout(t, func() { c.qb([]string{"data-exts", "update", "--help"}) })
	body := help[strings.Index(help, "Body (JSON"):]
	var flags []string
	for _, l := range strings.Split(body, "\n")[1:] {
		if l == "" {
			break
		}
		if !strings.HasPrefix(l, "      ") { // a flag or a heading, not a description's line
			flags = append(flags, strings.Fields(l)[0])
		}
	}
	want := []string{
		"--data-ext-name", "--data-ext-value", "--owner-id", "--request-id",
		"Exactly", "--list-data-ext-type", "--list-obj-ref",
		"or:", "--txn-data-ext-type", "--txn-id", "--txn-line-id",
		"or:", "--other-data-ext-type",
	}
	if strings.Join(flags, " ") != strings.Join(want, " ") {
		t.Fatalf("body flags = %q\nwant %q\n%s", flags, want, body)
	}
	if !strings.Contains(body, "\n  Exactly one of these groups (the flags in a group combine):\n    --list-data-ext-type STRING ") {
		t.Fatalf("help:\n%s", body)
	}
}

// A field of one oneOf alternative is a flag like any other: a sales order's data
// extension is addressed with --txn-data-ext-type and --txn-id, no --data needed.
func TestQBOneOfBranchFieldsAreFlags(t *testing.T) {
	c, seen := subsetSpecTest(t)
	_ = captureStdout(t, func() {
		c.qb([]string{"data-exts", "update", "conn_1", "--data-ext-name", "Color", "--data-ext-value", "Blue", "--owner-id", "0", "--txn-data-ext-type", "SalesOrder", "--txn-id", "1A-123"})
	})
	want := `{"data_ext_name":"Color","data_ext_value":"Blue","owner_id":"0","txn_data_ext_type":"SalesOrder","txn_id":"1A-123"}`
	if seen.method != "PUT" || seen.path != "/api/v2/connections/conn_1/data-exts" || seen.body != want {
		t.Fatalf("server saw %s %s %s\nwant body %s", seen.method, seen.path, seen.body, want)
	}
}
