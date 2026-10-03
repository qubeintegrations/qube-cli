package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"unicode/utf8"
)

// Usage is the operation's synopsis: `qube qb customers list <connection> [flags]`.
func (op *Op) Usage(prog string) string {
	s := prog + " " + op.Resource + " " + op.Verb
	if len(op.PathParams) > 0 {
		s += " " + argList(op.PathParams)
	}
	return s + " [flags]"
}

// helpWidth is the width help is wrapped to.
const helpWidth = 96

// WriteHelp describes the operation and every flag it takes, and ends with an example.
func (op *Op) WriteHelp(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage: %s\n\n", op.Usage(prog))
	if op.Summary != "" {
		fmt.Fprintf(w, "%s.  %s /api/v2%s\n", strings.TrimSuffix(op.Summary, "."), op.Method, op.Path)
	}
	if d := strings.TrimSpace(op.Description); d != "" {
		short, cut := brief(d, 700)
		fmt.Fprintf(w, "\n%s\n", renderMarkdown(short, helpWidth))
		if cut {
			fmt.Fprintln(w, "(--help --json has the whole description.)")
		}
	}
	var query, body []Flag
	for _, f := range op.Flags() {
		if f.Body {
			body = append(body, f)
		} else {
			query = append(query, f)
		}
	}
	if len(query) > 0 {
		fmt.Fprintln(w, "\nFlags:")
		writeFlags(w, query, queryRules(op.Query), nil)
	}
	if len(op.Body) > 0 {
		req := ""
		if op.BodyRequired {
			req = ", required"
		}
		fmt.Fprintf(w, "\nBody (JSON%s): --data JSON|@file|-, and/or its fields as flags (a flag wins):\n", req)
		writeFlags(w, body, op.bodyRules(), op.exampleFields())
	}
	fmt.Fprintln(w)
	own := []Flag{{Name: "wait[=10m]", Param: Param{Type: "boolean", Description: "Wait here for QuickBooks' answer (every page of an iterated query) and print it."}}}
	if len(op.Example) > 0 {
		own = append(own, Flag{Name: "example", Param: Param{Type: "boolean", Description: "Print the example body below as JSON, to edit and send with --data @file."}})
	}
	writeFlags(w, own, rules{}, nil)
	fmt.Fprintln(w, "\n--help --json prints the operation's full parameters and body schema, as JSON.")
	op.writeExample(w, prog)
}

// bodyRules are the rules among the body's top-level fields.
func (op *Op) bodyRules() rules {
	var schema map[string]interface{}
	if json.Unmarshal(op.Body, &schema) != nil {
		return rules{}
	}
	return rulesOf(schema)
}

// The flag column is as wide as the longest flag (with its indent) up to this; a longer
// flag has its description start on the next line.
const maxFlagWidth = 34

// entry is one entry of a flag list: a flag, a field with no flag of its own (a body field
// named like one of qube's flags), or a heading.
type entry struct {
	indent  int
	name    string // "--flag TYPE", a JSON key, or a heading's text
	flag    *Flag
	heading bool
	ruled   bool // the flag is in a choice block, which states the rule its description's note does
}

// writeFlags writes the flags that stand alone, then each exclusive choice among them as a
// block: "Exactly one of:" (or "At most one of:"), then each alternative's flags, the
// alternatives separated by "or:". A reader sees which flags go together and which exclude
// each other without piecing it together flag by flag. Every description is wrapped in one
// column. examples are the spec's example values of body fields, by name.
func writeFlags(w io.Writer, flags []Flag, r rules, examples map[string]*jnode) {
	blocks, rest := r.split()
	byName := map[string]*Flag{}
	for i := range flags {
		byName[flags[i].Param.Name] = &flags[i]
	}
	inBlock := map[string]bool{}
	for _, c := range blocks {
		for _, alt := range c.alts {
			for _, f := range alt {
				inBlock[f] = true
			}
		}
	}
	var entries []entry
	for i := range flags {
		if !inBlock[flags[i].Param.Name] {
			entries = append(entries, entry{indent: 2, name: flagLabel(flags[i]), flag: &flags[i]})
		}
	}
	for _, c := range blocks {
		entries = append(entries, entry{indent: 2, name: c.heading(), heading: true})
		for i, alt := range c.alts {
			if i > 0 {
				entries = append(entries, entry{indent: 2, name: "or:", heading: true})
			}
			for _, field := range alt {
				if f, ok := byName[field]; ok {
					entries = append(entries, entry{indent: 4, name: flagLabel(*f), flag: f, ruled: true})
				} else {
					entries = append(entries, entry{indent: 4, name: field})
				}
			}
		}
	}
	longest := 0
	for _, e := range entries {
		if n := e.indent + utf8.RuneCountInString(e.name); !e.heading && n > longest && n <= maxFlagWidth {
			longest = n
		}
	}
	col := longest + 2
	for _, e := range entries {
		var doc []string
		switch {
		case e.heading:
		case e.flag == nil:
			doc = []string{"(no flag of its own: set it with --data)"}
		default:
			p := e.flag.Param
			doc = flagDoc(p, rest.hints(p.Name, flagRef), e.ruled, examples[p.Name], helpWidth-col)
		}
		writeEntry(w, strings.Repeat(" ", e.indent)+e.name, doc, col)
	}
}

