package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

// A run waiting at a question, as the server's prompt shape has it: details, one choice per
// candidate, a search choice with a `record` field, and choices with fields or without.
func waitingRun(host string) map[string]interface{} {
	run := map[string]interface{}{
		"id":            "run_1",
		"state":         "awaiting_input",
		"current_state": "resolve / ask",
		"prompt": map[string]interface{}{
			"id":      "resolve / ask",
			"token":   "pt_abc",
			"kind":    "decision",
			"message": "Which customer is \"Lone Star Supply\"?",
			"details": []interface{}{
				map[string]interface{}{"label": "Name", "value": "Lone Star Supply"},
				map[string]interface{}{"label": "Kind", "value": "Customer"},
			},
			"options": []interface{}{map[string]interface{}{"id": "use"}},
			"choices": []interface{}{
				map[string]interface{}{"id": "use:1", "label": "Lone Star Supply", "detail": "Customer · Austin, TX", "description": nil, "fields": []interface{}{}},
				map[string]interface{}{"id": "use:2", "label": "Lone Star Supply Co", "detail": "Customer · Dallas, TX", "fields": []interface{}{}},
				map[string]interface{}{"id": "use", "label": "Another customer…", "fields": []interface{}{
					map[string]interface{}{"name": "list_id", "type": "record", "kind": "customer", "label": "Customer", "required": true},
				}},
				map[string]interface{}{"id": "narrow", "label": "Narrow it down", "fields": []interface{}{
					map[string]interface{}{"name": "city", "type": "string", "label": "City", "required": false},
					map[string]interface{}{"name": "email", "type": "string", "required": true},
				}},
				map[string]interface{}{"id": "cancel", "label": "Cancel", "description": "Stop here. Nothing is created.", "fields": []interface{}{}},
			},
		},
	}
	if host != "" {
		run["links"] = map[string]interface{}{
			"self":      "http://" + host + "/api/v2/connections/conn_1/workflow-runs/run_1",
			"decisions": "http://" + host + "/api/v2/connections/conn_1/workflow-runs/run_1/decisions?prompt_token=pt_abc",
			"search":    "http://" + host + "/api/v2/connections/conn_1/workflow-runs/run_1/search?prompt_token=pt_abc",
		}
	}
	return run
}

func runHandler(run func(host string) map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": run(r.Host)})
	}
}

// decisionStub records the body of each answer POSTed to the run's decisions route.
func decisionStub(bodies *[]map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		*bodies = append(*bodies, body)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "run_1", "state": "running"}})
	}
}

func TestDecideByChoiceSendsTheChoiceAndItsData(t *testing.T) {
	var bodies []map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1/decisions": decisionStub(&bodies),
	})
	setJSON(t, false)
	out := captureStdout(t, func() {
		c.workflows([]string{"decide", "conn_1", "run_1", "--choice", "use", "--data", `{"list_id":"80000012-1712345678"}`})
	})
	want := map[string]interface{}{"choice": "use", "data": map[string]interface{}{"list_id": "80000012-1712345678"}}
	if len(bodies) != 1 || !reflect.DeepEqual(bodies[0], want) {
		t.Fatalf("bodies = %v, want [%v]", bodies, want)
	}
	if !strings.Contains(out, `Decided "use" on run run_1: now running`) {
		t.Fatalf("output = %q", out)
	}
}

// The flag may follow the arguments, and a chosen connection stands in for the first one.
func TestDecideByChoiceTakesTheDefaultConnectionAndFlagsAnywhere(t *testing.T) {
	var bodies []map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1/decisions": decisionStub(&bodies),
	})
	c.connection = "conn_1"
	setJSON(t, false)
	captureStdout(t, func() { c.workflows([]string{"decide", "run_1", "--choice", "use:1"}) })
	want := map[string]interface{}{"choice": "use:1", "data": map[string]interface{}{}}
	if len(bodies) != 1 || !reflect.DeepEqual(bodies[0], want) {
		t.Fatalf("bodies = %v, want [%v]", bodies, want)
	}
}

// {option, data} keeps working: the older form has no choice in it.
func TestDecideByOptionStillSendsTheOption(t *testing.T) {
	var bodies []map[string]interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1/decisions": decisionStub(&bodies),
	})
	setJSON(t, false)
	out := captureStdout(t, func() {
		c.workflows([]string{"decide", "conn_1", "run_1", "rename", "--data", `{"name":"Lone Star Supply Co (2)"}`})
	})
	want := map[string]interface{}{"option": "rename", "data": map[string]interface{}{"name": "Lone Star Supply Co (2)"}}
	if len(bodies) != 1 || !reflect.DeepEqual(bodies[0], want) {
		t.Fatalf("bodies = %v, want [%v]", bodies, want)
	}
	if !strings.Contains(out, `Decided "rename" on run run_1`) {
		t.Fatalf("output = %q", out)
	}
}

