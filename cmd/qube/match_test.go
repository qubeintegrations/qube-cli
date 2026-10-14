package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// matchServer answers POST /api/v2/connections/<conn>/matches with `answer`, recording what
// it was sent in *got: the method and path, the raw query and the decoded body.
func matchServer(t *testing.T, conn string, answer map[string]interface{}, got *matchRequest) map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/api/v2/connections/" + conn + "/matches": func(w http.ResponseWriter, r *http.Request) {
			got.route = r.Method + " " + r.URL.Path
			got.query = r.URL.RawQuery
			if err := json.NewDecoder(r.Body).Decode(&got.body); err != nil {
				t.Errorf("body: %v", err)
			}
			_ = json.NewEncoder(w).Encode(answer)
		},
	}
}

type matchRequest struct {
	route, query string
	body         map[string]interface{}
}

var exactAnswer = map[string]interface{}{
	"match": "exact",
	"record": map[string]interface{}{
		"list_id": "80000027-1712345678", "full_name": "Smith:Kitchen", "list": "customer",
		"is_active": true, "score": 1.0, "reasons": []string{"same name"},
	},
	"candidates":                []interface{}{},
	"candidates_total":          0,
	"candidates_total_complete": true,
	"index":                     map[string]interface{}{"state": "ready", "as_of": "2026-10-11T14:02:10Z"},
}

func TestMatchSendsEverythingInTheBody(t *testing.T) {
	var got matchRequest
	c := newAPITest(t, matchServer(t, "conn_1", exactAnswer, &got))
	setJSON(t, true)
	_ = captureStdout(t, func() {
		c.match([]string{"conn_1", "customer", "ACME, Inc.", "--parent", "Smith", "--email", "ap@acme.example",
			"--postal-code", "78701", "--account-number", "crm-1001", "--min-score", "0.5"})
	})
	if got.route != "POST /api/v2/connections/conn_1/matches" || got.query != "" {
		t.Fatalf("server saw %s ?%s", got.route, got.query)
	}
	want := map[string]interface{}{
		"kind": "customer", "name": "ACME, Inc.", "parent": "Smith", "min_score": 0.5,
		"fields": map[string]interface{}{"email": "ap@acme.example", "postal_code": "78701", "account_number": "crm-1001"},
	}
	if !reflect.DeepEqual(got.body, want) {
		t.Fatalf("body = %v\nwant   %v", got.body, want)
	}
}

// Without flags the body is the kind and the name alone, and the connection can be
// --connection (or the default).
func TestMatchSendsOnlyWhatWasGiven(t *testing.T) {
	var got matchRequest
	c := newAPITest(t, matchServer(t, "conn_2", exactAnswer, &got))
	c.connection = "conn_2"
	setJSON(t, true)
	_ = captureStdout(t, func() { c.match([]string{"item", "Consulting", "--exact-only"}) })
	want := map[string]interface{}{"kind": "item", "name": "Consulting", "exact_only": true}
	if got.route != "POST /api/v2/connections/conn_2/matches" || !reflect.DeepEqual(got.body, want) {
		t.Fatalf("server saw %s %v", got.route, got.body)
	}
}

func TestMatchJSONPrintsTheAnswer(t *testing.T) {
	var got matchRequest
	c := newAPITest(t, matchServer(t, "conn_1", exactAnswer, &got))
	setJSON(t, true)
	out := captureStdout(t, func() { c.match([]string{"conn_1", "customer", "Smith:Kitchen"}) })
	var printed map[string]interface{}
	if err := json.Unmarshal([]byte(out), &printed); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if printed["match"] != "exact" || printed["record"].(map[string]interface{})["list_id"] != "80000027-1712345678" {
		t.Fatalf("printed %v", printed)
	}
}

