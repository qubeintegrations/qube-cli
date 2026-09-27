package ops

import (
	"errors"
	"io/ioutil"
	"os"
	"testing"
	"time"
)

// With QUBE_SPEC naming a copy of a host's /api/v2/openapi.json, check the whole of it parses.
func TestParseServedSpec(t *testing.T) {
	path := os.Getenv("QUBE_SPEC")
	if path == "" {
		t.Skip("set QUBE_SPEC to a saved /api/v2/openapi.json")
	}
	data, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ix, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d operations on %d resources in %v", len(ix.Ops), len(ix.Resources()), time.Since(start))
	verbs := map[string]int{}
	for _, op := range ix.Ops {
		verbs[op.Verb]++
		if len(op.PathParams) != 1 || op.PathParams[0] != "connection_id" {
			t.Errorf("%s %s: path params %v", op.Resource, op.Verb, op.PathParams)
		}
	}
	t.Logf("verbs: %v", verbs)

	// Every operation renders its help and binds a bare connection: a call, or (for a
	// required body) a usage error, never a panic.
	for i := range ix.Ops {
		op := &ix.Ops[i]
		op.WriteHelp(ioutil.Discard, "qube qb")
		_, err := op.Bind([]string{"conn"}, nil)
		var usage *UsageError
		if err != nil && !(errors.As(err, &usage) && op.BodyRequired) {
			t.Errorf("%s %s: %v", op.Resource, op.Verb, err)
		}
	}
	call, err := ix.Find("customers", "list").Bind([]string{"c1", "--name-range", `{"from":"A","to":"M"}`, "--include", "Name", "--include", "Balance", "--max-returned", "5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := call.Query.Encode(); got != "include%5B%5D=Name&include%5B%5D=Balance&max_returned=5&name_range%5Bfrom%5D=A&name_range%5Bto%5D=M" {
		t.Errorf("customers list query = %s", got)
	}
}
