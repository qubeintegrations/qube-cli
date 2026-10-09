package ops

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The check that keeps `qube qb` help honest as the API changes. checkSpec renders every
// operation's help and holds it to the spec it came from: no Markdown left raw, no URL
// broken, every request example binding through --data to the same body, and every field
// of a choice showing the rule it is part of.
//
// It runs over testdata/qbxml_subset.json in every `go test` (CI has no qube checkout), and
// over a whole spec with QUBE_SPEC set (served_test.go). The subset is extracted from the
// qube checkout's generated spec, never edited: `go test ./internal/ops -update` rewrites
// it, and the help it renders in testdata/help.golden.

var update = flag.Bool("update", false, "rewrite testdata/qbxml_subset.json from the qube checkout's generated spec, and testdata/help.golden")

const (
	subsetPath = "testdata/qbxml_subset.json"
	helpGolden = "testdata/help.golden"
)

// fixtureOps are the operations the subset holds, by operationId: a query with choices
// written in prose, bodies with flat exclusive choices at two depths, a required and an
// optional repeating choice, body choices written as a oneOf (two alternatives of fields;
// three, one of them a single required field; a flag against a list), a Markdown-heavy
// description, and a query with required parameters.
var fixtureOps = []string{"listCustomers", "createInvoice", "createJournalEntry", "updateEstimate", "createVehicleMileage", "updateDataExt", "recordPayment", "listAgingReports"}

func generatedSpec() string {
	repo := os.Getenv("QUBE_REPO")
	if repo == "" {
		repo = filepath.Join("..", "..", "..", "qube")
	}
	return filepath.Join(repo, "priv", "openapi", "generated", "qbxml_operations.json")
}

// The subset matches the qube checkout's generated spec, when there is one beside this
// repository (or QUBE_REPO names one).
func TestSpecFixtureIsCurrent(t *testing.T) {
	data, err := ioutil.ReadFile(generatedSpec())
	if err != nil {
		t.Skipf("no qube checkout (set QUBE_REPO): %v", err)
	}
	want, err := extractOps(data, fixtureOps)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := ioutil.WriteFile(subsetPath, want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	got, _ := ioutil.ReadFile(subsetPath)
	if !bytes.Equal(lf(got), want) {
		t.Fatalf("%s is out of date with %s: run `go test ./internal/ops -update`", subsetPath, generatedSpec())
	}
}

// The subset's help passes the check, and is what testdata/help.golden shows.
func TestFixtureHelp(t *testing.T) {
	data, err := ioutil.ReadFile(subsetPath)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Ops) != len(fixtureOps) {
		t.Fatalf("the subset has %d operations, want %d", len(ix.Ops), len(fixtureOps))
	}
	checkSpec(t, ix)

	var b bytes.Buffer
	for i := range ix.Ops {
		op := &ix.Ops[i]
		fmt.Fprintf(&b, "$ qube qb %s %s --help\n", op.Resource, op.Verb)
		op.WriteHelp(&b, "qube qb")
		if ex := op.ExampleJSON(); ex != nil {
			fmt.Fprintf(&b, "\n$ qube qb %s %s --example\n%s\n", op.Resource, op.Verb, ex)
		}
		b.WriteString("\n")
	}
	if *update {
		if err := ioutil.WriteFile(helpGolden, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := ioutil.ReadFile(helpGolden)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != string(lf(want)) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(lf(want)), "\n")
		for i := 0; i < len(gl) || i < len(wl); i++ {
			g, w := "", ""
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("help differs from %s at line %d (run `go test ./internal/ops -update` if the change is meant):\n got  %q\n want %q", helpGolden, i+1, g, w)
			}
		}
	}
}

// lf undoes a checkout's CRLF line endings.
func lf(b []byte) []byte { return bytes.Replace(b, []byte("\r\n"), []byte("\n"), -1) }