func TestRunsShowsAWaitingRunsQuestion(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1": runHandler(waitingRun),
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"runs", "conn_1", "run_1"}) })
	t.Logf("\n%s", out)
	for _, want := range []string{
		"Run run_1 is waiting for a decision:",
		`Which customer is "Lone Star Supply"?`,
		"  Name: Lone Star Supply\n",
		"  Kind: Customer\n",
		"Choices:\n",
		"  use:1   Lone Star Supply - Customer · Austin, TX\n",
		"  use:2   Lone Star Supply Co - Customer · Dallas, TX\n",
		"  use     Another customer…\n",
		"          fields: list_id (record, required) - Customer\n",
		"          fields: city (string, optional) - City\n",
		"                  email (string, required)\n",
		"          Stop here. Nothing is created.\n",
		"Answer with `qube workflows decide conn_1 run_1 --choice ID [--data JSON]`",
		"`qube workflows search conn_1 run_1 --field list_id QUERY`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q", want)
		}
	}
	if strings.Contains(out, `"token"`) || strings.Contains(out, "pt_abc") {
		t.Errorf("the question view shows the prompt token:\n%s", out)
	}
}

// A question with nothing to fill in, and a run that has no search, says only how to answer.
func TestRunsQuestionWithoutFieldsOrSearch(t *testing.T) {
	run := func(string) map[string]interface{} {
		return map[string]interface{}{
			"id": "run_2", "state": "awaiting_input",
			"prompt": map[string]interface{}{
				"message": "Add it again?",
				"choices": []interface{}{
					map[string]interface{}{"id": "add_again", "label": "Add it again", "fields": []interface{}{}},
					map[string]interface{}{"id": "cancel", "label": "Cancel", "fields": []interface{}{}},
				},
			},
		}
	}
	c := newAPITest(t, map[string]http.HandlerFunc{"/api/v2/connections/conn_1/workflow-runs/run_2": runHandler(run)})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"runs", "conn_1", "run_2"}) })
	if !strings.Contains(out, "Answer with `qube workflows decide conn_1 run_2 --choice ID`.\n") {
		t.Errorf("output = %q", out)
	}
	if strings.Contains(out, "search") || strings.Contains(out, "--data") {
		t.Errorf("output mentions search or --data:\n%s", out)
	}
}

// --json and a prompt in the older shape (options only) print the run as JSON, as before.
func TestRunsStaysJSONForJSONAndForOptionsOnlyPrompts(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1": runHandler(waitingRun),
		"/api/v2/connections/conn_1/workflow-runs/run_3": runHandler(func(string) map[string]interface{} {
			return map[string]interface{}{"id": "run_3", "state": "awaiting_input", "prompt": map[string]interface{}{"id": "name_taken", "options": []interface{}{"rename"}}}
		}),
	})
	setJSON(t, true)
	out := captureStdout(t, func() { c.workflows([]string{"runs", "conn_1", "run_1"}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["id"] != "run_1" {
		t.Fatalf("--json output is not the run: %v\n%s", err, out)
	}
	setJSON(t, false)
	out = captureStdout(t, func() { c.workflows([]string{"runs", "conn_1", "run_3"}) })
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["id"] != "run_3" {
		t.Fatalf("an options-only prompt is not JSON: %v\n%s", err, out)
	}
}

// --wait stops at a question and shows it the same way.
func TestWaitStopsAtAQuestionAndShowsIt(t *testing.T) {
	fastWait(t)
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1/decisions": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "run_1", "state": "running"}})
		},
		"/api/v2/connections/conn_1/workflow-runs/run_1": runHandler(waitingRun),
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.workflows([]string{"decide", "conn_1", "run_1", "--choice", "narrow", "--wait"}) })
	for _, want := range []string{"Run run_1 is waiting for a decision:", "Choices:", "--choice ID"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestQuestionTextDropsControlCharacters(t *testing.T) {
	run := map[string]interface{}{
		"id": "run_1",
		"prompt": map[string]interface{}{
			"message": "Pick\x1b[31m one\x07",
			"details": []interface{}{map[string]interface{}{"label": "Name", "value": "Acme\n\x1b]0;owned\x07 Inc"}},
			"choices": []interface{}{map[string]interface{}{"id": "a", "label": "Acme\r\x1b[2J", "fields": []interface{}{}}},
		},
	}
	got := questionText("conn_1", run)
	if strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "  Name: Acme ]0;owned Inc\n") {
		t.Fatalf("details = %q", got)
	}
}

