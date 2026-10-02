package ops

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A body shaped the way the v2 spec writes its rules (see rules.go): a flat optional choice
// with a set, a required flat choice, a oneOf, a required repeating choice (structural), an
// optional one (prose only), list items with their own choice and required keys.
const rulesSpec = `{
  "paths": {
    "/connections/{connection_id}/entries": {
      "post": {
        "operationId": "createJournalEntry", "summary": "Create entry",
        "parameters": [{"in": "path", "name": "connection_id", "required": true, "schema": {"type": "string"}}],
        "requestBody": {"required": true, "content": {"application/json": {
          "example": {"memo": "It's paid", "debit_line": [{"account_ref": {"full_name": "Sales"}, "amount": 10.50}]},
          "schema": {
            "type": "object",
            "properties": {
              "memo": {"type": "string", "description": "A memo."},
              "home": {"type": "boolean", "description": "Home currency.\n\nChoose at most one of: ` + "`home`" + `, or the set (` + "`currency`, `rate`" + `)."},
              "currency": {"type": "string"},
              "rate": {"type": "number"},
              "check": {"type": "boolean"},
              "card": {"type": "boolean"},
              "debit_line": {"type": "array", "description": "A debit.\n\nAt least one entry is required across ` + "`debit_line` and `credit_line`" + `; they combine, any number of each.", "items": {
                "type": "object", "required": ["account_ref"],
                "properties": {"account_ref": {"type": "object"}, "amount": {"type": "number"}, "rate": {"type": "number"}, "rate_percent": {"type": "number"}, "price_level_ref": {"type": "object"}},
                "not": {"anyOf": [{"required": ["rate", "rate_percent"]}, {"required": ["rate", "price_level_ref"]}, {"required": ["rate_percent", "price_level_ref"]}]}
              }},
              "credit_line": {"type": "array", "items": {"type": "object", "properties": {"amount": {"type": "number"}}}},
              "item_line": {"type": "array", "description": "` + "`item_line` and `group_line`" + ` combine: any number of each.", "items": {"type": "object", "properties": {"x": {"type": "string"}}}},
              "group_line": {"type": "array", "description": "` + "`item_line` and `group_line`" + ` combine: any number of each.", "items": {"type": "object", "properties": {"y": {"type": "string"}}}}
            },
            "oneOf": [
              {"title": "Miles", "type": "object", "properties": {"miles": {"type": "number"}}, "required": ["miles"]},
              {"title": "Start + End", "type": "object", "properties": {"start": {"type": "number"}, "end": {"type": "number"}}, "not": {"properties": {"start": false, "end": false}}}
            ],
            "not": {"anyOf": [
              {"required": ["home", "currency"]}, {"required": ["home", "rate"]},
              {"required": ["check", "card"]}, {"properties": {"check": false, "card": false}},
              {"properties": {"debit_line": {"maxItems": 0}, "credit_line": {"maxItems": 0}}}
            ]}
          }
        }}},
        "callbacks": {"answered": {}}
      },
      "get": {
        "operationId": "listEntries", "summary": "List entries",
        "parameters": [
          {"in": "path", "name": "connection_id", "required": true, "schema": {"type": "string"}},
          {"in": "query", "name": "kind", "required": true, "schema": {"type": "string", "enum": ["Summary", "Detail"]}},
          {"in": "query", "name": "year", "required": true, "schema": {"type": "integer"}},
          {"in": "query", "name": "list_id", "schema": {"type": "string"}, "description": "An id.\n\nChoose at most one of: ` + "`list_id`" + `, or the set (` + "`max_returned`, `name`" + `). Members of a set combine with each other, not with the other choices."},
          {"in": "query", "name": "max_returned", "schema": {"type": "integer"}, "description": "Choose at most one of: ` + "`list_id`" + `, or the set (` + "`max_returned`, `name`" + `). Members of a set combine with each other, not with the other choices."},
          {"in": "query", "name": "name", "schema": {"type": "string"}}
        ],
        "callbacks": {"answered": {}}
      }
    }
  }
}`

func rulesOp(t *testing.T, verb string) *Op {
	t.Helper()
	ix, err := Parse([]byte(rulesSpec))
	if err != nil {
		t.Fatal(err)
	}
	return ix.Find("entries", verb)
}