// extractOps is a spec cut down to some of its operations, as indented JSON: each with what
// the CLI reads of it (responses go, and callbacks keep only their names, `answered` being
// what marks a QuickBooks operation), and the components they refer to.
func extractOps(spec []byte, ids []string) ([]byte, error) {
	var doc map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(spec))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	paths, _ := doc["paths"].(map[string]interface{})
	outPaths := map[string]interface{}{}
	for path, item := range paths {
		itemMap, _ := item.(map[string]interface{})
		outItem := map[string]interface{}{}
		for method, raw := range itemMap {
			op, ok := raw.(map[string]interface{})
			id, _ := op["operationId"].(string)
			if !ok || !want[id] {
				continue
			}
			delete(want, id)
			slim := map[string]interface{}{}
			for k, v := range op {
				switch k {
				case "responses":
				case "callbacks":
					names := map[string]interface{}{}
					cbs, _ := v.(map[string]interface{})
					for name := range cbs {
						names[name] = map[string]interface{}{}
					}
					slim[k] = names
				default:
					slim[k] = v
				}
			}
			outItem[method] = slim
		}
		if len(outItem) > 0 {
			if shared, ok := itemMap["parameters"]; ok {
				outItem["parameters"] = shared
			}
			outPaths[path] = outItem
		}
	}
	if len(want) > 0 {
		var missing []string
		for id := range want {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("the spec has no operation %s: change fixtureOps", strings.Join(missing, ", "))
	}
	components, _ := doc["components"].(map[string]interface{})
	outComponents := map[string]interface{}{}
	pending := refsIn(outPaths)
	for len(pending) > 0 {
		ref := pending[0]
		pending = pending[1:]
		parts := strings.Split(strings.TrimPrefix(ref, "#/components/"), "/")
		if len(parts) != 2 {
			continue
		}
		section, _ := components[parts[0]].(map[string]interface{})
		target, ok := section[parts[1]]
		if !ok {
			continue
		}
		out, _ := outComponents[parts[0]].(map[string]interface{})
		if out == nil {
			out = map[string]interface{}{}
			outComponents[parts[0]] = out
		}
		if _, done := out[parts[1]]; done {
			continue
		}
		out[parts[1]] = target
		pending = append(pending, refsIn(target)...)
	}
	info, _ := doc["info"].(map[string]interface{})
	out := map[string]interface{}{
		"openapi": doc["openapi"],
		"info": map[string]interface{}{
			"title":       info["title"],
			"version":     info["version"],
			"description": "Some operations of the qube app's priv/openapi/generated/qbxml_operations.json, cut down to what the CLI reads. Regenerate it with `go test ./internal/ops -update`; do not edit it.",
		},
		"paths": outPaths,
	}
	if len(outComponents) > 0 {
		out["components"] = outComponents
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func refsIn(v interface{}) []string {
	var out []string
	switch x := v.(type) {
	case map[string]interface{}:
		if ref, ok := x["$ref"].(string); ok {
			out = append(out, ref)
		}
		for _, k := range sortedKeys(x) {
			out = append(out, refsIn(x[k])...)
		}
	case []interface{}:
		for _, item := range x {
			out = append(out, refsIn(item)...)
		}
	}
	return out
}

// ---------------------------------------------------------------- the check

func checkSpec(t *testing.T, ix *Index) {
	t.Helper()
	for i := range ix.Ops {
		op := &ix.Ops[i]
		var b bytes.Buffer
		op.WriteHelp(&b, "qube qb")
		help := b.String()
		where := op.Resource + " " + op.Verb
		checkRendered(t, where, op, help)
		checkExample(t, where, op, help)
		checkHints(t, where, op, help)
	}
}

var urlPattern = regexp.MustCompile(`https?://[^\s)\]>"'` + "`" + `]+`)

// checkRendered: no Markdown left raw, every URL whole, every line within the width unless
// a word too long for any line (a URL) or an example command makes it longer.
func checkRendered(t *testing.T, where string, op *Op, help string) {
	t.Helper()
	for _, raw := range []string{"](", "`", "**"} {
		if strings.Contains(help, raw) {
			t.Errorf("%s: help has raw Markdown %q:\n%s", where, raw, around(help, raw))
		}
	}
	urls := map[string]bool{}
	for _, s := range stringsOf(op) {
		for _, u := range urlPattern.FindAllString(s, -1) {
			urls[u] = true
		}
	}
	for _, line := range strings.Split(help, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("%s: a raw Markdown heading: %q", where, line)
		}
		for _, u := range urlPattern.FindAllString(line, -1) {
			if !urls[u] {
				t.Errorf("%s: a URL that is not one of the spec's (cut, or broken across lines): %q", where, u)
			}
		}
		if utf8.RuneCountInString(line) > helpWidth && !strings.HasPrefix(line, "  qube qb ") && longestWord(line) <= 40 {
			t.Errorf("%s: a line longer than %d: %q", where, helpWidth, line)
		}
	}
}

// checkExample: --example prints the spec's example, and the help's example command binds
// to that same body (or, when it shows the body shortened, the --example file does).
func checkExample(t *testing.T, where string, op *Op, help string) {
	t.Helper()
	i := strings.Index(help, "\nExample:\n")
	if i < 0 {
		if len(op.Example) > 0 {
			t.Errorf("%s: the spec has an example; help shows none", where)
		}
		return
	}
	line := strings.SplitN(help[i+len("\nExample:\n"):], "\n", 2)[0]
	args := shellWords(line)
	prefix := []string{"qube", "qb", op.Resource, op.Verb, "<connection>"}
	if len(args) < len(prefix) || !reflect.DeepEqual(args[:len(prefix)], prefix) {
		t.Errorf("%s: the example is not a command for it: %q", where, line)
		return
	}
	args = append([]string{"conn"}, args[len(prefix):]...)
	if len(op.Example) == 0 { // a query's: its required parameters, when none is a placeholder
		if !strings.Contains(strings.Join(args, " "), "<") {
			if _, err := op.Bind(args, nil, nil); err != nil {
				t.Errorf("%s: the example %q does not bind: %v", where, line, err)
			}
		}
		return
	}
	var want, printed interface{}
	if err := decodeJSON(op.Example, &want); err != nil {
		t.Errorf("%s: the spec's example is not JSON: %v", where, err)
		return
	}
	if err := decodeJSON(op.ExampleJSON(), &printed); err != nil || !reflect.DeepEqual(printed, want) {
		t.Errorf("%s: --example prints %s, want the spec's example", where, op.ExampleJSON())
	}
	if strings.Contains(help, "That body is shortened") {
		if !strings.Contains(help, op.Resource+" "+op.Verb+" --example > ") {
			t.Errorf("%s: a shortened example that doesn't say how to get the whole one", where)
		}
		args = []string{"conn", "--data", string(op.ExampleJSON())}
	}
	call, err := op.Bind(args, nil, nil)
	if err != nil {
		t.Errorf("%s: the example does not bind: %v", where, err)
		return
	}
	var got interface{}
	if err := decodeJSON(call.Body, &got); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("%s: the example binds to %s, want the spec's example %s", where, call.Body, op.Example)
	}
}