func flagLabel(f Flag) string { return strings.TrimSpace("--" + f.Name + " " + typeHint(f.Param)) }

// exampleFields are the top-level fields of the spec's example body, by name.
func (op *Op) exampleFields() map[string]*jnode {
	if len(op.Example) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(op.Example))
	dec.UseNumber()
	root, err := readJNode(dec)
	if err != nil || root.kind != 'o' {
		return nil
	}
	out := map[string]*jnode{}
	for i, k := range root.keys {
		out[k] = root.kids[i]
	}
	return out
}

// flagRef names a field by its flag.
func flagRef(apiName string) string { return "--" + flagName(apiName) }

// keyRef names a field by its JSON key.
func keyRef(apiName string) string { return apiName }

// writeEntry writes "  name  doc" (head carries its own indent), each of the doc's
// paragraphs wrapped in the column that starts at col.
func writeEntry(w io.Writer, head string, doc []string, col int) {
	pad := strings.Repeat(" ", col)
	var lines []string
	for _, para := range doc {
		lines = append(lines, wrapText(para, helpWidth, pad, pad)...)
	}
	switch {
	case len(lines) == 0:
		fmt.Fprintln(w, head)
		return
	case utf8.RuneCountInString(head)+2 <= col:
		fmt.Fprintln(w, head+lines[0][utf8.RuneCountInString(head):])
		lines = lines[1:]
	default:
		fmt.Fprintln(w, head)
	}
	for _, l := range lines {
		fmt.Fprintln(w, l)
	}
}

func typeHint(p Param) string {
	switch p.Type {
	case "boolean":
		return ""
	case "array":
		item := p.Items
		if item == "" {
			item = "value"
		}
		return strings.ToUpper(item) + "..."
	case "string":
		switch p.Format {
		case "date":
			return "DATE"
		case "date-time":
			return "DATETIME"
		case "uri":
			return "URL"
		}
	case "":
		return "VALUE"
	}
	return strings.ToUpper(p.Type)
}

// flagDoc is a flag's description, as paragraphs: required and the first sentence of its
// description; the rules it takes part in; its values; and an object's shape, in width.
// ruled says a choice block around the flag states its rule.
func flagDoc(p Param, hints []string, ruled bool, example *jnode, width int) []string {
	var out []string
	first := docSentence(p.Description, ruled || len(hints) > 0)
	if p.Required {
		first = strings.TrimSpace("(required) " + first)
	}
	if first != "" {
		out = append(out, first)
	}
	if len(hints) > 0 {
		out = append(out, "("+strings.Join(hints, "; ")+")")
	}
	switch {
	case len(p.Enum) > 0:
		out = append(out, "One of: "+enumList(p.Enum)+".")
	case p.Type == "object" || p.Items == "object":
		if shape := shapeDoc(p.Schema, p.Type == "array", example, width); shape != "" {
			out = append(out, shape)
		}
	}
	if p.Type == "array" {
		const repeat = "(repeat the flag, or a JSON array)"
		if n := len(out); n > 0 && !strings.HasPrefix(out[n-1], "(") {
			out[n-1] += " " + repeat
		} else {
			out = append(out, repeat)
		}
	}
	return out
}

// docSentence is the first sentence of a description, rendered and at most 200 characters.
// With the rules shown (as hints, or by a choice block), the paragraphs the generator writes
// to state those same rules are passed over (the help says it shorter).
func docSentence(desc string, ruled bool) string {
	for _, b := range parseBlocks(desc) {
		if b.kind == codeBlock || ruled && generatedNote(b.text) {
			continue
		}
		if s := sentences(b.text); len(s) > 0 {
			return clip(renderInline(s[0]), 200)
		}
	}
	return ""
}

// generatedNote says whether a paragraph is one of the generator's choice notes (see
// rules.go), which hints restate.
func generatedNote(text string) bool {
	return choiceNote.MatchString(text) || combineNote.MatchString(text) || requiredCombineNote.MatchString(text)
}

