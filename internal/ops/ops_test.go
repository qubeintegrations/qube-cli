package ops

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/ioutil"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A small v2 document: two QuickBooks operations on customers, one that executes, one
// that isn't a QuickBooks operation (no `answered` callback) and one with an unsafe path.
const fixture = `{
  "info": {"version": "2.0.0"},
  "paths": {
    "/connections/{connection_id}/customers": {
      "get": {
        "operationId": "listCustomers",
        "summary": "List customers",
        "description": "qbXML: CustomerQueryRq.\n\nReturns customers.",
        "parameters": [
          {"$ref": "#/components/parameters/ConnectionId"},
          {"in": "query", "name": "max_returned", "schema": {"type": "integer"}, "description": "Limits the number of objects. More text."},
          {"in": "query", "name": "iterator", "schema": {"type": "boolean"}},
          {"in": "query", "name": "name_range", "schema": {"type": "object", "properties": {"from": {"type": "string"}, "to": {"type": "string"}}}},
          {"in": "query", "name": "include", "schema": {"type": "array", "items": {"type": "string", "enum": ["Name", "Balance"]}}},
          {"in": "query", "name": "active_status", "schema": {"type": ["string", "null"], "enum": ["ActiveOnly", "All"]}},
          {"in": "query", "name": "webhook_url", "schema": {"type": "string", "format": "uri"}}
        ],
        "callbacks": {"answered": {}}
      },
      "post": {
        "operationId": "createCustomer",
        "summary": "Create customer",
        "parameters": [{"$ref": "#/components/parameters/ConnectionId"}, {"in": "query", "name": "webhook_url", "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"$ref": "#/components/schemas/CustomerAdd"}}}},
        "callbacks": {"answered": {}}
      }
    },
    "/connections/{connection_id}/txn-void": {
      "post": {
        "operationId": "executeTxnVoid",
        "summary": "Execute txn void",
        "parameters": [{"$ref": "#/components/parameters/ConnectionId"}],
        "requestBody": {"required": true, "content": {"application/json": {"schema": {"type": "object", "properties": {"txn_id": {"type": "string"}}}}}},
        "callbacks": {"answered": {}}
      }
    },
    "/connections/{connection_id}/queued_requests": {
      "get": {"operationId": "listQueuedRequests", "summary": "List queued requests"}
    },
    "/connections/{connection_id}/../etc": {
      "get": {"operationId": "listEtc", "callbacks": {"answered": {}}}
    }
  },
  "components": {
    "parameters": {
      "ConnectionId": {"in": "path", "name": "connection_id", "required": true, "schema": {"type": "string"}}
    },
    "schemas": {
      "CustomerAdd": {
        "type": "object",
        "required": ["name"],
        "properties": {
          "name": {"type": "string", "description": "The customer's name."},
          "credit_limit": {"type": "number"},
          "is_active": {"type": "boolean"},
          "bill_address": {"$ref": "#/components/schemas/Address"},
          "parent": {"$ref": "#/components/schemas/CustomerAdd"}
        }
      },
      "Address": {"type": "object", "properties": {"city": {"type": "string"}, "state": {"type": "string"}}}
    }
  }
}`