// checkHints: every field of a choice is listed in a block of alternatives apart from the
// fields it excludes, or shows the rule beside its flag naming them; every list of a
// repeating choice says what it combines with; an object's required keys are named. The
// fields are found in the schema directly (and, where only the generator's prose says it,
// in that prose), and the blocks in the rendered text, not by the code under test.
func checkHints(t *testing.T, where string, op *Op, help string) {
	t.Helper()
	var body map[string]interface{}
	_ = json.Unmarshal(op.Body, &body)
	blocks := choiceBlocks(help)
	for member, excluded := range choiceMembers(body) {
		flag := flagRef(member)
		block := flagBlock(help, flag)
		at, inBlock := blocks[flag]
		if !inBlock && !hasChoiceHint(block) {
			t.Errorf("%s: %s is in a choice; its help says nothing of it: %q", where, flag, block)
		}
		for _, x := range excluded {
			other, ok := blocks[flagRef(x)]
			apart := inBlock && ok && other.block == at.block && other.alt != at.alt
			if !apart && !mentions(block, flagRef(x)) {
				t.Errorf("%s: %s excludes %s; its help doesn't say: %q", where, flag, flagRef(x), block)
			}
		}
	}
	props, _ := body["properties"].(map[string]interface{})
	fields := map[string]map[string]interface{}{}
	for name, p := range props {
		fields[name], _ = p.(map[string]interface{})
	}
	for _, p := range op.Query {
		var s map[string]interface{}
		_ = json.Unmarshal(p.Schema, &s)
		if s == nil {
			s = map[string]interface{}{"description": p.Description}
		} else {
			s["description"] = p.Description
		}
		fields[p.Name] = s
	}
	for name, s := range fields {
		block := flagBlock(help, flagRef(name))
		if block == "" {
			continue // a name a flag can't have (--data); --data sets it
		}
		desc, _ := s["description"].(string)
		if strings.Contains(desc, "Choose at most one of:") || strings.Contains(desc, "Choose exactly one of:") {
			if _, inBlock := blocks[flagRef(name)]; !inBlock && !hasChoiceHint(block) {
				t.Errorf("%s: %s's description names a choice; its help shows none: %q", where, flagRef(name), block)
			}
		}
		if strings.Contains(desc, "any number of each") && !strings.Contains(block, "can be combined with") {
			t.Errorf("%s: %s's description says it combines; its help doesn't: %q", where, flagRef(name), block)
		}
		object := s
		if items, ok := s["items"].(map[string]interface{}); ok {
			object = items
		}
		for member := range choiceMembers(object) {
			if !mentions(block, member) || !hasChoiceHint(block) {
				t.Errorf("%s: %s's %q is in a choice; its help doesn't say: %q", where, flagRef(name), member, block)
			}
		}
		if req := strs(object["required"]); len(req) > 0 && typeOf(object) == "object" {
			if !strings.Contains(strings.ToLower(block), "required: ") {
				t.Errorf("%s: %s's keys %v are required; its help doesn't say: %q", where, flagRef(name), req, block)
			}
			for _, k := range req {
				if !strings.Contains(block, k) {
					t.Errorf("%s: %s's key %q is required; its help doesn't say: %q", where, flagRef(name), k, block)
				}
			}
		}
	}
}