func TestMatchPrintsTheRecord(t *testing.T) {
	var got matchRequest
	c := newAPITest(t, matchServer(t, "conn_1", exactAnswer, &got))
	setJSON(t, false)
	out := captureStdout(t, func() { c.match([]string{"conn_1", "customer", "Smith:Kitchen"}) })
	for _, want := range []string{"Exact:", "80000027-1712345678", "Smith:Kitchen", "1.00", "same name"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestMatchPrintsCandidatesAndHowToNarrowThem(t *testing.T) {
	answer := map[string]interface{}{
		"match":  "candidates",
		"record": nil,
		"candidates": []interface{}{
			map[string]interface{}{"list_id": "80000011-1712345678", "full_name": "Acme Inc", "list": "customer", "is_active": true, "score": 1.0, "reasons": []string{"same after removing punctuation and suffixes"}},
			map[string]interface{}{"list_id": "80000012-1712345678", "full_name": "Acme LLC", "list": "customer", "is_active": false, "score": 0.875, "reasons": []string{"similar name", "same city"}},
		},
		"candidates_total":          50,
		"candidates_total_complete": false,
		"index":                     map[string]interface{}{"state": "ready", "as_of": "2026-10-11T14:02:10Z"},
	}
	var got matchRequest
	c := newAPITest(t, matchServer(t, "conn_1", answer, &got))
	setJSON(t, false)
	out := captureStdout(t, func() { c.match([]string{"conn_1", "customer", "Acme"}) })
	lines := strings.Split(out, "\n")
	if !strings.HasPrefix(lines[0], "Candidates") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "Acme LLC") || !strings.Contains(out, "0.88") || !strings.Contains(out, "similar name; same city") {
		t.Fatalf("output:\n%s", out)
	}
	for _, l := range lines {
		if strings.Contains(l, "Acme LLC") && !strings.Contains(l, " no ") {
			t.Fatalf("an inactive candidate should say so: %q", l)
		}
	}
	if !strings.Contains(out, "At least 50 qualified; these are the first 2.") {
		t.Fatalf("output:\n%s", out)
	}
}

// A match changes nothing: a read-only session in a production app runs it without --yes,
// even under --json (where a write would be refused for want of one).
func TestMatchIsARead(t *testing.T) {
	var got matchRequest
	c := newAPITestApp(t, false, matchServer(t, "conn_1", exactAnswer, &got))
	s := c.cfg.Sessions[c.host]
	s.Access = "read_only"
	c.cfg.Sessions[c.host] = s
	setJSON(t, true)
	_ = captureStdout(t, func() { c.match([]string{"conn_1", "customer", "Smith:Kitchen"}) })
	if got.route != "POST /api/v2/connections/conn_1/matches" {
		t.Fatalf("server saw %q", got.route)
	}
}

// While the index builds, a match fails (exit 1) with the server's detail and how to hurry it.
func TestMatchWhileTheIndexBuilds(t *testing.T) {
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1/matches": func(w http.ResponseWriter, r *http.Request) {
			_, _ = ioutil.ReadAll(r.Body)
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"errors": map[string]interface{}{
				"detail": "This call started building the connection's name index.",
				"code":   "name_index_building",
			}})
		},
	})
	exit, out := matchInChild(t, c.host, "conn_1", "customer", "Acme")
	if exit != ui.ExitFailure {
		t.Fatalf("want exit %d, got %d\n%s", ui.ExitFailure, exit, out)
	}
	for _, want := range []string{"HTTP 409 name_index_building: This call started building", "qube connections show conn_1", "qube simulator sync conn_1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

// A connection is an id, never a kind: `customer Acme Inc` is a name without its quotes, and
// is refused (exit 2) before anything is sent, saying so.
func TestMatchAsksForQuotes(t *testing.T) {
	var sent []string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/": func(w http.ResponseWriter, r *http.Request) { sent = append(sent, r.Method+" "+r.URL.Path) },
	})
	exit, out := matchInChild(t, c.host, "customer", "Acme", "Inc")
	if exit != ui.ExitUsage || len(sent) > 0 {
		t.Fatalf("want exit %d and nothing sent; got %d, sent %v\n%s", ui.ExitUsage, exit, sent, out)
	}
	if !strings.Contains(out, `qube match customer "Acme Inc"`) {
		t.Fatalf("output = %s", out)
	}
}

// matchInChild runs `qube match args...` in a child of this test binary pointed at host, as a
// refusal exits the process, and returns its exit code and output.
func matchInChild(t *testing.T, host string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMatchChild$")
	cmd.Env = append(os.Environ(), "QUBE_TEST_MATCH_HOST="+host, "QUBE_TEST_MATCH_ARGS="+strings.Join(args, "\x1f"))
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	}
	if err != nil {
		t.Fatal(err)
	}
	return 0, string(out)
}

// TestMatchChild is matchInChild's child; on its own it does nothing.
func TestMatchChild(t *testing.T) {
	host := os.Getenv("QUBE_TEST_MATCH_HOST")
	if host == "" {
		t.Skip("run by matchInChild")
	}
	c := &ctx{host: host, cfg: &config.File{Sessions: map[string]config.Session{host: {Token: "qct_x"}}}, bg: context.Background()}
	c.match(strings.Split(os.Getenv("QUBE_TEST_MATCH_ARGS"), "\x1f"))
	os.Exit(0)
}

func TestNameIndexSummary(t *testing.T) {
	cases := []struct {
		index map[string]interface{}
		want  string
	}{
		{map[string]interface{}{"state": "ready", "records": float64(3412), "as_of": "2026-10-11T14:02:10Z"}, "ready (3412 records, as of 2026-10-11T14:02:10Z)"},
		{map[string]interface{}{"state": "building", "records": float64(800), "as_of": nil}, "building (800 records)"},
		{map[string]interface{}{"state": "not_built", "records": nil, "as_of": nil}, "not_built"},
	}
	for _, tc := range cases {
		if got := nameIndexSummary(tc.index); got != tc.want {
			t.Errorf("got %q, want %q", got, tc.want)
		}
	}
}

// matchKinds are the match body's `kind` enum, when a qube checkout sits beside this one (or
// QUBE_REPO names one); CI has none and skips this.
func TestMatchKindsMatchSpec(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(qubeRepo(), "priv", "openapi", "connections.json"))
	if err != nil {
		t.Skipf("no qube checkout (set QUBE_REPO): %v", err)
	}
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Properties map[string]struct {
							Enum []string `json:"enum"`
						} `json:"properties"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"requestBody"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	spec := doc.Paths["/connections/{connection_id}/matches"]["post"].RequestBody.Content["application/json"].Schema.Properties["kind"].Enum
	if len(spec) == 0 {
		t.Fatal("the spec has no kind enum for POST /connections/{connection_id}/matches")
	}
	got, want := append([]string(nil), matchKinds...), append([]string(nil), spec...)
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("matchKinds (match.go) = %v\nthe spec's kinds = %v", matchKinds, spec)
	}
}