// search: the run is read for its search link; the search route gets the prompt_token the link
// carries, the field and the text; the matches print as a table, and the way to use one goes
// to stderr.
func TestSearchPrintsTheMatches(t *testing.T) {
	var query url.Values
	var searched int
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1": runHandler(waitingRun),
		"/api/v2/connections/conn_1/workflow-runs/run_1/search": func(w http.ResponseWriter, r *http.Request) {
			searched++
			query = r.URL.Query()
			if r.Method != "GET" {
				t.Errorf("method = %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{
				map[string]interface{}{"value": "80000012-1712345678", "label": "Lone Star Supply", "detail": "Customer · Austin, TX"},
				map[string]interface{}{"value": "80000099-1712345999", "label": "Lone Star Supply Co", "detail": nil},
			}})
		},
	})
	setJSON(t, false)
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() { c.workflows([]string{"search", "conn_1", "run_1", "--field", "list_id", "Lone Star"}) })
	})
	t.Logf("\n%s%s", stdout, stderr)
	if searched != 1 || query.Get("field") != "list_id" || query.Get("q") != "Lone Star" || query.Get("prompt_token") != "pt_abc" {
		t.Fatalf("searched %d times with %v", searched, query)
	}
	for _, want := range []string{"VALUE", "LABEL", "DETAIL", "80000012-1712345678", "Lone Star Supply", "Customer · Austin, TX", "80000099-1712345999"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	// the choice is the one that takes the field, and the data is the first match's value
	wantHint := `qube workflows decide conn_1 run_1 --choice use --data '{"list_id":"80000012-1712345678"}'`
	if !strings.Contains(stderr, wantHint) {
		t.Errorf("stderr lacks %q:\n%s", wantHint, stderr)
	}
}

func TestSearchJSONAndNoMatches(t *testing.T) {
	var matches []interface{}
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1": runHandler(waitingRun),
		"/api/v2/connections/conn_1/workflow-runs/run_1/search": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": matches})
		},
	})
	c.connection = "conn_1"

	matches = []interface{}{map[string]interface{}{"value": "v1", "label": "One", "detail": "d"}}
	setJSON(t, true)
	out := captureStdout(t, func() { c.workflows([]string{"search", "run_1", "--field", "list_id", "On"}) })
	var got []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got) != 1 || got[0]["value"] != "v1" {
		t.Fatalf("--json output = %v (%v)", out, err)
	}

	matches = []interface{}{}
	setJSON(t, false)
	out = captureStdout(t, func() { c.workflows([]string{"search", "run_1", "--field", "list_id", "Nobody"}) })
	if !strings.Contains(out, `No list_id matches "Nobody".`) {
		t.Fatalf("output = %q", out)
	}
}

// The API key goes where --host says, whatever host the link a response carries names: the
// search lands on the configured host (the stub), carrying only the link's query.
func TestSearchCallsTheConfiguredHostNotTheLinks(t *testing.T) {
	var searched int
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/workflow-runs/run_1": runHandler(func(string) map[string]interface{} {
			return waitingRun("evil.example")
		}),
		"/api/v2/connections/conn_1/workflow-runs/run_1/search": func(w http.ResponseWriter, r *http.Request) {
			searched++
			if r.URL.Query().Get("prompt_token") != "pt_abc" {
				t.Errorf("query = %v", r.URL.Query())
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": []interface{}{}})
		},
	})
	setJSON(t, false)
	captureStdout(t, func() { c.workflows([]string{"search", "conn_1", "run_1", "--field", "list_id", "Lone"}) })
	if searched != 1 {
		t.Fatalf("searched %d times", searched)
	}
}

func TestSearchLinkSaysWhyARunHasNone(t *testing.T) {
	if link, err := searchLink(waitingRun("h.example")); err != nil || !strings.HasSuffix(link, "/search?prompt_token=pt_abc") {
		t.Fatalf("link = %q, err = %v", link, err)
	}
	if _, err := searchLink(map[string]interface{}{"id": "run_9", "state": "running", "links": map[string]interface{}{}}); err == nil || !strings.Contains(err.Error(), "not waiting at a question") {
		t.Fatalf("a running run: %v", err)
	}
	_, err := searchLink(waitingRun(""))
	if err == nil || !strings.Contains(err.Error(), "record field") || !strings.Contains(err.Error(), "name index") {
		t.Fatalf("a question without a search: %v", err)
	}
}

func TestSearchQueryKeepsTheLinksTokenAndSetsFieldAndText(t *testing.T) {
	q := searchQuery("https://qubesync.com/api/v2/connections/c/workflow-runs/r/search?prompt_token=pt_1", "list_id", "Lone Star")
	want := url.Values{"prompt_token": {"pt_1"}, "field": {"list_id"}, "q": {"Lone Star"}}
	if !reflect.DeepEqual(q, want) {
		t.Fatalf("query = %v, want %v", q, want)
	}
	if q := searchQuery("", "list_id", "x"); q.Get("field") != "list_id" || q.Get("q") != "x" {
		t.Fatalf("no link: %v", q)
	}
}