func parsed(t *testing.T) *Index {
	t.Helper()
	ix, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestParseKeepsQuickBooksOperations(t *testing.T) {
	ix := parsed(t)
	var got []string
	for _, op := range ix.Ops {
		got = append(got, op.Resource+" "+op.Verb+" "+op.Method)
	}
	want := []string{"customers list GET", "customers create POST", "txn-void execute POST"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ops = %v, want %v", got, want)
	}
	if ix.Version != "2.0.0" || !reflect.DeepEqual(ix.Resources(), []string{"customers", "txn-void"}) {
		t.Fatalf("version %q resources %v", ix.Version, ix.Resources())
	}
	list := ix.Find("customers", "list")
	if !reflect.DeepEqual(list.PathParams, []string{"connection_id"}) || len(list.Query) != 6 {
		t.Fatalf("path params %v, query %d", list.PathParams, len(list.Query))
	}
	if p := list.Query[4]; p.Name != "active_status" || p.Type != "string" || len(p.Enum) != 2 {
		t.Fatalf("a 3.1 type list: %+v", p)
	}
}

func TestParseInlinesBodyRefsButNotCycles(t *testing.T) {
	op := parsed(t).Find("customers", "create")
	var body map[string]interface{}
	if err := json.Unmarshal(op.Body, &body); err != nil {
		t.Fatal(err)
	}
	props := body["properties"].(map[string]interface{})
	addr := props["bill_address"].(map[string]interface{})
	if _, ok := addr["properties"].(map[string]interface{})["city"]; !ok {
		t.Fatalf("bill_address not inlined: %v", addr)
	}
	if ref := props["parent"].(map[string]interface{})["$ref"]; ref != "#/components/schemas/CustomerAdd" {
		t.Fatalf("a self-reference should stay a $ref, got %v", props["parent"])
	}
	if !op.BodyRequired {
		t.Fatal("body should be required")
	}
}

func TestParseRejectsDocumentsWithoutOperations(t *testing.T) {
	if _, err := Parse([]byte(`{"paths": {"/workflows": {"get": {}}}}`)); err == nil {
		t.Fatal("want an error for a document with no QuickBooks operations")
	}
	if _, err := Parse([]byte(`<html>`)); err == nil {
		t.Fatal("want an error for a document that isn't JSON")
	}
}

func TestBindQuery(t *testing.T) {
	op := parsed(t).Find("customers", "list")
	call, err := op.Bind([]string{
		"conn 1", "--max-returned", "5", "--iterator", "--name_range", `{"from":"A","to":"M"}`,
		"--include", "Name", "--include=Balance", "--webhook-url=https://example.test/hook",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if call.Method != "GET" || call.Path != "/api/v2/connections/conn%201/customers" || call.Body != nil {
		t.Fatalf("call = %+v", call)
	}
	want := url.Values{
		"max_returned":     {"5"},
		"iterator":         {"true"},
		"name_range[from]": {"A"},
		"name_range[to]":   {"M"},
		"include[]":        {"Name", "Balance"},
		"webhook_url":      {"https://example.test/hook"},
	}
	if !reflect.DeepEqual(call.Query, want) {
		t.Fatalf("query = %v\nwant    %v", call.Query, want)
	}
}

func TestBindArrayAsJSONAndBooleanFalse(t *testing.T) {
	op := parsed(t).Find("customers", "list")
	call, err := op.Bind([]string{"c", "--include", `["Name","Balance"]`, "--iterator=false"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := call.Query.Encode(); got != "include%5B%5D=Name&include%5B%5D=Balance&iterator=false" {
		t.Fatalf("query = %s", got)
	}
}

func TestBindBodyFlagsWinOverData(t *testing.T) {
	op := parsed(t).Find("customers", "create")
	call, err := op.Bind([]string{"c", "--data", `{"name":"Old","credit_limit":1.10}`, "--name", "Acme", "--bill-address", `{"city":"Austin"}`, "--is-active=false"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(call.Body, &body); err != nil {
		t.Fatal(err)
	}
	if body["name"] != "Acme" || body["is_active"] != false || body["bill_address"].(map[string]interface{})["city"] != "Austin" {
		t.Fatalf("body = %s", call.Body)
	}
	if !strings.Contains(string(call.Body), `"credit_limit":1.10`) {
		t.Fatalf("numbers should pass through exactly: %s", call.Body)
	}
}

func TestBindDataFromFileAndStdin(t *testing.T) {
	op := parsed(t).Find("customers", "create")
	file := filepath.Join(t.TempDir(), "c.json")
	if err := ioutil.WriteFile(file, []byte(`{"name":"From file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	call, err := op.Bind([]string{"c", "--data", "@" + file}, nil)
	if err != nil || string(call.Body) != `{"name":"From file"}` {
		t.Fatalf("file: %s %v", call.Body, err)
	}
	call, err = op.Bind([]string{"c", "--data", "-"}, bytes.NewBufferString(`{"name":"From stdin"}`))
	if err != nil || string(call.Body) != `{"name":"From stdin"}` {
		t.Fatalf("stdin: %v %v", call, err)
	}
}

func TestBindErrors(t *testing.T) {
	ix := parsed(t)
	list, create := ix.Find("customers", "list"), ix.Find("customers", "create")
	var unknown *UnknownFlagError
	if _, err := list.Bind([]string{"c", "--nope", "1"}, nil); !errors.As(err, &unknown) || unknown.Flag != "nope" {
		t.Fatalf("unknown flag: %v", err)
	}
	cases := []struct {
		op   *Op
		args []string
		want string
	}{
		{list, []string{}, "takes <connection> (got 0 arguments)"},
		{list, []string{"a", "b"}, "got 2 arguments"},
		{list, []string{"c", "--max-returned", "five"}, "whole number"},
		{list, []string{"c", "--max-returned"}, "needs a value"},
		{list, []string{"c", "--name-range", `"A"`}, "JSON object"},
		{list, []string{"c", "--data", "{}"}, "takes no body"},
		{create, []string{"c"}, "needs a body"},
		{create, []string{"c", "--data", "{"}, "not JSON"},
		{create, []string{"c", "--data", "[1]", "--name", "x"}, "must be a JSON object"},
	}
	for _, tc := range cases {
		_, err := tc.op.Bind(tc.args, nil)
		var usage *UsageError
		if !errors.As(err, &usage) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %v: err = %v, want a usage error containing %q", tc.op.Verb, tc.args, err, tc.want)
		}
	}
}

func TestFlagsSkipReservedNamesAndCollisions(t *testing.T) {
	op := &Op{
		Query: []Param{{Name: "data", Type: "string"}, {Name: "request_id", Type: "string"}},
		Body:  json.RawMessage(`{"type":"object","properties":{"request_id":{"type":"string"},"help":{"type":"string"},"name":{"type":"string"}}}`),
	}
	var names []string
	for _, f := range op.Flags() {
		names = append(names, f.Name)
	}
	if !reflect.DeepEqual(names, []string{"request-id", "name"}) {
		t.Fatalf("flags = %v", names)
	}
}

func TestEncodeQueryNesting(t *testing.T) {
	q := url.Values{}
	encodeQuery(q, "f", map[string]interface{}{
		"a": []interface{}{"x", "y"},
		"b": map[string]interface{}{"c": json.Number("2")},
	})
	if got := q.Encode(); got != "f%5Ba%5D%5B%5D=x&f%5Ba%5D%5B%5D=y&f%5Bb%5D%5Bc%5D=2" {
		t.Fatalf("query = %s", got)
	}
}

func TestHelpShowsFlagsAndBody(t *testing.T) {
	ix := parsed(t)
	var b bytes.Buffer
	ix.Find("customers", "list").WriteHelp(&b, "qube qb")
	for _, want := range []string{
		"Usage: qube qb customers list <connection> [flags]",
		"List customers.  GET /api/v2/connections/{connection_id}/customers",
		"--max-returned INTEGER", "Limits the number of objects.",
		`--name-range OBJECT`, `JSON: {"from", "to"}`,
		"--include STRING...", "One of: Name, Balance.",
		"--iterator ",
	} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("help lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	ix.Find("customers", "create").WriteHelp(&b, "qube qb")
	if !strings.Contains(b.String(), "Body (JSON, required)") || !strings.Contains(b.String(), "--name STRING") || !strings.Contains(b.String(), "(required) The customer's name.") {
		t.Errorf("create help:\n%s", b.String())
	}
	b.Reset()
	ix.WriteResources(&b)
	if !strings.Contains(b.String(), "customers  list create") {
		t.Errorf("resources:\n%s", b.String())
	}
}

func TestKebab(t *testing.T) {
	for in, want := range map[string]string{
		"listCustomers":        "list-customers",
		"executeQBCommandExec": "execute-qb-command-exec",
		"recordPayment":        "record-payment",
	} {
		if got := kebab(in); got != want {
			t.Errorf("kebab(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------- cache

func source(t *testing.T, dir string, now time.Time, fetches *int, fetchErr error) *Source {
	return &Source{
		Host:     "https://dev.example.test",
		CacheDir: dir,
		MaxAge:   24 * time.Hour,
		Now:      func() time.Time { return now },
		Fetch: func() ([]byte, error) {
			*fetches++
			if fetchErr != nil {
				return nil, fetchErr
			}
			return []byte(fixture), nil
		},
	}
}

func TestLoadCachesAndRefetchesWhenStale(t *testing.T) {
	dir, fetches := t.TempDir(), 0
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	ix, fetched, warn, err := source(t, dir, now, &fetches, nil).Load(false)
	if err != nil || warn != nil || !fetched || len(ix.Ops) != 3 || fetches != 1 {
		t.Fatalf("first load: fetched=%v warn=%v err=%v fetches=%d", fetched, warn, err, fetches)
	}
	if _, _, _, err = source(t, dir, now.Add(time.Hour), &fetches, nil).Load(false); err != nil || fetches != 1 {
		t.Fatalf("a fresh cache should be used: err=%v fetches=%d", err, fetches)
	}
	if _, _, _, err = source(t, dir, now.Add(time.Hour), &fetches, nil).Load(true); err != nil || fetches != 2 {
		t.Fatalf("refresh should fetch: err=%v fetches=%d", err, fetches)
	}
	if _, fetched, _, err = source(t, dir, now.Add(48*time.Hour), &fetches, nil).Load(false); err != nil || !fetched || fetches != 3 {
		t.Fatalf("a stale cache should be refetched: err=%v fetches=%d", err, fetches)
	}
}

func TestLoadFallsBackToAStaleCache(t *testing.T) {
	dir, fetches := t.TempDir(), 0
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, _, _, err := source(t, dir, now, &fetches, nil).Load(false); err != nil {
		t.Fatal(err)
	}
	down := errors.New("connection refused")
	ix, fetched, warn, err := source(t, dir, now.Add(72*time.Hour), &fetches, down).Load(false)
	if err != nil || fetched || warn != down || ix == nil || !ix.FetchedAt.Equal(now) {
		t.Fatalf("stale fallback: ix=%v fetched=%v warn=%v err=%v", ix != nil, fetched, warn, err)
	}
	if _, _, _, err := source(t, t.TempDir(), now, &fetches, down).Load(false); err != down {
		t.Fatalf("no cache and no host: err = %v", err)
	}
}

func TestLoadIgnoresAnotherHostsOrFormatsCache(t *testing.T) {
	dir, fetches := t.TempDir(), 0
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	s := source(t, dir, now, &fetches, nil)
	old := &Index{Format: Format - 1, Host: s.Host, FetchedAt: now, Ops: []Op{{Resource: "x", Verb: "list"}}}
	data, _ := json.Marshal(old)
	if err := ioutil.WriteFile(s.CachePath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, fetched, _, err := s.Load(false); err != nil || !fetched {
		t.Fatalf("an old-format cache should be refetched: fetched=%v err=%v", fetched, err)
	}
	if filepath.Base(s.CachePath()) != "openapi-v2-dev.example.test.json" {
		t.Fatalf("cache path %s", s.CachePath())
	}
}

func TestBriefKeepsWholeSentences(t *testing.T) {
	long := strings.Repeat("A sentence that goes on. ", 40)
	got, cut := brief("qbXML: `InvoiceAddRq`.\n\n"+long, 300)
	if !cut || !strings.HasPrefix(got, "qbXML: `InvoiceAddRq`.\n\nA sentence") || !strings.HasSuffix(got, "on. …") || len(got) > 305 {
		t.Fatalf("brief = %q (cut %v)", got, cut)
	}
	if got, cut := brief("Short.\n\nAlso short.", 300); cut || got != "Short.\n\nAlso short." {
		t.Fatalf("brief = %q (cut %v)", got, cut)
	}
}
