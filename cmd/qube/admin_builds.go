package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// `qube admin integration-requests | integration-request | builds | build` (QuBe Sync staff):
// the "build it free" pipeline's staff API (docs/build-it-free-pipeline.md in the qube repo).
// A request is what a prospect sent; a build is one attempt at building their integration,
// with a plan (the manifest), stages and the gates a person approves in the browser. Reading
// needs admin.read (a request's text also admin.read_customer_data); changing anything needs
// builds.manage, which an agent's session can hold. Nothing here approves a gate or reopens a
// build: that is the admin page's, for a person.

// buildPollEvery is how often `test-company --wait` reads the build (a variable so tests can
// shorten it).
var buildPollEvery = 3 * time.Second

// maxManifestRead caps what `manifest set -` reads from stdin. The server's own limit (256 KB)
// is what refuses a manifest; this only keeps a runaway pipe from filling memory.
const maxManifestRead = 4 << 20

type dataAnswer struct {
	Data map[string]interface{} `json:"data"`
}

// leadingID is the id a command names first, and the words after it.
func leadingID(args []string, usage string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		ui.Usage("usage: %s", usage)
	}
	return args[0], args[1:]
}

func requestPath(id string, rest ...string) string {
	return "/api/admin/integration-requests/" + url.PathEscape(id) + strings.Join(rest, "")
}

func buildPath(id string, rest ...string) string {
	return "/api/admin/builds/" + url.PathEscape(id) + strings.Join(rest, "")
}

// call performs a staff request and stops the command on an error, printing every problem a
// 422 lists as `path: message`.
func (c *ctx) call(cl *api.Client, method, path string, query url.Values, body, out interface{}) {
	if err := cl.Do(method, path, query, body, out); err != nil {
		c.failCall(err)
	}
}

// failCall reports a refusal from a staff route. A 422 whose body lists problems (a manifest
// that doesn't validate, a kit field that is refused) prints the server's message and then each
// problem as `path: message`; under --json the server's body goes to stdout, so a script reads
// the problems with their paths.
func (c *ctx) failCall(err error) {
	err = staffError(c.host, err)
	var e *api.Error
	if errors.As(err, &e) && e.Status == http.StatusUnprocessableEntity {
		if lines := problemLines(e.Body); len(lines) > 0 {
			if ui.JSON {
				ui.PrintRaw([]byte(strings.TrimSpace(e.Body)))
			}
			fmt.Fprintf(os.Stderr, "qube: %v\n", e)
			for _, l := range lines {
				fmt.Fprintf(os.Stderr, "  %s\n", l)
			}
			os.Exit(ui.ExitFailure)
		}
	}
	fail(err)
}