func enumList(values []string) string {
	if len(values) <= 8 {
		return strings.Join(values, ", ")
	}
	return strings.Join(values[:8], ", ") + fmt.Sprintf(", … (%d; --help --json lists them)", len(values))
}

// shapeDoc shows an object's fields and its rules: `JSON: {"from", "to"}`, or for a list
// `JSON: [{"account_ref", …}, ...]. In each, required: account_ref; at most one of: rate |
// rate_percent.` The fields it requires come first, then those the spec's example sets,
// then the rest, as many as fit on a line of width.
func shapeDoc(schema json.RawMessage, array bool, example *jnode, width int) string {
	var s map[string]interface{}
	if json.Unmarshal(schema, &s) != nil {
		return ""
	}
	if array {
		s, _ = s["items"].(map[string]interface{})
		if example != nil && example.kind == 'a' && len(example.kids) > 0 {
			example = example.kids[0]
		}
	}
	fields := objectFields(s)
	if len(fields) == 0 {
		return ""
	}
	required := strs(s["required"])
	ordered := append([]string{}, required...)
	if example != nil && example.kind == 'o' {
		for _, k := range example.keys {
			if contains(fields, k) && !contains(ordered, k) {
				ordered = append(ordered, k)
			}
		}
	}
	for _, f := range fields {
		if !contains(ordered, f) {
			ordered = append(ordered, f)
		}
	}
	open, close := "JSON: {", "}"
	if array {
		open, close = "JSON: [{", "}, ...]"
	}
	room := width - len(open) - len(close) - len(", …")
	var keys []string
	n := 0
	for i, k := range ordered {
		q := fmt.Sprintf("%q", k)
		if len(keys) > 0 && (n+2+len(q) > room && i < len(ordered)-1 || n+2+len(q) > room+len(", …")) {
			keys = append(keys, "…")
			break
		}
		keys = append(keys, q)
		n += len(q) + 2
	}
	out := open + strings.Join(keys, ", ") + close
	var notes []string
	if len(required) > 0 {
		notes = append(notes, "required: "+strings.Join(required, ", "))
	}
	notes = append(notes, rulesOf(s).all(keyRef)...)
	if len(notes) == 0 {
		return out
	}
	rest := strings.Join(notes, "; ")
	if array {
		return out + ". In each, " + rest + "."
	}
	return out + ". " + strings.ToUpper(rest[:1]) + rest[1:] + "."
}

// objectFields are an object's fields: its properties and those of its oneOf alternatives.
func objectFields(s map[string]interface{}) []string {
	props, _ := s["properties"].(map[string]interface{})
	all := map[string]interface{}{}
	for k, v := range props {
		all[k] = v
	}
	branches, _ := s["oneOf"].([]interface{})
	for _, b := range branches {
		branch, _ := b.(map[string]interface{})
		bp, _ := branch["properties"].(map[string]interface{})
		for k, v := range bp {
			if _, ok := all[k]; !ok {
				all[k] = v
			}
		}
	}
	return sortedKeys(all)
}

// Summary is a one-line view of an operation, for listings and --json.
type Summary struct {
	Resource    string `json:"resource"`
	Verb        string `json:"verb"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id"`
	Summary     string `json:"summary"`
}

func (op *Op) Summarize() Summary {
	return Summary{Resource: op.Resource, Verb: op.Verb, Method: op.Method, Path: "/api/v2" + op.Path, OperationID: op.OperationID, Summary: op.Summary}
}

// WriteResources lists every resource with its verbs.
func (ix *Index) WriteResources(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "RESOURCE\tVERBS")
	for _, r := range ix.Resources() {
		var verbs []string
		for _, op := range ix.Of(r) {
			verbs = append(verbs, op.Verb)
		}
		fmt.Fprintf(tw, "%s\t%s\n", r, strings.Join(verbs, " "))
	}
	tw.Flush()
}

// WriteVerbs lists one resource's operations.
func (ix *Index) WriteVerbs(w io.Writer, resource string) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "VERB\tMETHOD\tSUMMARY")
	for _, op := range ix.Of(resource) {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", op.Verb, op.Method, op.Summary)
	}
	tw.Flush()
}

// Suggest names up to three resources that look like the one asked for.
func (ix *Index) Suggest(resource string) []string {
	var out []string
	want := strings.TrimSuffix(strings.ToLower(resource), "s")
	for _, r := range ix.Resources() {
		if strings.Contains(r, want) || strings.Contains(want, strings.TrimSuffix(r, "s")) {
			out = append(out, r)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}