func TestRulesFromTheSchema(t *testing.T) {
	r := rulesOp(t, "create").bodyRules()
	want := []choice{
		{alts: [][]string{{"home"}, {"currency", "rate"}}},
		{alts: [][]string{{"check"}, {"card"}}, required: true},
		{alts: [][]string{{"miles"}, {"end", "start"}}, required: true},
	}
	if !reflect.DeepEqual(r.choices, want) {
		t.Errorf("choices = %+v\nwant      %+v", r.choices, want)
	}
	combos := []combo{
		{members: []string{"debit_line", "credit_line"}, required: true}, // the rule, with the note's order
		{members: []string{"item_line", "group_line"}},                   // the note alone
	}
	if !reflect.DeepEqual(r.combos, combos) {
		t.Errorf("combos = %+v\nwant     %+v", r.combos, combos)
	}
}

func TestQueryRulesFromTheGeneratorsNote(t *testing.T) {
	r := queryRules(rulesOp(t, "list").Query)
	want := []choice{{alts: [][]string{{"list_id"}, {"max_returned", "name"}}}}
	if !reflect.DeepEqual(r.choices, want) {
		t.Fatalf("choices = %+v, want %+v", r.choices, want)
	}
}

func TestHintsNameTheRule(t *testing.T) {
	r := rulesOp(t, "create").bodyRules()
	for field, want := range map[string][]string{
		"rate":        {"at most one of: --home | (--currency, --rate)"},
		"card":        {"exactly one of: --check | --card"},
		"start":       {"exactly one of: --miles | (--end, --start)"},
		"credit_line": {"can be combined with --debit-line; at least one entry is required across them"},
		"group_line":  {"can be combined with --item-line"},
		"memo":        nil,
	} {
		if got := r.hints(field, flagRef); !reflect.DeepEqual(got, want) {
			t.Errorf("hints(%s) = %q, want %q", field, got, want)
		}
	}
	// a long rule: a member of a set is told what it excludes
	long := choice{alts: [][]string{{"list_id"}, {"full_name"}, {"active_status", "from_modified_date", "max_returned", "name_contains", "name_ends_with", "name_range"}}}
	if got := long.hint("max_returned", flagRef); got != "not with --list-id or --full-name" {
		t.Errorf("long set member: %q", got)
	}
	if got := long.hint("list_id", flagRef); !strings.HasPrefix(got, "at most one of: --list-id | --full-name | (--active-status, ") {
		t.Errorf("long single: %q", got)
	}
}

func TestHelpShowsHintsShapesAndExample(t *testing.T) {
	var b bytes.Buffer
	rulesOp(t, "create").WriteHelp(&b, "qube qb")
	help := b.String()
	for flag, wants := range map[string][]string{
		"--home":        {"Home currency.", "(at most one of: --home | (--currency, --rate))"},
		"--miles":       {"(exactly one of: --miles | (--end, --start))"},
		"--debit-line":  {"A debit.", "(can be combined with --credit-line; at least one entry is required across them)", `JSON: [{"account_ref", "amount", "price_level_ref", …}, ...]. In each, required: account_ref; at most one of: rate | rate_percent | price_level_ref.`, "(repeat the flag, or a JSON array)"},
		"--credit-line": {"(can be combined with --debit-line; at least one entry is required across them)"},
		"--group-line":  {"(can be combined with --item-line)"},
	} {
		block := flagBlock(help, flag)
		for _, want := range wants {
			if !strings.Contains(block, want) {
				t.Errorf("%s lacks %q: %q", flag, want, block)
			}
		}
	}
	if strings.Contains(flagBlock(help, "--home"), "Choose at most") {
		t.Errorf("the generator's note should give way to the hint:\n%s", flagBlock(help, "--home"))
	}
	if !strings.HasSuffix(help, "Example:\n  qube qb entries create <connection> --data '{\"memo\":\"It'\\''s paid\",\"debit_line\":[{\"account_ref\":{\"full_name\":\"Sales\"},\"amount\":10.50}]}'\n") {
		t.Errorf("help should end with the example:\n%s", help)
	}
	if !strings.Contains(help, "\n  --example     Print the example body below as JSON") {
		t.Errorf("help should list --example:\n%s", help)
	}
}

func TestExampleJSONIsTheSpecsInItsOrder(t *testing.T) {
	op := rulesOp(t, "create")
	want := "{\n  \"memo\": \"It's paid\",\n  \"debit_line\": [\n    {\n      \"account_ref\": {\n        \"full_name\": \"Sales\"\n      },\n      \"amount\": 10.50\n    }\n  ]\n}"
	if got := string(op.ExampleJSON()); got != want {
		t.Fatalf("ExampleJSON:\n%s\nwant\n%s", got, want)
	}
	if rulesOp(t, "list").ExampleJSON() != nil {
		t.Fatal("a query has no example body")
	}
}

