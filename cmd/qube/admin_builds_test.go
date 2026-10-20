package main

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// The "build it free" commands (admin_builds.go) against stubs of the staff routes they call:
// what each sends, what each prints, and how each fails.

const (
	reqID   = "9b2f0c1e-2222-4000-8000-000000000001"
	buildID = "b1d0c1e0-3333-4000-8000-000000000001"
)

var (
	reqPath   = "/api/admin/integration-requests/" + reqID
	buildBase = "/api/admin/builds/" + buildID
)

// shortPolls makes --wait read the build every millisecond.
func shortPolls(t *testing.T) {
	t.Helper()
	old := buildPollEvery
	buildPollEvery = time.Millisecond
	t.Cleanup(func() { buildPollEvery = old })
}

var buildStages = []string{"plan", "install", "test_company", "code", "video", "deliver"}

// scenario is one entry of the test company stage's output, as the server's worker writes it.
func scenario(name string, attempt int, result string, extra map[string]interface{}) map[string]interface{} {
	e := map[string]interface{}{"name": name, "reference": "b1-" + name, "attempt": attempt, "result": result,
		"run_id": "run-" + name, "state": "running", "outcome": nil, "answers": 0}
	switch result {
	case "passed":
		e["state"], e["outcome"] = "completed", "added"
	case "failed":
		e["state"], e["outcome"] = "failed", "refused"
	}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

// fakeBuild is a build as GET /builds/:id answers it: the stages named in states (the rest
// pending), the test company's scenarios in its output, and the first stage that isn't done as
// `next`. Its manifest names three scenarios.
func fakeBuild(states map[string]string, scenarios []map[string]interface{}) map[string]interface{} {
	// lists are []interface{}, as the JSON decoder makes them: the commands read builds so
	var stages []interface{}
	brief := map[string]string{}
	next := map[string]interface{}{"stage": nil, "ready": false, "waiting_for_gate": nil}
	for _, name := range buildStages {
		state := states[name]
		if state == "" {
			state = "pending"
		}
		brief[name] = state
		stage := map[string]interface{}{"stage": name, "state": state, "started_at": nil, "finished_at": nil, "error": nil, "output": map[string]interface{}{}}
		if state != "pending" {
			stage["started_at"] = "2026-10-18T10:00:00Z"
		}
		if state == "done" || state == "failed" {
			stage["finished_at"] = "2026-10-18T10:20:00Z"
		}
		if name == "test_company" && scenarios != nil {
			entries := []interface{}{}
			for _, e := range scenarios {
				entries = append(entries, e)
			}
			stage["output"] = map[string]interface{}{"test_company_id": "conn-1", "seeded": map[string]interface{}{"added": 7, "present": 0}, "scenarios": entries}
		}
		stages = append(stages, stage)
		if state != "done" && next["stage"] == nil {
			next["stage"] = name
			next["ready"] = true
			if name == "install" && states["plan"] == "done" {
				next["ready"], next["waiting_for_gate"] = false, "plan"
			}
		}
	}
	return map[string]interface{}{
		"id": buildID, "integration_request_id": reqID, "number": 1, "states": brief, "next": next, "has_manifest": true,
		"created_at": "2026-10-18T09:00:00Z", "organization_id": acmeID, "stages": stages,
		"approvals": []map[string]interface{}{
			{"gate": "plan", "approved": true, "approved_by": map[string]string{"id": "u1", "email": "garrett@qubesync.com"}, "approved_at": "2026-10-18T10:05:00Z", "note": "Looks right"},
			{"gate": "code", "approved": false, "approved_by": nil, "approved_at": nil, "note": nil},
			{"gate": "video", "approved": false, "approved_by": nil, "approved_at": nil, "note": nil},
		},
		"manifest": map[string]interface{}{
			"who":        map[string]interface{}{"company_name": "Juniper Lane Landscaping"},
			"workflows":  []interface{}{map[string]interface{}{"key": "record_payment", "source": "template", "template": "record_payment"}},
			"operations": []interface{}{map[string]interface{}{"name": "RecordPayment", "workflow": "record_payment"}},
			"scenarios":  []interface{}{map[string]interface{}{"name": "contract-1043"}, map[string]interface{}{"name": "contract-1044"}, map[string]interface{}{"name": "contract-1045"}},
		},
		"test_company_id": nil, "updated_at": "2026-10-18T10:20:00Z",
	}
}

func dataOf(v interface{}) map[string]interface{} { return map[string]interface{}{"data": v} }

func requestDetail() map[string]interface{} {
	return map[string]interface{}{
		"id": reqID, "status": "building", "starting_point": "describe", "company_name": "Juniper Lane Landscaping",
		"organization": map[string]string{"id": acmeID, "name": "Juniper Lane"}, "user": map[string]string{"id": "u2", "email": "dana@juniper.example"},
		"app_id": "app-9", "submitted_at": "2026-10-17T15:02:00Z", "delivered_at": nil, "created_at": "2026-10-17T15:00:00Z",
		"company_url": "https://juniper.example", "company_description": "We mow lawns.",
		"stack":       map[string]interface{}{"language": "Ruby", "framework": "Rails 7", "http_library": nil},
		"description": "Each job becomes an invoice.\nPayments come back.",
		"answers":     map[string]interface{}{"directions": []string{"to_quickbooks", "from_quickbooks"}, "records": []string{"invoices"}, "other_records": nil, "triggers": []string{"on_change"}, "matching": "name"},
		"notes":       nil,
		"kit": map[string]interface{}{"stack": "rails", "http": "faraday", "mapping_out": "inv = Invoice.new\ninv.save\n", "mapping_in": nil,
			"code_for": "Ruby on Rails, Faraday", "code_for_source": "from the stack they gave"},
		"app":          map[string]interface{}{"id": "app-9", "name": "My App Dev", "sandbox": true},
		"latest_build": fakeBuild(map[string]string{"plan": "done"}, nil),
	}
}

// ---------------------------------------------------------------- integration requests

func TestAdminIntegrationRequestsList(t *testing.T) {
	var q url.Values
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/integration-requests": func(w http.ResponseWriter, r *http.Request) {
			q = r.URL.Query()
			brief := requestDetail()
			writeJSON(w, 200, map[string]interface{}{"data": []interface{}{brief, map[string]interface{}{
				"id": "other", "status": "submitted", "company_name": "Newer Co", "organization": nil, "latest_build": nil, "submitted_at": "2026-10-18T08:00:00Z"}},
				"meta": map[string]interface{}{"page": 1, "page_size": 25, "has_next_page": true, "total_count": 30}})
		},
	})
	setJSON(t, false)
	var out string
	note := captureStderr(t, func() {
		out = captureStdout(t, func() {
			c.adminIntegrationRequests(c.staffClient(), []string{"--status", "building", "--page", "2", "--page-size", "10"})
		})
	})
	if q.Get("status") != "building" || q.Get("page") != "2" || q.Get("page_size") != "10" {
		t.Fatalf("query = %v", q)
	}
	for _, want := range []string{"ID", "COMPANY", reqID, "Juniper Lane Landscaping", "#1 install pending, waits for the plan gate", "Newer Co"} {
		if !strings.Contains(out, want) {
			t.Errorf("table lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(note, "(more: --page 2)") {
		t.Errorf("no page note: %q", note)
	}
	setJSON(t, true)
	out = captureStdout(t, func() { c.adminIntegrationRequests(c.staffClient(), nil) })
	var got struct {
		Data []map[string]interface{} `json:"data"`
		Meta map[string]interface{}   `json:"meta"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len(got.Data) != 2 || got.Meta["total_count"] != float64(30) {
		t.Fatalf("json: %v\n%s", err, out)
	}
}

func TestAdminIntegrationRequestShow(t *testing.T) {
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath: func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, dataOf(requestDetail())) },
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID}) })
	for _, want := range []string{"Juniper Lane Landscaping · building · request " + reqID, "dana@juniper.example", "Ruby · Rails 7",
		"My App Dev (sandbox app app-9)", "code for Ruby on Rails, Faraday (from the stack they gave), stack rails, http faraday, mapping out 2 lines, mapping in none",
		"#1 (" + buildID + ") next: install pending, waits for the plan gate",
		"Description:\n  Each job becomes an invoice.\n  Payments come back.", "directions: to_quickbooks, from_quickbooks", "matching: name"} {
		if !strings.Contains(out, want) {
			t.Errorf("show lacks %q:\n%s", want, out)
		}
	}
	setJSON(t, true)
	out = captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["id"] != reqID || got["description"] == nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
}

func TestAdminIntegrationRequestSetApp(t *testing.T) {
	var method string
	var body map[string]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath + "/app": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, 200, dataOf(map[string]interface{}{"id": reqID, "app": map[string]interface{}{"id": "app-9", "name": "My App Dev", "sandbox": true}}))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID, "set-app", "app-9"}) })
	if method != "PATCH" || !reflect.DeepEqual(body, map[string]interface{}{"app_id": "app-9"}) {
		t.Fatalf("%s %v", method, body)
	}
	if !strings.Contains(out, "Request "+reqID+" now builds in My App Dev (sandbox app app-9).") {
		t.Fatalf("output: %s", out)
	}
}

func TestAdminIntegrationRequestBuildsList(t *testing.T) {
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath + "/builds": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, dataOf([]interface{}{fakeBuild(map[string]string{"plan": "done", "install": "done", "test_company": "running"}, nil)}))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID, "builds"}) })
	for _, want := range []string{"BUILD", buildID, "test_company running", "saved"} {
		if !strings.Contains(out, want) {
			t.Errorf("builds lacks %q:\n%s", want, out)
		}
	}
}

// ---------------------------------------------------------------- the kit

func TestAdminKitShowAndSet(t *testing.T) {
	var method string
	var body map[string]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath: func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, dataOf(requestDetail())) },
		reqPath + "/kit": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, 200, dataOf(map[string]interface{}{"id": reqID, "stack": "rails", "http": "faraday", "mapping_out": body["kit_mapping_out"], "mapping_in": nil,
				"code_for": "Ruby on Rails, Faraday", "code_for_source": "the kit's stack"}))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID, "kit"}) })
	for _, want := range []string{"Code for:  Ruby on Rails, Faraday", "Stack:     rails", "HTTP:      faraday", "Mapping out (2 lines):\n  inv = Invoice.new\n  inv.save", "Mapping in: none"} {
		if !strings.Contains(out, want) {
			t.Errorf("kit lacks %q:\n%s", want, out)
		}
	}
	setJSON(t, true)
	out = captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID, "kit"}) })
	var kit map[string]interface{}
	if err := json.Unmarshal([]byte(out), &kit); err != nil || kit["code_for"] != "Ruby on Rails, Faraday" || kit["id"] != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}

	// only the fields given go, the file's text as it is; an empty value clears
	dir := t.TempDir()
	mapping := filepath.Join(dir, "out.rb")
	text := "invoice = Invoice.new(number: 1)\ninvoice.save!\n"
	if err := os.WriteFile(mapping, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	setJSON(t, false)
	out = captureStdout(t, func() {
		c.adminIntegrationRequest(c.staffClient(), []string{reqID, "kit", "set", "--stack", "rails", "--mapping-out", mapping})
	})
	if method != "PATCH" || !reflect.DeepEqual(body, map[string]interface{}{"kit_stack": "rails", "kit_mapping_out": text}) {
		t.Fatalf("%s %v", method, body)
	}
	if !strings.Contains(out, "Saved the kit of request "+reqID+": code for Ruby on Rails, Faraday (the kit's stack), stack rails, http faraday, mapping out 2 lines, mapping in none.") {
		t.Fatalf("output: %s", out)
	}
	_ = captureStdout(t, func() { c.adminKitSet(c.staffClient(), reqID, []string{"--http", "", "--mapping-in", ""}) })
	if !reflect.DeepEqual(body, map[string]interface{}{"kit_http": "", "kit_mapping_in": ""}) {
		t.Fatalf("clearing sent %v", body)
	}
}

func TestAdminKitSetWithNothingToSet(t *testing.T) {
	if inChild(t, func(c *ctx) { c.adminKitSet(c.staffClient(), reqID, nil) }) {
		return
	}
	c := staffCtx(t, true, nil)
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, "nothing to set") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A 422 prints the server's message and then each problem as `path: message`; under --json the
// server's body is on stdout.
func TestAdminKitSetPrintsEveryProblem(t *testing.T) {
	if inChild(t, func(c *ctx) { c.adminKitSet(c.staffClient(), reqID, []string{"--http", "faraday"}) }) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath + "/kit": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 422, map[string]interface{}{"error": map[string]interface{}{"code": "invalid_kit", "message": "The kit's fields have problems; each is in errors.",
				"errors": []map[string]string{{"path": "kit_http", "message": "pick a stack it goes with first"}, {"path": "kit_mapping_out", "message": "line 14 is 97 columns; the kit's code box holds 92"}}}})
		},
	})
	code, out := runChild(t, c)
	if code != ui.ExitFailure {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"HTTP 422 invalid_kit: The kit's fields have problems", "  kit_http: pick a stack it goes with first", "  kit_mapping_out: line 14 is 97 columns; the kit's code box holds 92"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	setJSON(t, true)
	code, out = runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, `"code":"invalid_kit"`) {
		t.Fatalf("json: exit %d:\n%s", code, out)
	}
}

func kitFilesHandler(files []map[string]interface{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, dataOf(map[string]interface{}{"id": reqID, "stack": "rails", "http": "faraday", "code_for": "Ruby on Rails, Faraday",
			"generated": true, "fallback": nil, "mapping_check_stack": "rails", "width": 92,
			"operations": []map[string]interface{}{{"name": "CreateInvoice", "workflow": "add_transaction", "slug": "create_invoice", "record": "invoice", "kind": "invoice.create",
				"from_plan": true, "path": "app/qube/create_invoice.rb"}},
			"files": files}))
	}
}

func kitFilesFixture() []map[string]interface{} {
	return []map[string]interface{}{
		{"path": "app/services/qube_sync.rb", "app_path": "app/services/qube_sync.rb", "language": "ruby", "section": "The wrapper", "content": "class QubeSync\nend\n"},
		{"path": "app/qube/create_invoice.rb", "app_path": "app/qube/create_invoice.rb", "language": "ruby", "section": "Operations", "content": "class CreateInvoice\nend\n"},
		{"path": "routes/webhooks.rb", "app_path": "config/routes.rb", "language": "ruby", "section": "Webhooks", "content": "post 'qube'\n"},
		{"path": "README.md", "app_path": "README.md", "language": "markdown", "section": "Read me", "content": "# Kit\n"},
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The files are written under --out, directories and all; an existing file stops the whole
// write until --force; and a refused write leaves what was there.
func TestAdminKitFilesWritesUnderOut(t *testing.T) {
	out := filepath.Join(t.TempDir(), "kit")
	if inChild(t, func(c *ctx) {
		c.adminKitFiles(c.staffClient(), reqID, []string{"--out", os.Getenv("KIT_OUT")})
	}) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{reqPath + "/kit/files": kitFilesHandler(kitFilesFixture())})
	setJSON(t, false)
	text := captureStdout(t, func() { c.adminIntegrationRequest(c.staffClient(), []string{reqID, "kit", "files", "--out", out}) })
	if got := readFile(t, filepath.Join(out, "app", "services", "qube_sync.rb")); got != "class QubeSync\nend\n" {
		t.Fatalf("content = %q", got)
	}
	if readFile(t, filepath.Join(out, "README.md")) != "# Kit\n" || readFile(t, filepath.Join(out, "app", "qube", "create_invoice.rb")) != "class CreateInvoice\nend\n" {
		t.Fatal("a file is missing or wrong")
	}
	for _, want := range []string{"Wrote 4 files to " + out + " (code for Ruby on Rails, Faraday)", "app/services/qube_sync.rb", "routes/webhooks.rb", "config/routes.rb", "ruby", "CreateInvoice",
		"invoice.create", "app/qube/create_invoice.rb", "the plan", "A mapping line is at most 92 columns wide (bin/kit-mapping-check --stack rails)."} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}

	// A file in the way stops the whole write, and leaves it as it was.
	if err := os.WriteFile(filepath.Join(out, "README.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{filepath.Join("app", "qube", "create_invoice.rb"), filepath.Join("app", "services", "qube_sync.rb"), filepath.Join("routes", "webhooks.rb")} {
		if err := os.Remove(filepath.Join(out, gone)); err != nil {
			t.Fatal(err)
		}
	}
	code, msg := runChild(t, c, "KIT_OUT="+out)
	if code != ui.ExitFailure || !strings.Contains(msg, "already exists, so nothing was written (--force overwrites)") || !strings.Contains(msg, filepath.Join(out, "README.md")) {
		t.Fatalf("exit %d:\n%s", code, msg)
	}
	if readFile(t, filepath.Join(out, "README.md")) != "mine\n" {
		t.Fatal("a file was overwritten without --force")
	}
	for _, left := range []string{filepath.Join("app", "qube", "create_invoice.rb"), filepath.Join("app", "services", "qube_sync.rb"), filepath.Join("routes", "webhooks.rb")} {
		if _, err := os.Stat(filepath.Join(out, left)); err == nil {
			t.Fatalf("the write went ahead past the conflict: %s", left)
		}
	}

	// --force overwrites.
	_ = captureStdout(t, func() { c.adminKitFiles(c.staffClient(), reqID, []string{"--out", out, "--force"}) })
	if readFile(t, filepath.Join(out, "README.md")) != "# Kit\n" || readFile(t, filepath.Join(out, "app", "qube", "create_invoice.rb")) == "" {
		t.Fatal("--force didn't overwrite")
	}

	// --json names the files, never their content.
	setJSON(t, true)
	js := captureStdout(t, func() { c.adminKitFiles(c.staffClient(), reqID, []string{"--out", out, "--force"}) })
	var got struct {
		Out        string                   `json:"out"`
		Generated  bool                     `json:"generated"`
		Width      int                      `json:"width"`
		CheckStack string                   `json:"mapping_check_stack"`
		Files      []map[string]interface{} `json:"files"`
		Ops        []map[string]interface{} `json:"operations"`
	}
	if err := json.Unmarshal([]byte(js), &got); err != nil || got.Out != out || len(got.Files) != 4 || got.Files[2]["app_path"] != "config/routes.rb" || got.Generated != true || got.Width != 92 || got.CheckStack != "rails" || got.Files[0]["bytes"] != float64(len("class QubeSync\nend\n")) || len(got.Ops) != 1 || strings.Contains(js, "class QubeSync") {
		t.Fatalf("json: %v\n%s", err, js)
	}
}

// A stack the kit writes no code for has no files: it says so and writes (and creates) nothing.
func TestAdminKitFilesForAStackWithoutCode(t *testing.T) {
	out := filepath.Join(t.TempDir(), "kit")
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath + "/kit/files": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, dataOf(map[string]interface{}{"id": reqID, "stack": nil, "http": nil, "code_for": "Go", "generated": false,
				"fallback": "The kit writes code for Rails, Node, TypeScript and Python; for Go, use the API reference.", "mapping_check_stack": nil, "width": 92,
				"operations": []interface{}{}, "files": []interface{}{}}))
		},
	})
	setJSON(t, false)
	text := captureStdout(t, func() { c.adminKitFiles(c.staffClient(), reqID, []string{"--out", out}) })
	if !strings.Contains(text, "The kit has no files for Go; nothing was written.") || !strings.Contains(text, "for Go, use the API reference.") {
		t.Fatalf("output: %s", text)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("the directory was created for nothing")
	}
	setJSON(t, true)
	js := captureStdout(t, func() { c.adminKitFiles(c.staffClient(), reqID, []string{"--out", out}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(js), &got); err != nil || got["generated"] != false || got["fallback"] == "" {
		t.Fatalf("json: %v\n%s", err, js)
	}
}

func TestAdminKitFilesNeedsOut(t *testing.T) {
	if inChild(t, func(c *ctx) { c.adminKitFiles(c.staffClient(), reqID, nil) }) {
		return
	}
	c := staffCtx(t, true, nil)
	code, out := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(out, "kit files --out DIR [--force]") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A path the server sends that is absolute, or has .., is refused, and refused before anything
// is written: the good file beside it isn't written either.
func TestSafeKitPath(t *testing.T) {
	for _, p := range []string{"app/qube.rb", "./app/qube.rb", "a/b/c.rb", "dir//file.rb", "README.md", `app\qube.rb`} {
		if got, err := safeKitPath(p); err != nil || strings.Contains(got, "..") || filepath.IsAbs(got) {
			t.Errorf("%q: got %q, %v", p, got, err)
		}
	}
	for _, p := range []string{"/etc/passwd", "/", `\windows\system32\x`, "C:/x.rb", `C:\x.rb`, "c:x.rb", "../x.rb", "a/../../x.rb", `a\..\..\x.rb`, "a/../x.rb", "..", "a/..", "", "  ", ".", "./", "x\x00y"} {
		if got, err := safeKitPath(p); err == nil {
			t.Errorf("%q was accepted as %q", p, got)
		}
	}
}

func TestWriteKitFilesWritesNothingForABadPath(t *testing.T) {
	for _, bad := range []string{"/tmp/escape.rb", "../escape.rb", "a/../../escape.rb", `..\escape.rb`, "C:/escape.rb"} {
		parent := t.TempDir()
		dir := filepath.Join(parent, "kit")
		_, err := writeKitFiles(dir, []kitFile{{Path: "good.rb", Content: "x"}, {Path: bad, Content: "y"}}, true)
		if err == nil || !strings.Contains(err.Error(), "nothing was written") {
			t.Errorf("%q: err = %v", bad, err)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("%q: %s was created", bad, dir)
		}
		if _, err := os.Stat(filepath.Join(parent, "escape.rb")); err == nil {
			t.Errorf("%q: a file escaped", bad)
		}
	}
}

func TestAdminKitFilesRefusesAnUnsafePathThroughTheCommand(t *testing.T) {
	out := filepath.Join(t.TempDir(), "kit")
	if inChild(t, func(c *ctx) {
		c.adminKitFiles(c.staffClient(), reqID, []string{"--out", os.Getenv("KIT_OUT"), "--force"})
	}) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{reqPath + "/kit/files": kitFilesHandler([]map[string]interface{}{
		{"path": "ok.rb", "language": "ruby", "section": "s", "content": "x"},
		{"path": "../../escape.rb", "language": "ruby", "section": "s", "content": "y"},
	})})
	code, msg := runChild(t, c, "KIT_OUT="+out)
	if code != ui.ExitFailure || !strings.Contains(msg, `"../../escape.rb" goes up with ..`) || !strings.Contains(msg, "nothing was written") {
		t.Fatalf("exit %d:\n%s", code, msg)
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("something was written")
	}
}

func TestWriteKitFilesStaysInsideThroughLinksAndRefusesDuplicates(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	parent := t.TempDir()
	dir, outside := filepath.Join(parent, "kit"), filepath.Join(parent, "outside")
	for _, d := range []string{dir, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// a directory in the output that is a link out
	if err := os.Symlink(outside, filepath.Join(dir, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := writeKitFiles(dir, []kitFile{{Path: "sub/x.rb", Content: "x"}}, false); err == nil || !strings.Contains(err.Error(), "leads outside") {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "x.rb")); err == nil {
		t.Fatal("a file was written through the link")
	}
	// a file that is a link, with and without --force
	target := filepath.Join(outside, "target.rb")
	if err := os.WriteFile(target, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "link.rb")); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		if _, err := writeKitFiles(dir, []kitFile{{Path: "link.rb", Content: "x"}}, force); err == nil || !strings.Contains(err.Error(), "is a link") {
			t.Fatalf("force %v: err = %v", force, err)
		}
	}
	if readFile(t, target) != "keep" {
		t.Fatal("the link's target was written")
	}
	// a directory where a file goes, and one path twice (case-insensitively, as a Mac disk sees it)
	if err := os.MkdirAll(filepath.Join(dir, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writeKitFiles(dir, []kitFile{{Path: "adir", Content: "x"}}, true); err == nil || !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v", err)
	}
	if _, err := writeKitFiles(dir, []kitFile{{Path: "A.rb", Content: "x"}, {Path: "a.rb", Content: "y"}}, true); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("err = %v", err)
	}
}

// ---------------------------------------------------------------- builds

func TestAdminBuildsStart(t *testing.T) {
	if inChild(t, func(c *ctx) { c.adminBuilds(c.staffClient(), []string{"begin"}) }) {
		return
	}
	var method string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath + "/builds": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			writeJSON(w, 201, dataOf(fakeBuild(nil, nil)))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuilds(c.staffClient(), []string{"start", reqID}) })
	if method != "POST" || !strings.Contains(out, "Started build 1 of request "+reqID+" (build "+buildID+"). Next: plan pending.") {
		t.Fatalf("%s: %s", method, out)
	}
	code, msg := runChild(t, c)
	if code != ui.ExitUsage || !strings.Contains(msg, "usage: qube admin builds start REQUEST_ID") {
		t.Fatalf("exit %d:\n%s", code, msg)
	}
}

func TestAdminBuildShowsStagesGatesAndScenarios(t *testing.T) {
	build := fakeBuild(map[string]string{"plan": "done", "install": "done", "test_company": "failed"},
		[]map[string]interface{}{scenario("contract-1043", 1, "passed", nil), scenario("contract-1044", 1, "failed", map[string]interface{}{"message": "ended failed/refused, wanted completed/added"})})
	stageEntry(build, "test_company")["error"] = "Scenario “contract-1044” (run run-contract-1044) didn't pass"
	c := staffCtx(t, true, map[string]http.HandlerFunc{buildBase: func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, dataOf(build)) }})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID}) })
	for _, want := range []string{"Build 1 of request " + reqID + " (build " + buildID + ")", "Next: test_company failed",
		"STAGE", "plan", "done", "error: Scenario “contract-1044”", "GATE", "approved by garrett@qubesync.com", "Looks right", "code", "not approved",
		"Test company: 1 of 3 scenarios passed", "contract-1043", "passed", "completed/added", "run-contract-1043",
		"contract-1044", "failed", "failed/refused", "ended failed/refused, wanted completed/added", "contract-1045", "not started"} {
		if !strings.Contains(out, want) {
			t.Errorf("build lacks %q:\n%s", want, out)
		}
	}
	setJSON(t, true)
	out = captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["id"] != buildID || got["manifest"] == nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
}

func TestAdminBuildManifest(t *testing.T) {
	if inChild(t, func(c *ctx) { c.adminBuild(c.staffClient(), []string{buildID, "manifest"}) }) {
		return
	}
	build := fakeBuild(nil, nil)
	var mu sync.Mutex
	c := staffCtx(t, true, map[string]http.HandlerFunc{buildBase: func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, 200, dataOf(build))
	}})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID, "manifest"}) })
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["who"] == nil || got["id"] != nil || got["stages"] != nil {
		t.Fatalf("the manifest alone: %v\n%s", err, out)
	}
	// a build with no manifest has none to print
	mu.Lock()
	build["manifest"] = nil
	mu.Unlock()
	code, msg := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(msg, "has no manifest yet") {
		t.Fatalf("exit %d:\n%s", code, msg)
	}
}

func TestAdminManifestSetSendsTheFileAsWritten(t *testing.T) {
	var method, contentType string
	var body []byte
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		buildBase + "/manifest": func(w http.ResponseWriter, r *http.Request) {
			method, contentType = r.Method, r.Header.Get("Content-Type")
			body, _ = io.ReadAll(r.Body)
			writeJSON(w, 200, dataOf(fakeBuild(nil, nil)))
		},
	})
	file := filepath.Join(t.TempDir(), "manifest.json")
	text := "{\n  \"who\": {\"company_name\": \"Juniper Lane\"},\n  \"big\": 12345678901234567890\n}\n"
	if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID, "manifest", "set", file}) })
	if method != "PUT" || contentType != "application/json" || string(body) != text {
		t.Fatalf("%s %s %q", method, contentType, body)
	}
	if !strings.Contains(out, "Saved the manifest of build 1: 1 workflow, 1 operation, 3 scenarios. Next: plan pending.") {
		t.Fatalf("output: %s", out)
	}

	// `-` reads stdin
	in, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	old := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = old }()
	body = nil
	_ = captureStdout(t, func() { c.adminManifestSet(c.staffClient(), buildID, []string{"-"}) })
	if string(body) != text {
		t.Fatalf("stdin body = %q", body)
	}
}

func TestAdminManifestSetPrintsEveryProblem(t *testing.T) {
	file := os.Getenv("MANIFEST_FILE")
	if inChild(t, func(c *ctx) { c.adminManifestSet(c.staffClient(), buildID, []string{file}) }) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		buildBase + "/manifest": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 422, map[string]interface{}{"error": map[string]interface{}{"code": "invalid_manifest", "message": "The manifest has 3 problems; each is in errors, at its path.",
				"errors": []map[string]string{{"path": "who.company_name", "message": "can't be blank"}, {"path": "operations.0.name", "message": "isn't a class name"},
					{"path": "operations.0.workflow", "message": "isn't one of the manifest's workflows"}}}})
		},
	})
	good := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(good, []byte(`{"who": {}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out := runChild(t, c, "MANIFEST_FILE="+good)
	if code != ui.ExitFailure {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{"HTTP 422 invalid_manifest: The manifest has 3 problems", "  who.company_name: can't be blank", "  operations.0.name: isn't a class name", "  operations.0.workflow: isn't one of the manifest's workflows"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	// a file that isn't a JSON object never reaches the server
	for name, content := range map[string]string{"array": `[1, 2]`, "empty": ``, "broken": `{"who": `} {
		bad := filepath.Join(t.TempDir(), name+".json")
		if err := os.WriteFile(bad, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		code, out = runChild(t, c, "MANIFEST_FILE="+bad)
		if code != ui.ExitFailure || !strings.Contains(out, name+".json: ") || strings.Contains(out, "HTTP ") {
			t.Errorf("%s: exit %d:\n%s", name, code, out)
		}
	}
}

func TestAdminBuildStageBodies(t *testing.T) {
	var path string
	var body map[string]interface{}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/builds/" + buildID + "/stages/": func(w http.ResponseWriter, r *http.Request) {
			path = r.URL.Path
			body = nil
			_ = json.NewDecoder(r.Body).Decode(&body)
			writeJSON(w, 200, dataOf(fakeBuild(map[string]string{"plan": "running"}, nil)))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID, "stage", "plan", "running"}) })
	if path != buildBase+"/stages/plan" || !reflect.DeepEqual(body, map[string]interface{}{"state": "running"}) {
		t.Fatalf("%s %v", path, body)
	}
	if !strings.Contains(out, "Build 1: plan is now running. Next: plan running.") {
		t.Fatalf("output: %s", out)
	}
	// the command's spelling of the stage, an error and an output
	_ = captureStdout(t, func() {
		c.adminBuild(c.staffClient(), []string{buildID, "stage", "test-company", "failed", "--error", "boom", "--output", `{"scenarios": 3}`})
	})
	if path != buildBase+"/stages/test_company" || !reflect.DeepEqual(body, map[string]interface{}{"state": "failed", "error": "boom", "output": map[string]interface{}{"scenarios": float64(3)}}) {
		t.Fatalf("%s %v", path, body)
	}
	file := filepath.Join(t.TempDir(), "output.json")
	if err := os.WriteFile(file, []byte(`{"installed": ["record_payment"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = captureStdout(t, func() {
		c.adminBuild(c.staffClient(), []string{buildID, "stage", "install", "done", "--output", "@" + file})
	})
	if !reflect.DeepEqual(body, map[string]interface{}{"state": "done", "output": map[string]interface{}{"installed": []interface{}{"record_payment"}}}) {
		t.Fatalf("body = %v", body)
	}
}

func TestAdminBuildStageUsage(t *testing.T) {
	args := strings.Fields(os.Getenv("STAGE_ARGS"))
	if inChild(t, func(c *ctx) { c.adminBuildStage(c.staffClient(), buildID, args) }) {
		return
	}
	c := staffCtx(t, true, nil)
	for _, tc := range []struct{ args, want string }{
		{"plan approved", "the state is running, done or failed"},
		{"plan", "usage: qube admin build ID stage STAGE running|done|failed"},
		{"plan done --output [1]", "--output is a JSON object"},
	} {
		code, out := runChild(t, c, "STAGE_ARGS="+tc.args)
		if code != ui.ExitUsage || !strings.Contains(out, tc.want) {
			t.Errorf("%q: exit %d:\n%s", tc.args, code, out)
		}
	}
}

// ---------------------------------------------------------------- the test company

func TestAdminTestCompanyStartsWithoutWaiting(t *testing.T) {
	var method string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		buildBase + "/test-company": func(w http.ResponseWriter, r *http.Request) {
			method = r.Method
			writeJSON(w, 202, dataOf(fakeBuild(map[string]string{"plan": "done", "install": "done", "test_company": "running"}, nil)))
		},
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID, "test-company"}) })
	if method != "POST" || !strings.Contains(out, "Started the test company of build 1 on the server.") || !strings.Contains(out, "or add --wait") {
		t.Fatalf("%s: %s", method, out)
	}
}

// served is a stub of GET /builds/:id that answers from a script, one entry a read (the last
// repeats). A test may change an entry while the server runs.
type served struct {
	mu     sync.Mutex
	script []map[string]interface{}
	reads  int
}

func servedBuilds(script []map[string]interface{}) *served { return &served{script: script} }

func (s *served) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.reads
	if i >= len(s.script) {
		i = len(s.script) - 1
	}
	s.reads++
	writeJSON(w, 200, dataOf(s.script[i]))
}

func (s *served) set(i int, build map[string]interface{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[i] = build
}

func (s *served) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func running(scenarios ...map[string]interface{}) map[string]interface{} {
	return fakeBuild(map[string]string{"plan": "done", "install": "done", "test_company": "running"}, scenarios)
}

func ended(state string, scenarios ...map[string]interface{}) map[string]interface{} {
	b := fakeBuild(map[string]string{"plan": "done", "install": "done", "test_company": state}, scenarios)
	return b
}

// Each scenario's result is printed once, as it lands, and the wait ends when the stage does.
func TestWaitForTestCompanyPrintsEachResultOnce(t *testing.T) {
	shortPolls(t)
	script := servedBuilds([]map[string]interface{}{
		running(scenario("contract-1043", 1, "running", nil)),
		running(scenario("contract-1043", 1, "passed", nil), scenario("contract-1044", 1, "running", nil)),
		running(scenario("contract-1043", 1, "passed", nil), scenario("contract-1044", 1, "running", nil)),
		ended("done", scenario("contract-1043", 1, "passed", nil), scenario("contract-1044", 1, "passed", nil), scenario("contract-1045", 1, "passed", nil)),
	})
	c := staffCtx(t, true, map[string]http.HandlerFunc{buildBase: script.handler})
	var sb strings.Builder
	build, why := c.waitForTestCompany(c.staffClient(), buildID, running(), time.Minute, &sb)
	if why != "done" || script.count() != 4 {
		t.Fatalf("why %q after %d reads", why, script.count())
	}
	want := "passed  contract-1043  completed/added (run run-contract-1043)\n" +
		"passed  contract-1044  completed/added (run run-contract-1044)\n" +
		"passed  contract-1045  completed/added (run run-contract-1045)\n"
	if sb.String() != want {
		t.Fatalf("printed:\n%s\nwant:\n%s", sb.String(), want)
	}
	if got := scenarioSummary(build); got != "3 of 3 scenarios passed" {
		t.Fatalf("summary = %q", got)
	}
}

// A scenario that failed before the stage was started again isn't announced again; its new
// attempt is.
func TestWaitForTestCompanyKeepsStaleResultsQuiet(t *testing.T) {
	shortPolls(t)
	stale := scenario("contract-1045", 1, "failed", map[string]interface{}{"message": "ended failed/refused"})
	passed := scenario("contract-1043", 1, "passed", nil)
	script := servedBuilds([]map[string]interface{}{
		running(passed, stale),
		ended("done", passed, scenario("contract-1045", 2, "passed", nil)),
	})
	c := staffCtx(t, true, map[string]http.HandlerFunc{buildBase: script.handler})
	var sb strings.Builder
	_, why := c.waitForTestCompany(c.staffClient(), buildID, running(passed, stale), time.Minute, &sb)
	if why != "done" || sb.String() != "passed  contract-1045  completed/added (run run-contract-1045)\n" {
		t.Fatalf("why %q, printed:\n%s", why, sb.String())
	}
}

func TestWaitForTestCompanyEnds(t *testing.T) {
	shortPolls(t)
	cases := []struct {
		name, why string
		script    []map[string]interface{}
		limit     time.Duration
		printed   string
	}{
		{"a failed stage", "failed", []map[string]interface{}{
			ended("failed", scenario("contract-1043", 1, "passed", nil), scenario("contract-1045", 1, "failed", map[string]interface{}{"message": "ended failed/refused"}))},
			time.Minute, "FAILED  contract-1045 (run run-contract-1045): ended failed/refused"},
		{"the limit", "timeout", []map[string]interface{}{running(scenario("contract-1043", 1, "running", nil))}, 30 * time.Millisecond, ""},
		{"a stage reopened", "pending", []map[string]interface{}{fakeBuild(map[string]string{"plan": "done", "install": "done"}, nil)}, time.Minute, ""},
	}
	for _, tc := range cases {
		c := staffCtx(t, true, map[string]http.HandlerFunc{buildBase: servedBuilds(tc.script).handler})
		var sb strings.Builder
		_, why := c.waitForTestCompany(c.staffClient(), buildID, running(), tc.limit, &sb)
		if why != tc.why || !strings.Contains(sb.String(), tc.printed) {
			t.Errorf("%s: why %q, printed %q", tc.name, why, sb.String())
		}
	}
}

// A read that fails for a moment doesn't end the wait.
func TestWaitForTestCompanyRetriesAFailedRead(t *testing.T) {
	shortPolls(t)
	reads := 0
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		buildBase: func(w http.ResponseWriter, r *http.Request) {
			reads++
			if reads < 3 {
				writeJSON(w, 503, map[string]interface{}{"error": map[string]string{"code": "unavailable", "message": "The database didn't answer."}})
				return
			}
			writeJSON(w, 200, dataOf(ended("done", scenario("contract-1043", 1, "passed", nil))))
		},
	})
	var sb strings.Builder
	_, why := c.waitForTestCompany(c.staffClient(), buildID, running(), time.Minute, &sb)
	if why != "done" || reads != 3 {
		t.Fatalf("why %q after %d reads", why, reads)
	}
}

// The command: results as they land, the answer at the end, and a non-zero exit for a failure
// or a wait that ran out.
func TestAdminTestCompanyWaitCommand(t *testing.T) {
	shortPolls(t)
	script := servedBuilds([]map[string]interface{}{
		running(scenario("contract-1043", 1, "passed", nil)),
		ended("done", scenario("contract-1043", 1, "passed", nil)),
	})
	post := func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 202, dataOf(running())) }
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		buildBase:                   script.handler,
		buildBase + "/test-company": post,
	})
	setJSON(t, false)
	out := captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID, "test-company", "--wait"}) })
	if want := "passed  contract-1043  completed/added (run run-contract-1043)\nThe test company is done: 1 of 3 scenarios passed.\n"; out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}

	// --json: stdout is the build, the progress is on stderr
	done := servedBuilds([]map[string]interface{}{ended("done", scenario("contract-1043", 1, "passed", nil))})
	c = staffCtx(t, true, map[string]http.HandlerFunc{buildBase: done.handler, buildBase + "/test-company": post})
	setJSON(t, true)
	var js string
	progress := captureStderr(t, func() {
		js = captureStdout(t, func() { c.adminBuild(c.staffClient(), []string{buildID, "test-company", "--wait=1m"}) })
	})
	var got map[string]interface{}
	if err := json.Unmarshal([]byte(js), &got); err != nil || got["id"] != buildID || !strings.Contains(progress, "passed  contract-1043") {
		t.Fatalf("json: %v\nstdout: %s\nstderr: %s", err, js, progress)
	}
}

func TestAdminTestCompanyWaitFailsLoudly(t *testing.T) {
	if inChild(t, func(c *ctx) {
		buildPollEvery = time.Millisecond
		c.adminTestCompany(c.staffClient(), buildID, []string{"--wait=" + os.Getenv("WAIT")})
	}) {
		return
	}
	failed := ended("failed", scenario("contract-1043", 1, "passed", nil), scenario("contract-1045", 1, "failed", map[string]interface{}{"message": "ended failed/refused"}))
	stageEntry(failed, "test_company")["error"] = "Scenario “contract-1045” (run run-contract-1045) didn't pass: ended failed/refused"
	script := servedBuilds([]map[string]interface{}{failed})
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		buildBase:                   script.handler,
		buildBase + "/test-company": func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 202, dataOf(running())) },
	})
	code, out := runChild(t, c, "WAIT=1m")
	if code != ui.ExitFailure || !strings.Contains(out, "FAILED  contract-1045 (run run-contract-1045): ended failed/refused") || !strings.Contains(out, "the test company stage failed: Scenario “contract-1045”") {
		t.Fatalf("a failure: exit %d:\n%s", code, out)
	}

	script.set(0, running(scenario("contract-1043", 1, "running", nil)))
	code, out = runChild(t, c, "WAIT=40ms")
	if code != ui.ExitFailure || !strings.Contains(out, "still running after 40ms") || !strings.Contains(out, "keeps running on the server") {
		t.Fatalf("a timeout: exit %d:\n%s", code, out)
	}
}

// --wait=D, else the global --timeout when it was given, else ten minutes.
func TestWaitLimit(t *testing.T) {
	bare, long := &waitFlag{}, &waitFlag{}
	_ = bare.Set("true")
	_ = long.Set("30m")
	c := &ctx{timeout: 45 * time.Second}
	if got := c.waitLimit(bare); got != 10*time.Minute {
		t.Errorf("bare --wait = %v", got)
	}
	if got := c.waitLimit(long); got != 30*time.Minute {
		t.Errorf("--wait=30m = %v", got)
	}
	c.timeoutSet = true
	if got := c.waitLimit(bare); got != 45*time.Second {
		t.Errorf("--wait with --timeout 45 = %v", got)
	}
	if got := c.waitLimit(long); got != 30*time.Minute {
		t.Errorf("--wait=30m with --timeout 45 = %v", got)
	}
}

// `qube admin` reaches each command, with the method and route the staff API has for it.
func TestAdminDispatchesTheBuildCommands(t *testing.T) {
	var calls []string
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		"/api/admin/": func(w http.ResponseWriter, r *http.Request) {
			calls = append(calls, r.Method+" "+r.URL.Path)
			if r.URL.Path == "/api/admin/integration-requests" || strings.HasSuffix(r.URL.Path, "/builds") && r.Method == "GET" {
				writeJSON(w, 200, map[string]interface{}{"data": []interface{}{}, "meta": map[string]interface{}{}})
				return
			}
			writeJSON(w, 200, dataOf(fakeBuild(nil, nil)))
		},
	})
	setJSON(t, true)
	for _, args := range [][]string{
		{"integration-requests"},
		{"integration-request", reqID},
		{"integration-request", reqID, "builds"},
		{"integration-request", reqID, "set-app", "app-9"},
		{"integration-request", reqID, "kit"},
		{"integration-request", reqID, "kit", "set", "--stack", "rails"},
		{"builds", "start", reqID},
		{"build", buildID},
		{"build", buildID, "manifest"},
		{"build", buildID, "stage", "plan", "running"},
		{"build", buildID, "test-company"},
	} {
		_ = captureStdout(t, func() { c.admin(args) })
	}
	want := []string{
		"GET /api/admin/integration-requests",
		"GET " + reqPath,
		"GET " + reqPath + "/builds",
		"PATCH " + reqPath + "/app",
		"GET " + reqPath,
		"PATCH " + reqPath + "/kit",
		"POST " + reqPath + "/builds",
		"GET " + buildBase,
		"GET " + buildBase,
		"POST " + buildBase + "/stages/plan",
		"POST " + buildBase + "/test-company",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
}

// ---------------------------------------------------------------- errors, help and completion

// A refusal prints the server's message; a missing build:manage names the fix.
func TestAdminBuildWritesPrintTheServersMessage(t *testing.T) {
	if inChild(t, func(c *ctx) { c.adminBuilds(c.staffClient(), []string{"start", reqID}) }) {
		return
	}
	c := staffCtx(t, true, map[string]http.HandlerFunc{
		reqPath + "/builds": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 403, map[string]interface{}{"error": map[string]string{"code": "permission_required",
				"message": "This session doesn't hold builds.manage. Log in with `qube login --admin` (an act-as grant or an ordinary session holds no staff permissions)."}})
		},
	})
	code, out := runChild(t, c)
	if code != ui.ExitFailure || !strings.Contains(out, "HTTP 403 permission_required: This session doesn't hold builds.manage. Log in with `qube login --admin`") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

func TestProblemLines(t *testing.T) {
	body := `{"error": {"code": "invalid_manifest", "errors": [{"path": "who.company_name", "message": "can't be blank"}, {"message": "too big"}]}}`
	if got := strings.Join(problemLines(body), "|"); got != "who.company_name: can't be blank|too big" {
		t.Fatalf("lines = %q", got)
	}
	for _, body := range []string{"", "not json", `{"error": {"errors": "x"}}`, `{"errors": {"detail": "x"}}`} {
		if lines := problemLines(body); len(lines) != 0 {
			t.Errorf("%q: %v", body, lines)
		}
	}
}

// The help lists every build command and the permission it needs, and completion knows every
// flag these commands define (read from the source, so a new flag can't be left out).
func TestBuildCommandsAreInHelpAndCompletion(t *testing.T) {
	help := captureStdout(t, staffUsage)
	for _, want := range []string{"admin integration-requests", "admin integration-request ID set-app APP_ID", "admin integration-request ID kit set",
		"admin integration-request ID kit files --out DIR [--force]", "admin integration-request ID builds", "admin builds start REQUEST_ID",
		"admin build ID manifest set FILE|-", "admin build ID stage STAGE running|done|failed [--error MSG] [--output JSON|@FILE]",
		"admin build ID test-company [--wait[=10m]]", "builds.manage", "login --agent NAME [--admin]"} {
		if !strings.Contains(help, want) {
			t.Errorf("staff help lacks %q", want)
		}
	}
	f, err := parser.ParseFile(token.NewFileSet(), "admin_builds.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var flags []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != "fs" {
			return true
		}
		arg := 0
		switch sel.Sel.Name {
		case "String", "Bool", "Int":
		case "Var":
			arg = 1
		default:
			return true
		}
		if name, ok := stringLit(call.Args[arg]); ok {
			flags = append(flags, "--"+name)
		}
		return true
	})
	if len(flags) < 8 {
		t.Fatalf("found only %v", flags)
	}
	for _, flag := range flags {
		if !contains(commandTree["admin"], flag) && !contains(globalFlags, flag) {
			t.Errorf("completion lacks %s", flag)
		}
	}
}