// problemLines reads {"error": {"errors": [{"path", "message"}]}} as `path: message` lines (the
// message alone where a problem has no path).
func problemLines(body string) []string {
	var env struct {
		Error struct {
			Errors []struct {
				Path    string `json:"path"`
				Message string `json:"message"`
			} `json:"errors"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &env) != nil {
		return nil
	}
	var lines []string
	for _, p := range env.Error.Errors {
		if p.Path == "" {
			lines = append(lines, p.Message)
		} else {
			lines = append(lines, p.Path+": "+p.Message)
		}
	}
	return lines
}

// ---------------------------------------------------------------- integration requests

func (c *ctx) adminIntegrationRequests(cl *api.Client, args []string) {
	fs := flag.NewFlagSet("admin integration-requests", flag.ExitOnError)
	status := fs.String("status", "", "submitted, building or delivered")
	page := fs.Int("page", 0, "page number")
	size := fs.Int("page-size", 0, "requests per page (at most 100)")
	parseAnywhere(fs, args)
	if fs.NArg() > 0 {
		ui.Usage("usage: qube admin integration-requests [--status submitted|building|delivered] [--page N]")
	}
	q := url.Values{}
	setIf(q, "status", *status)
	setInt(q, "page", *page)
	setInt(q, "page_size", *size)
	var out listAnswer
	c.call(cl, "GET", "/api/admin/integration-requests", q, nil, &out)
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	var rows [][]string
	for _, r := range out.Data {
		org, _ := r["organization"].(map[string]interface{})
		build := "-"
		if latest, ok := r["latest_build"].(map[string]interface{}); ok {
			build = fmt.Sprintf("#%d %s", num(latest["number"]), nextLabel(latest))
		}
		rows = append(rows, []string{str(r["id"]), str(r["status"]), truncate(str(r["company_name"]), 40), str(org["name"]), build, localTime(str(r["submitted_at"]))})
	}
	ui.Table(os.Stdout, []string{"ID", "STATUS", "COMPANY", "ORGANIZATION", "BUILD", "SENT"}, rows, "No integration requests match.")
	pageNote(out.Meta, "")
}

const integrationRequestUsage = "qube admin integration-request ID [set-app APP_ID | kit [set ... | files --out DIR [--force]] | builds]"

func (c *ctx) adminIntegrationRequest(cl *api.Client, args []string) {
	id, rest := leadingID(args, integrationRequestUsage)
	if len(rest) == 0 {
		var out dataAnswer
		c.call(cl, "GET", requestPath(id), nil, nil, &out)
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		printRequest(os.Stdout, out.Data)
		return
	}
	switch rest[0] {
	case "set-app":
		c.adminSetApp(cl, id, rest[1:])
	case "kit":
		c.adminKit(cl, id, rest[1:])
	case "builds":
		var out listAnswer
		c.call(cl, "GET", requestPath(id, "/builds"), nil, nil, &out)
		if ui.JSON {
			ui.PrintJSON(out)
			return
		}
		var rows [][]string
		for _, b := range out.Data {
			manifest := "none"
			if b["has_manifest"] == true {
				manifest = "saved"
			}
			rows = append(rows, []string{str(b["number"]), str(b["id"]), nextLabel(b), manifest, localTime(str(b["created_at"]))})
		}
		ui.Table(os.Stdout, []string{"BUILD", "ID", "NEXT", "MANIFEST", "STARTED"}, rows, "No builds yet (`qube admin builds start "+id+"`).")
	default:
		ui.Usage("usage: %s", integrationRequestUsage)
	}
}

func (c *ctx) adminSetApp(cl *api.Client, id string, args []string) {
	fs := flag.NewFlagSet("admin integration-request set-app", flag.ExitOnError)
	parseAnywhere(fs, args)
	app := onePositional(fs, "qube admin integration-request ID set-app APP_ID")
	var out dataAnswer
	c.call(cl, "PATCH", requestPath(id, "/app"), nil, map[string]string{"app_id": app}, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	a, _ := out.Data["app"].(map[string]interface{})
	fmt.Printf("Request %s now builds in %s.\n", id, appLabel(a))
}

// appLabel is `Name (sandbox app ID)`.
func appLabel(a map[string]interface{}) string {
	if a == nil {
		return "no app"
	}
	kind := "production app"
	if a["sandbox"] == true {
		kind = "sandbox app"
	}
	return fmt.Sprintf("%s (%s %s)", str(a["name"]), kind, str(a["id"]))
}

// printRequest is a request for a person: what they sent, in their words, and where the build
// stands. Under --json the server's object is printed instead.
func printRequest(w io.Writer, d map[string]interface{}) {
	fmt.Fprintf(w, "%s · %s · request %s\n\n", orEmpty(str(d["company_name"]), "(no company name)"), str(d["status"]), str(d["id"]))
	org, _ := d["organization"].(map[string]interface{})
	user, _ := d["user"].(map[string]interface{})
	var kv [][]string
	add := func(field, value string) {
		if strings.TrimSpace(value) != "" {
			kv = append(kv, []string{field, value})
		}
	}
	add("organization", joinNames(str(org["name"]), str(org["id"])))
	add("sent by", str(user["email"]))
	add("sent", localTime(str(d["submitted_at"])))
	add("delivered", localTime(str(d["delivered_at"])))
	add("website", str(d["company_url"]))
	add("starting point", str(d["starting_point"]))
	if st, ok := d["stack"].(map[string]interface{}); ok {
		add("stack", strings.Join(nonEmpty(str(st["language"]), str(st["framework"]), str(st["http_library"])), " · "))
	}
	if app, ok := d["app"].(map[string]interface{}); ok {
		add("app", appLabel(app))
	} else {
		add("app", "none yet (`qube admin integration-request "+str(d["id"])+" set-app APP_ID`)")
	}
	if kit, ok := d["kit"].(map[string]interface{}); ok {
		add("kit", kitSummary(kit))
	}
	if latest, ok := d["latest_build"].(map[string]interface{}); ok {
		add("latest build", fmt.Sprintf("#%d (%s) next: %s", num(latest["number"]), str(latest["id"]), nextLabel(latest)))
	} else {
		add("latest build", "none (`qube admin builds start "+str(d["id"])+"`)")
	}
	ui.Table(w, []string{"FIELD", "VALUE"}, kv, "")
	block := func(title, text string) {
		if strings.TrimSpace(text) != "" {
			fmt.Fprintf(w, "\n%s:\n%s\n", title, indent(text))
		}
	}
	block("About the company", str(d["company_description"]))
	block("Description", str(d["description"]))
	if a, ok := d["answers"].(map[string]interface{}); ok {
		var lines []string
		for _, k := range []string{"directions", "records", "other_records", "triggers", "matching"} {
			if v := valueText(a[k]); v != "" {
				lines = append(lines, k+": "+v)
			}
		}
		block("Answers", strings.Join(lines, "\n"))
	}
	block("Notes", str(d["notes"]))
}

// kitSummary is the kit's settings on one line: which code it writes, and its mappings.
func kitSummary(kit map[string]interface{}) string {
	parts := []string{"code for " + orEmpty(str(kit["code_for"]), "?")}
	if src := str(kit["code_for_source"]); src != "" {
		parts[0] += " (" + src + ")"
	}
	if s := str(kit["stack"]); s != "" {
		parts = append(parts, "stack "+s)
	}
	if h := str(kit["http"]); h != "" {
		parts = append(parts, "http "+h)
	}
	for _, m := range []struct{ name, key string }{{"mapping out", "mapping_out"}, {"mapping in", "mapping_in"}} {
		if n := lineCount(str(kit[m.key])); n > 0 {
			parts = append(parts, fmt.Sprintf("%s %s", m.name, plural(n, "line", "lines")))
		} else {
			parts = append(parts, m.name+" none")
		}
	}
	return strings.Join(parts, ", ")
}

func lineCount(s string) int {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func nonEmpty(list ...string) []string {
	var out []string
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func indent(text string) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	for i, l := range lines {
		lines[i] = "  " + l
	}
	return strings.Join(lines, "\n")
}

// valueText is an answer's value as text: a string as it is, a list joined with commas.
func valueText(v interface{}) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []interface{}:
		return strings.Join(strs(x), ", ")
	}
	return str(v)
}

// nextLabel is what a build (or its brief) waits on, in words: "install pending, waits for the
// plan gate".
func nextLabel(build map[string]interface{}) string {
	next, _ := build["next"].(map[string]interface{})
	stage := str(next["stage"])
	if stage == "" {
		return "every stage done"
	}
	states, _ := build["states"].(map[string]interface{})
	label := stage + " " + orEmpty(str(states[stage]), "pending")
	if gate := str(next["waiting_for_gate"]); gate != "" {
		label += ", waits for the " + gate + " gate"
	}
	return label
}

// ---------------------------------------------------------------- the request's kit

func (c *ctx) adminKit(cl *api.Client, id string, args []string) {
	if len(args) == 0 {
		var out dataAnswer
		c.call(cl, "GET", requestPath(id), nil, nil, &out)
		kit, _ := out.Data["kit"].(map[string]interface{})
		if ui.JSON {
			ui.PrintJSON(kit)
			return
		}
		printKit(os.Stdout, kit)
		return
	}
	switch args[0] {
	case "set":
		c.adminKitSet(cl, id, args[1:])
	case "files":
		c.adminKitFiles(cl, id, args[1:])
	default:
		ui.Usage("usage: %s", integrationRequestUsage)
	}
}

func printKit(w io.Writer, kit map[string]interface{}) {
	fmt.Fprintf(w, "Code for:  %s\n", orEmpty(str(kit["code_for"]), "?"))
	if src := str(kit["code_for_source"]); src != "" {
		fmt.Fprintf(w, "           (%s)\n", src)
	}
	fmt.Fprintf(w, "Stack:     %s\n", orEmpty(str(kit["stack"]), "as detected"))
	fmt.Fprintf(w, "HTTP:      %s\n", orEmpty(str(kit["http"]), "as detected"))
	for _, m := range []struct{ title, key string }{{"Mapping out", "mapping_out"}, {"Mapping in", "mapping_in"}} {
		text := str(kit[m.key])
		if lineCount(text) == 0 {
			fmt.Fprintf(w, "\n%s: none\n", m.title)
			continue
		}
		fmt.Fprintf(w, "\n%s (%s):\n%s\n", m.title, plural(lineCount(text), "line", "lines"), indent(text))
	}
}

func (c *ctx) adminKitSet(cl *api.Client, id string, args []string) {
	const usage = "qube admin integration-request ID kit set [--stack S] [--http H] [--mapping-out FILE] [--mapping-in FILE]"
	fs := flag.NewFlagSet("admin integration-request kit set", flag.ExitOnError)
	stack := fs.String("stack", "", "the kit's stack, as the admin page lists them: rails, express_ts, fastapi... or generic (empty: detect)")
	httpLib := fs.String("http", "", "an HTTP library the stack uses: faraday, net_http, fetch, axios, requests, httpx (empty: the default)")
	mappingOut := fs.String("mapping-out", "", "a file with the code that maps the app's records to QuBe Sync's requests (empty: none)")
	mappingIn := fs.String("mapping-in", "", "a file with the code that maps QuBe Sync's answers into the app's records (empty: none)")
	parseAnywhere(fs, args)
	if fs.NArg() > 0 {
		ui.Usage("usage: %s", usage)
	}
	body := map[string]interface{}{}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "stack":
			body["kit_stack"] = *stack
		case "http":
			body["kit_http"] = *httpLib
		case "mapping-out":
			body["kit_mapping_out"] = readMappingFile(*mappingOut)
		case "mapping-in":
			body["kit_mapping_in"] = readMappingFile(*mappingIn)
		}
	})
	if len(body) == 0 {
		ui.Usage("nothing to set\nusage: %s", usage)
	}
	var out dataAnswer
	c.call(cl, "PATCH", requestPath(id, "/kit"), nil, body, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	fmt.Printf("Saved the kit of request %s: %s.\n", id, kitSummary(out.Data))
}

// readMappingFile is a mapping's code from a file; an empty name clears the mapping.
func readMappingFile(name string) string {
	if name == "" {
		return ""
	}
	data, err := os.ReadFile(name)
	if err != nil {
		ui.Fail("%v", err)
	}
	return string(data)
}

// kitFile is one file of the generated kit, as GET .../kit/files answers it: Path is where it is
// written under the output directory (unique), AppPath where it goes in the app.
type kitFile struct {
	Path     string `json:"path"`
	AppPath  string `json:"app_path"`
	Language string `json:"language"`
	Section  string `json:"section"`
	Content  string `json:"content"`
}

func (c *ctx) adminKitFiles(cl *api.Client, id string, args []string) {
	const usage = "qube admin integration-request ID kit files --out DIR [--force]"
	fs := flag.NewFlagSet("admin integration-request kit files", flag.ExitOnError)
	outDir := fs.String("out", "", "the directory to write the kit's files under (created if needed)")
	force := fs.Bool("force", false, "overwrite files that already exist")
	parseAnywhere(fs, args)
	if fs.NArg() > 0 || *outDir == "" {
		ui.Usage("usage: %s", usage)
	}
	var out struct {
		Data struct {
			Stack             string                   `json:"stack"`
			HTTP              string                   `json:"http"`
			CodeFor           string                   `json:"code_for"`
			Generated         *bool                    `json:"generated"` // absent from a server before it said
			Fallback          string                   `json:"fallback"`
			MappingCheckStack string                   `json:"mapping_check_stack"`
			Width             int                      `json:"width"`
			Operations        []map[string]interface{} `json:"operations"`
			Files             []kitFile                `json:"files"`
		} `json:"data"`
	}
	c.call(cl, "GET", requestPath(id, "/kit/files"), nil, nil, &out)
	d := out.Data
	generated := d.Generated == nil || *d.Generated
	var written []kitFile
	if len(d.Files) > 0 {
		var err error
		if written, err = writeKitFiles(*outDir, d.Files, *force); err != nil {
			ui.Fail("%v", err)
		}
	}
	if ui.JSON {
		files := []map[string]interface{}{}
		for _, f := range written {
			files = append(files, map[string]interface{}{"path": f.Path, "app_path": f.AppPath, "language": f.Language, "section": f.Section, "bytes": len(f.Content)})
		}
		ui.PrintJSON(map[string]interface{}{"out": *outDir, "stack": d.Stack, "http": d.HTTP, "code_for": d.CodeFor, "generated": generated, "fallback": d.Fallback,
			"mapping_check_stack": d.MappingCheckStack, "width": d.Width, "operations": d.Operations, "files": files})
		return
	}
	if len(written) == 0 {
		fmt.Printf("The kit has no files for %s; nothing was written.\n", orEmpty(d.CodeFor, "this request"))
		if d.Fallback != "" {
			fmt.Println(d.Fallback)
		}
		return
	}
	fmt.Printf("Wrote %s to %s (code for %s):\n", plural(len(written), "file", "files"), *outDir, orEmpty(d.CodeFor, "?"))
	var rows [][]string
	for _, f := range written {
		goesIn := ""
		if f.AppPath != "" && f.AppPath != f.Path {
			goesIn = f.AppPath
		}
		rows = append(rows, []string{f.Path, goesIn, f.Language, fmt.Sprintf("%d", len(f.Content)), truncate(f.Section, 60)})
	}
	ui.Table(os.Stdout, []string{"FILE", "GOES IN (IF DIFFERENT)", "LANGUAGE", "BYTES", "SECTION"}, rows, "")
	if len(d.Operations) > 0 {
		var ops [][]string
		for _, o := range d.Operations {
			source := "kit default"
			if o["from_plan"] == true {
				source = "the plan"
			}
			ops = append(ops, []string{str(o["name"]), str(o["workflow"]), str(o["record"]), str(o["kind"]), str(o["path"]), source})
		}
		fmt.Println("\nOperations:")
		ui.Table(os.Stdout, []string{"NAME", "WORKFLOW", "RECORD", "KIND", "FILE", "NAMED BY"}, ops, "")
	}
	if d.Width > 0 {
		check := ""
		if d.MappingCheckStack != "" {
			check = fmt.Sprintf(" (bin/kit-mapping-check --stack %s)", d.MappingCheckStack)
		}
		fmt.Printf("\nA mapping line is at most %d columns wide%s.\n", d.Width, check)
	}
}

// safeKitPath is the path a kit file is written to, relative to the output directory. A path
// the server sends is untrusted data: an absolute one (a leading slash or backslash, a drive
// letter) or one with a `..` element is refused outright, whatever it would resolve to.
func safeKitPath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("an empty file path")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("the file path %q holds a NUL", p)
	}
	slashed := strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(slashed, "/") || filepath.IsAbs(p) || filepath.VolumeName(p) != "" || (len(slashed) >= 2 && slashed[1] == ':') {
		return "", fmt.Errorf("the file path %q is absolute", p)
	}
	for _, part := range strings.Split(slashed, "/") {
		if part == ".." {
			return "", fmt.Errorf("the file path %q goes up with ..", p)
		}
	}
	clean := filepath.Clean(filepath.FromSlash(slashed))
	if clean == "." {
		return "", fmt.Errorf("the file path %q names no file", p)
	}
	return clean, nil
}

// writeKitFiles writes the kit's files under dir. It checks every path and every target before
// it writes anything: a refused path, a file in the way (a directory, a link, or without force
// any existing file) or two files for one path writes nothing. Directories are created as
// needed, and no file lands outside dir even through a link in it.
func writeKitFiles(dir string, files []kitFile, force bool) ([]kitFile, error) {
	type target struct {
		file kitFile
		path string
	}
	var targets []target
	seen := map[string]bool{}
	var exists []string
	for _, f := range files {
		rel, err := safeKitPath(f.Path)
		if err != nil {
			return nil, fmt.Errorf("the server sent %v, so nothing was written", err)
		}
		// a case-insensitive disk would write two such paths to one file
		key := strings.ToLower(rel)
		if seen[key] {
			return nil, fmt.Errorf("the server sent the path %q twice, so nothing was written", f.Path)
		}
		seen[key] = true
		path := filepath.Join(dir, rel)
		info, err := os.Lstat(path)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			return nil, fmt.Errorf("%s is a link; nothing was written", path)
		case err == nil && info.IsDir():
			return nil, fmt.Errorf("%s is a directory; nothing was written", path)
		case err == nil && !force:
			exists = append(exists, path)
		case err != nil && !os.IsNotExist(err):
			return nil, fmt.Errorf("%v; nothing was written", err)
		}
		f.Path = filepath.ToSlash(rel)
		targets = append(targets, target{f, path})
	}
	if len(exists) > 0 {
		sort.Strings(exists)
		verb := "already exist"
		if len(exists) == 1 {
			verb = "already exists"
		}
		return nil, fmt.Errorf("%s %s, so nothing was written (--force overwrites):\n  %s", plural(len(exists), "file", "files"), verb, strings.Join(exists, "\n  "))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var written []kitFile
	for _, t := range targets {
		parent := filepath.Dir(t.path)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return written, err
		}
		resolved, err := filepath.EvalSymlinks(parent)
		if err != nil {
			return written, err
		}
		if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return written, fmt.Errorf("%s leads outside %s; stopped", t.path, dir)
		}
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if force {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		f, err := os.OpenFile(t.path, flags, 0o644)
		if err != nil {
			return written, err
		}
		_, werr := f.WriteString(t.file.Content)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return written, werr
		}
		written = append(written, t.file)
	}
	return written, nil
}

// ---------------------------------------------------------------- builds

func (c *ctx) adminBuilds(cl *api.Client, args []string) {
	const usage = "qube admin builds start REQUEST_ID"
	if len(args) == 0 || args[0] != "start" {
		ui.Usage("usage: %s", usage)
	}
	fs := flag.NewFlagSet("admin builds start", flag.ExitOnError)
	parseAnywhere(fs, args[1:])
	requestID := onePositional(fs, usage)
	var out dataAnswer
	c.call(cl, "POST", requestPath(requestID, "/builds"), nil, nil, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	fmt.Printf("Started build %d of request %s (build %s). Next: %s.\n", num(out.Data["number"]), requestID, str(out.Data["id"]), nextLabel(out.Data))
}

const buildUsage = "qube admin build ID [manifest [set FILE|-] | stage STAGE running|done|failed [--error MSG] [--output JSON|@FILE] | test-company [--wait]]"

func (c *ctx) adminBuild(cl *api.Client, args []string) {
	id, rest := leadingID(args, buildUsage)
	if len(rest) == 0 {
		var out dataAnswer
		c.call(cl, "GET", buildPath(id), nil, nil, &out)
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		printBuild(os.Stdout, out.Data)
		return
	}
	switch rest[0] {
	case "manifest":
		switch {
		case len(rest) == 1:
			var out dataAnswer
			c.call(cl, "GET", buildPath(id), nil, nil, &out)
			if out.Data["manifest"] == nil {
				ui.Fail("build %s has no manifest yet (`qube admin build %s manifest set FILE`)", id, id)
			}
			ui.PrintJSON(out.Data["manifest"])
		case rest[1] == "set":
			c.adminManifestSet(cl, id, rest[2:])
		default:
			ui.Usage("usage: %s", buildUsage)
		}
	case "stage":
		c.adminBuildStage(cl, id, rest[1:])
	case "test-company":
		c.adminTestCompany(cl, id, rest[1:])
	default:
		ui.Usage("usage: %s", buildUsage)
	}
}

func (c *ctx) adminManifestSet(cl *api.Client, id string, args []string) {
	const usage = "qube admin build ID manifest set FILE|-"
	fs := flag.NewFlagSet("admin build manifest set", flag.ExitOnError)
	parseAnywhere(fs, args)
	file := onePositional(fs, usage)
	var raw []byte
	var err error
	if file == "-" {
		raw, err = io.ReadAll(io.LimitReader(os.Stdin, maxManifestRead+1))
		file = "stdin"
	} else {
		raw, err = os.ReadFile(file)
	}
	if err != nil {
		ui.Fail("%v", err)
	}
	if len(raw) > maxManifestRead {
		ui.Fail("%s: over %d MB; a manifest is at most 256 KB", file, maxManifestRead>>20)
	}
	if t := bytes.TrimSpace(raw); len(t) == 0 || t[0] != '{' {
		ui.Fail("%s: the manifest is a JSON object", file)
	}
	var probe map[string]interface{}
	if err := json.Unmarshal(raw, &probe); err != nil {
		ui.Fail("%s: not JSON: %v", file, err)
	}
	var out dataAnswer
	// the file's own bytes go as the body: numbers and key order reach the server as written
	c.call(cl, "PUT", buildPath(id, "/manifest"), nil, raw, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	m, _ := out.Data["manifest"].(map[string]interface{})
	fmt.Printf("Saved the manifest of build %d: %s, %s, %s. Next: %s.\n", num(out.Data["number"]),
		plural(listLen(m, "workflows"), "workflow", "workflows"), plural(listLen(m, "operations"), "operation", "operations"),
		plural(listLen(m, "scenarios"), "scenario", "scenarios"), nextLabel(out.Data))
}

func listLen(m map[string]interface{}, key string) int {
	list, _ := m[key].([]interface{})
	return len(list)
}

func (c *ctx) adminBuildStage(cl *api.Client, id string, args []string) {
	const usage = "qube admin build ID stage STAGE running|done|failed [--error MSG] [--output JSON|@FILE]"
	fs := flag.NewFlagSet("admin build stage", flag.ExitOnError)
	errMsg := fs.String("error", "", "why the stage failed (failed needs one)")
	output := fs.String("output", "", "what the stage produced, a JSON object (ids, counts, paths) or @FILE; a stage keeps the output it has when this is left out")
	parseAnywhere(fs, args)
	if fs.NArg() != 2 {
		ui.Usage("usage: %s", usage)
	}
	// the stage is test_company; test-company is how the command spells it
	stage, state := strings.ReplaceAll(fs.Arg(0), "-", "_"), fs.Arg(1)
	switch state {
	case "running", "done", "failed":
	default:
		ui.Usage("the state is running, done or failed (got %q)\nusage: %s", state, usage)
	}
	body := map[string]interface{}{"state": state}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "error":
			body["error"] = *errMsg
		case "output":
			obj, ok := readJSONArg(*output).(map[string]interface{})
			if !ok {
				ui.Usage("--output is a JSON object")
			}
			body["output"] = obj
		}
	})
	var out dataAnswer
	c.call(cl, "POST", buildPath(id, "/stages/", url.PathEscape(stage)), nil, body, &out)
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	fmt.Printf("Build %d: %s is now %s. Next: %s.\n", num(out.Data["number"]), stage, state, nextLabel(out.Data))
}

// ---------------------------------------------------------------- the test company stage

func (c *ctx) adminTestCompany(cl *api.Client, id string, args []string) {
	const usage = "qube admin build ID test-company [--wait[=10m]]"
	fs := flag.NewFlagSet("admin build test-company", flag.ExitOnError)
	wait := &waitFlag{}
	fs.Var(wait, "wait", "wait until the stage is done or failed, printing each scenario as it ends (--wait=30m for longer than 10m; the global --timeout sets the same limit)")
	parseAnywhere(fs, args)
	if fs.NArg() > 0 {
		ui.Usage("usage: %s", usage)
	}
	var out dataAnswer
	c.call(cl, "POST", buildPath(id, "/test-company"), nil, nil, &out)
	if !wait.on {
		if ui.JSON {
			ui.PrintJSON(out.Data)
			return
		}
		fmt.Printf("Started the test company of build %d on the server. `qube admin build %s` shows where it is, or add --wait.\n", num(out.Data["number"]), id)
		return
	}
	limit := c.waitLimit(wait)
	// each scenario's result is printed as it lands: on stdout for a person, on stderr under
	// --json, where stdout holds the one answer a script reads
	progress := io.Writer(os.Stdout)
	if ui.JSON {
		progress = os.Stderr
	}
	build, why := c.waitForTestCompany(cl, id, out.Data, limit, progress)
	switch why {
	case "done":
		if ui.JSON {
			ui.PrintJSON(build)
			return
		}
		fmt.Printf("The test company is done: %s.\n", orEmpty(scenarioSummary(build), "no scenarios"))
	case "failed":
		if ui.JSON {
			ui.PrintJSON(build)
		}
		reason := str(stageEntry(build, "test_company")["error"])
		ui.Fail("the test company stage failed: %s", orEmpty(reason, "see `qube admin build "+id+"`"))
	case "timeout":
		if ui.JSON {
			ui.PrintJSON(build)
		}
		ui.Fail("the test company is still running after %s; it keeps running on the server. `qube admin build %s` shows where it is, and `test-company --wait` waits again.", limit, id)
	default:
		if ui.JSON {
			ui.PrintJSON(build)
		}
		ui.Fail("the test company stage went back to pending (the build was reopened), so qube stopped waiting")
	}
}

// waitLimit is how long a --wait waits: --wait=D, else the global --timeout when it was given
// (the same words, "or the timeout passes"), else ten minutes.
func (c *ctx) waitLimit(w *waitFlag) time.Duration {
	if c.timeoutSet && w.limit == defaultWaitLimit {
		return c.timeout
	}
	return w.limit
}

// waitForTestCompany reads the build every buildPollEvery until its test_company stage is done
// or failed, or limit passes (0: no limit), writing each scenario's result to w as it lands.
// first is the build as starting the stage answered it: scenarios that had already ended in it,
// from an earlier attempt, are not repeated. It returns the last build read and why it stopped:
// "done", "failed", "timeout", or "pending" (the stage was reopened while it waited). A read
// that fails for a moment (the network, a 5xx) is tried again.
func (c *ctx) waitForTestCompany(cl *api.Client, id string, first map[string]interface{}, limit time.Duration, w io.Writer) (map[string]interface{}, string) {
	seen := map[string]bool{}
	for _, e := range scenarioEntries(first) {
		if scenarioEnded(e) {
			seen[scenarioKey(e)] = true
		}
	}
	var deadline time.Time
	if limit > 0 {
		deadline = time.Now().Add(limit)
	}
	build, failures := first, 0
	for {
		for _, e := range scenarioEntries(build) {
			if scenarioEnded(e) && !seen[scenarioKey(e)] {
				seen[scenarioKey(e)] = true
				fmt.Fprintln(w, scenarioLine(e))
			}
		}
		switch str(stageEntry(build, "test_company")["state"]) {
		case "done":
			return build, "done"
		case "failed":
			return build, "failed"
		case "running":
		default:
			return build, "pending"
		}
		pause := buildPollEvery
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return build, "timeout"
			}
			if remaining < pause {
				pause = remaining
			}
		}
		select {
		case <-c.bg.Done():
			fail(c.bg.Err())
		case <-time.After(pause):
		}
		var out dataAnswer
		if err := cl.Do("GET", buildPath(id), nil, nil, &out); err != nil {
			if api.Retryable(err) && failures < 5 {
				failures++
				continue
			}
			c.failCall(err)
		}
		failures = 0
		build = out.Data
	}
}

// ---------------------------------------------------------------- a build for a person

// stageEntry is the build's entry for a stage, or an empty map.
func stageEntry(build map[string]interface{}, stage string) map[string]interface{} {
	stages, _ := build["stages"].([]interface{})
	for _, s := range stages {
		if m, ok := s.(map[string]interface{}); ok && str(m["stage"]) == stage {
			return m
		}
	}
	return map[string]interface{}{}
}

// scenarioEntries are the test company's scenarios as the stage's output holds them: one for
// each that has started, in the manifest's order.
func scenarioEntries(build map[string]interface{}) []map[string]interface{} {
	output, _ := stageEntry(build, "test_company")["output"].(map[string]interface{})
	list, _ := output["scenarios"].([]interface{})
	var out []map[string]interface{}
	for _, s := range list {
		if m, ok := s.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out
}

func scenarioEnded(e map[string]interface{}) bool {
	r := str(e["result"])
	return r == "passed" || r == "failed"
}

// scenarioKey tells one result from another: a scenario run again is a new attempt.
func scenarioKey(e map[string]interface{}) string {
	return str(e["name"]) + "\x00" + str(e["attempt"]) + "\x00" + str(e["result"])
}

// endedAs is how a scenario's run ended: "completed/added".
func endedAs(e map[string]interface{}) string {
	return strings.Trim(str(e["state"])+"/"+str(e["outcome"]), "/")
}

func scenarioLine(e map[string]interface{}) string {
	run := ""
	if r := str(e["run_id"]); r != "" {
		run = " (run " + r + ")"
	}
	if str(e["result"]) == "passed" {
		return fmt.Sprintf("passed  %s  %s%s", str(e["name"]), endedAs(e), run)
	}
	return fmt.Sprintf("FAILED  %s%s: %s", str(e["name"]), run, orEmpty(str(e["message"]), "no message"))
}

// scenarioSummary is "5 of 7 scenarios passed": of the manifest's scenarios, or of those that
// have started where there is no manifest. Empty when there is nothing to count.
func scenarioSummary(build map[string]interface{}) string {
	entries := scenarioEntries(build)
	total := len(manifestScenarioNames(build))
	if total < len(entries) {
		total = len(entries)
	}
	if total == 0 {
		return ""
	}
	passed := 0
	for _, e := range entries {
		if str(e["result"]) == "passed" {
			passed++
		}
	}
	return fmt.Sprintf("%d of %s passed", passed, plural(total, "scenario", "scenarios"))
}

func manifestScenarioNames(build map[string]interface{}) []string {
	manifest, _ := build["manifest"].(map[string]interface{})
	list, _ := manifest["scenarios"].([]interface{})
	var names []string
	for _, s := range list {
		if m, ok := s.(map[string]interface{}); ok {
			names = append(names, str(m["name"]))
		}
	}
	return names
}

// stageDetail is what a stage's row says beyond its state: why it failed, how the test company
// went, or the small facts it produced.
func stageDetail(build, stage map[string]interface{}) string {
	if e := str(stage["error"]); e != "" {
		return "error: " + truncate(e, 90)
	}
	if str(stage["stage"]) == "test_company" {
		return scenarioSummary(build)
	}
	output, _ := stage["output"].(map[string]interface{})
	var keys []string
	for k := range output {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		switch v := output[k].(type) {
		case map[string]interface{}:
		case []interface{}:
			parts = append(parts, fmt.Sprintf("%s: %d", k, len(v)))
		default:
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	return truncate(strings.Join(parts, ", "), 90)
}

// printBuild is a build for a person: its stages, its gates, what it waits on and, once the
// test company has started, each scenario's result. Under --json the server's object is
// printed instead, manifest and all.
func printBuild(w io.Writer, d map[string]interface{}) {
	fmt.Fprintf(w, "Build %d of request %s (build %s)\nNext: %s\n\n", num(d["number"]), str(d["integration_request_id"]), str(d["id"]), nextLabel(d))
	stages, _ := d["stages"].([]interface{})
	var rows [][]string
	for _, s := range stages {
		m, _ := s.(map[string]interface{})
		rows = append(rows, []string{str(m["stage"]), str(m["state"]), localTime(str(m["started_at"])), localTime(str(m["finished_at"])), stageDetail(d, m)})
	}
	ui.Table(w, []string{"STAGE", "STATE", "STARTED", "FINISHED", "DETAIL"}, rows, "No stages.")
	approvals, _ := d["approvals"].([]interface{})
	var gates [][]string
	for _, a := range approvals {
		m, _ := a.(map[string]interface{})
		status := "not approved"
		if m["approved"] == true {
			by, _ := m["approved_by"].(map[string]interface{})
			status = strings.Join(nonEmpty("approved by", str(by["email"]), localTime(str(m["approved_at"]))), " ")
			if note := str(m["note"]); note != "" {
				status += " · " + truncate(note, 60)
			}
		}
		gates = append(gates, []string{str(m["gate"]), status})
	}
	fmt.Fprintln(w)
	ui.Table(w, []string{"GATE", "STATUS"}, gates, "No gates.")
	rowsOf := scenarioRows(d)
	if len(rowsOf) == 0 {
		return
	}
	fmt.Fprintf(w, "\nTest company: %s\n", orEmpty(scenarioSummary(d), "no scenarios"))
	ui.Table(w, []string{"SCENARIO", "RESULT", "TRIES", "ENDED AS", "RUN", "MESSAGE"}, rowsOf, "")
}

// scenarioRows are the test company's scenarios, in the manifest's order, each with its result
// so far ("not started" for one that hasn't begun), then any result for a scenario the manifest
// no longer names.
func scenarioRows(d map[string]interface{}) [][]string {
	entries := scenarioEntries(d)
	byName := map[string]map[string]interface{}{}
	for _, e := range entries {
		byName[str(e["name"])] = e
	}
	var rows [][]string
	row := func(name string, e map[string]interface{}) {
		if e == nil {
			rows = append(rows, []string{name, "not started", "", "", "", ""})
			return
		}
		rows = append(rows, []string{name, str(e["result"]), str(e["attempt"]), endedAs(e), str(e["run_id"]), truncate(str(e["message"]), 70)})
	}
	named := map[string]bool{}
	for _, name := range manifestScenarioNames(d) {
		named[name] = true
		row(name, byName[name])
	}
	for _, e := range entries {
		if !named[str(e["name"])] {
			row(str(e["name"]), e)
		}
	}
	return rows
}