func TestQueryExampleUsesItsRequiredParameters(t *testing.T) {
	var b bytes.Buffer
	rulesOp(t, "list").WriteHelp(&b, "qube qb")
	if !strings.HasSuffix(b.String(), "Example:\n  qube qb entries list <connection> --kind Summary --year <INTEGER>\n") {
		t.Fatalf("help:\n%s", b.String())
	}
	b.Reset()
	parsed(t).Find("customers", "list").WriteHelp(&b, "qube qb")
	if strings.Contains(b.String(), "Example:") {
		t.Fatalf("a query with no required parameter has no example:\n%s", b.String())
	}
}

func TestLongExampleIsShortenedAndPointsAtExample(t *testing.T) {
	op := rulesOp(t, "create")
	op.Example = json.RawMessage(`{"customer_ref":{"full_name":"Acme Corp"},"invoice_line_add":[{"desc":"Blue widget, large","item_ref":{"full_name":"Widget"},"quantity":2,"rate":125.0}],"memo":"Monthly service","txn_date":"2026-01-15"}`)
	var b bytes.Buffer
	op.writeExample(&b, "qube qb")
	want := "\nExample:\n" +
		`  qube qb entries create <connection> --data '{"customer_ref":{…},"invoice_line_add":[…],"memo":"Monthly service","txn_date":"2026-01-15"}'` + "\n" +
		"\n  That body is shortened: {…} and […] stand for more. The whole of it, to edit and send:\n" +
		"  qube qb entries create --example > journal-entry.json\n" +
		"  qube qb entries create <connection> --data @journal-entry.json\n"
	if b.String() != want {
		t.Fatalf("example:\n%s\nwant\n%s", b.String(), want)
	}
}

func TestShortenJSON(t *testing.T) {
	raw := []byte(`{"z":1,"big":{"a":"aaaaaaaaaaaaaaaaaaaa","b":[1,2,3]},"small":{"k":"v"},"list":[{"x":1}]}`)
	for max, want := range map[int]string{
		200: `{"z":1,"big":{"a":"aaaaaaaaaaaaaaaaaaaa","b":[1,2,3]},"small":{"k":"v"},"list":[{"x":1}]}`,
		60:  `{"z":1,"big":{…},"small":{"k":"v"},"list":[{"x":1}]}`,
		40:  `{"z":1,"big":{…},"small":{…},"list":[…]}`,
		20:  `{"z":1,"big":{…},…}`,
	} {
		if got := shortenJSON(raw, max, nil); got != want {
			t.Errorf("shortenJSON(%d) = %s, want %s", max, got, want)
		}
	}
	// the body's required fields stay longest, each run of left-out members a …
	if got := shortenJSON(raw, 28, []string{"list"}); got != `{"z":1,…,"list":[…]}` {
		t.Errorf("shortenJSON keeping list = %s", got)
	}
}

func TestShellQuoteAndExampleFile(t *testing.T) {
	for in, want := range map[string]string{"Summary": "Summary", "it's": `'it'\''s'`, `{"a":1}`: `'{"a":1}'`} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
	for id, want := range map[string]string{"createInvoice": "invoice.json", "executeQBCommandExec": "qb-command-exec.json", "update": "body.json"} {
		if got := exampleFile(&Op{OperationID: id}); got != want {
			t.Errorf("exampleFile(%s) = %s, want %s", id, got, want)
		}
	}
}

func TestOneOfFieldsAreFlags(t *testing.T) {
	op := rulesOp(t, "create")
	call, err := op.Bind([]string{"c", "--miles", "12", "--debit-line", `{"account_ref":{}}`}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(call.Body) != `{"debit_line":[{"account_ref":{}}],"miles":12}` {
		t.Fatalf("body = %s", call.Body)
	}
}

func TestComplete(t *testing.T) {
	list := rulesOp(t, "list")
	if got := list.Complete("--kind"); !reflect.DeepEqual(got, []string{"Summary", "Detail"}) {
		t.Errorf("an enum flag's values: %q", got)
	}
	if got := list.Complete("--year"); got != nil {
		t.Errorf("a value without listed values: %q", got)
	}
	got := list.Complete("c1")
	for _, want := range []string{"--kind", "--year", "--list-id", "--max-returned", "--data", "--wait", "--help"} {
		if !contains(got, want) {
			t.Errorf("flags lack %s: %q", want, got)
		}
	}
	if contains(got, "--example") || !contains(rulesOp(t, "create").Complete("--memo=x"), "--example") {
		t.Errorf("--example is offered only where there is one")
	}
}