// mentions says whether text names a field or flag as a whole word.
func mentions(text, name string) bool {
	return regexp.MustCompile(`(^|[^a-z0-9_-])` + regexp.QuoteMeta(name) + `($|[^a-z0-9_-])`).MatchString(text)
}

func hasChoiceHint(block string) bool {
	return strings.Contains(block, "one of:") || strings.Contains(block, "not with ")
}

// choiceMembers are an object schema's fields that are in a choice, each with the fields it
// excludes: the pairs of a `not` rule, the fields of its "not none of these" entries, and
// the alternatives of a oneOf.
func choiceMembers(schema map[string]interface{}) map[string][]string {
	out := map[string][]string{}
	if not, ok := schema["not"].(map[string]interface{}); ok {
		patterns, _ := not["anyOf"].([]interface{})
		for _, p := range patterns {
			pat, _ := p.(map[string]interface{})
			if req := strs(pat["required"]); len(req) == 2 {
				out[req[0]] = append(out[req[0]], req[1])
				out[req[1]] = append(out[req[1]], req[0])
			}
			props, _ := pat["properties"].(map[string]interface{})
			for name, v := range props {
				if v == false {
					out[name] = append(out[name], []string{}...)
				}
			}
		}
	}
	branches, _ := schema["oneOf"].([]interface{})
	var alts [][]string
	for _, b := range branches {
		branch, _ := b.(map[string]interface{})
		props, _ := branch["properties"].(map[string]interface{})
		alts = append(alts, sortedKeys(props))
	}
	for i, alt := range alts {
		for _, f := range alt {
			out[f] = append(out[f], []string{}...)
			for j, other := range alts {
				if i != j {
					out[f] = append(out[f], other...)
				}
			}
		}
	}
	return out
}

// flagBlock is a flag's entry in help, its lines joined by single spaces: the line the flag
// starts (at any indent) and those indented further below it.
func flagBlock(help, flag string) string {
	lines := strings.Split(help, "\n")
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " ")
		indent := len(l) - len(trimmed)
		if indent < 2 || trimmed != flag && !strings.HasPrefix(trimmed, flag+" ") {
			continue
		}
		block := []string{l}
		for _, next := range lines[i+1:] {
			if len(next)-len(strings.TrimLeft(next, " ")) <= indent || strings.TrimSpace(next) == "" {
				break
			}
			block = append(block, next)
		}
		return strings.Join(strings.Fields(strings.Join(block, " ")), " ")
	}
	return ""
}

// blockAt is where a flag sits in help's choice blocks: which block, which alternative.
type blockAt struct{ block, alt int }

var (
	blockHeading = regexp.MustCompile(`^  (Exactly|At most) one of`)
	blockFlag    = regexp.MustCompile(`^    (--[a-z0-9-]+)`)
)

// choiceBlocks reads the rendered help's choice blocks: a heading ("  Exactly one of..."),
// then alternatives of flags at an indent of 4, separated by "  or:". Any other line at an
// indent of 2, or a blank one, ends a block.
func choiceBlocks(help string) map[string]blockAt {
	out := map[string]blockAt{}
	block, alt := 0, -1
	for _, l := range strings.Split(help, "\n") {
		switch {
		case blockHeading.MatchString(l):
			block, alt = block+1, 0
		case alt < 0:
		case l == "  or:":
			alt++
		case blockFlag.MatchString(l):
			out[blockFlag.FindStringSubmatch(l)[1]] = blockAt{block, alt}
		case strings.TrimSpace(l) == "" || strings.HasPrefix(l, "  ") && !strings.HasPrefix(l, "   "):
			alt = -1
		}
	}
	return out
}

// stringsOf is every string in an operation: its descriptions, its schemas' and examples'.
func stringsOf(op *Op) []string {
	data, _ := json.Marshal(op)
	var v interface{}
	_ = json.Unmarshal(data, &v)
	var out []string
	var walk func(interface{})
	walk = func(v interface{}) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case map[string]interface{}:
			for _, y := range x {
				walk(y)
			}
		case []interface{}:
			for _, y := range x {
				walk(y)
			}
		}
	}
	walk(v)
	return out
}

func longestWord(line string) int {
	n := 0
	for _, w := range strings.Fields(line) {
		if l := utf8.RuneCountInString(w); l > n {
			n = l
		}
	}
	return n
}

func around(s, sub string) string {
	i := strings.Index(s, sub)
	start, end := i-60, i+60
	if start < 0 {
		start = 0
	}
	if end > len(s) {
		end = len(s)
	}
	return s[start:end]
}

// shellWords splits a POSIX shell command line into its words: single quotes (a quote inside
// them is written close, \', reopen), double quotes and backslashes, enough for the examples
// help prints.
func shellWords(line string) []string {
	var out []string
	var w strings.Builder
	in := false
	quote := byte(0)
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				w.WriteByte(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(line) {
				i++
				w.WriteByte(line[i])
			} else {
				w.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, in = c, true
		case c == '\\' && i+1 < len(line):
			i++
			w.WriteByte(line[i])
			in = true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, w.String())
				w.Reset()
				in = false
			}
		default:
			w.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, w.String())
	}
	return out
}
