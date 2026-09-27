package ops

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Usage is the operation's synopsis: `qube qb customers list <connection> [flags]`.
func (op *Op) Usage(prog string) string {
	s := prog + " " + op.Resource + " " + op.Verb
	if len(op.PathParams) > 0 {
		s += " " + argList(op.PathParams)
	}
	return s + " [flags]"
}

// WriteHelp describes the operation and every flag it takes.
func (op *Op) WriteHelp(w io.Writer, prog string) {
	fmt.Fprintf(w, "Usage: %s\n\n", op.Usage(prog))
	if op.Summary != "" {
		fmt.Fprintf(w, "%s.  %s /api/v2%s\n", strings.TrimSuffix(op.Summary, "."), op.Method, op.Path)
	}
	if d := strings.TrimSpace(op.Description); d != "" {
		short, cut := brief(d, 700)
		fmt.Fprintf(w, "\n%s\n", wrap(short, 96))
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
		writeFlags(w, query)
	}
	if len(op.Body) > 0 {
		req := ""
		if op.BodyRequired {
			req = ", required"
		}
		fmt.Fprintf(w, "\nBody (JSON%s): --data JSON|@file|- , and/or its fields as flags (a flag wins):\n", req)
		writeFlags(w, body)
	}
	fmt.Fprintln(w, "\n  --wait[=10m]  wait here for QuickBooks' answer (every page of an iterated query) and print it")
	fmt.Fprintf(w, "\nThe operation's full parameters and body schema, as JSON: %s %s %s --help --json\n", prog, op.Resource, op.Verb)
}

func writeFlags(w io.Writer, flags []Flag) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, f := range flags {
		fmt.Fprintf(tw, "  --%s %s\t%s\n", f.Name, typeHint(f.Param), flagDoc(f.Param))
	}
	tw.Flush()
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
	case "":
		return "VALUE"
	}
	return strings.ToUpper(p.Type)
}

// flagDoc is one line: required, the first sentence of the description, then the shape.
func flagDoc(p Param) string {
	var parts []string
	if p.Required {
		parts = append(parts, "(required)")
	}
	if s := firstSentence(p.Description); s != "" {
		parts = append(parts, s)
	}
	switch {
	case len(p.Enum) > 0:
		parts = append(parts, "One of: "+enumList(p.Enum)+".")
	case p.Type == "object" || p.Items == "object":
		if keys := objectKeys(p.Schema, p.Type == "array"); keys != "" {
			parts = append(parts, "JSON: "+keys)
		}
	}
	if p.Type == "array" {
		parts = append(parts, "(repeat the flag, or a JSON array)")
	}
	return strings.Join(parts, " ")
}

func firstSentence(s string) string {
	s = strings.TrimSpace(strings.Split(strings.TrimSpace(s), "\n")[0])
	if i := strings.Index(s, ". "); i >= 0 {
		s = s[:i+1]
	}
	if r := []rune(s); len(r) > 110 {
		s = string(r[:109]) + "…"
	}
	return s
}

func enumList(values []string) string {
	if len(values) <= 8 {
		return strings.Join(values, ", ")
	}
	return strings.Join(values[:8], ", ") + fmt.Sprintf(", … (%d; --help --json lists them)", len(values))
}

// objectKeys shows an object's fields: {"from", "to"}.
func objectKeys(schema json.RawMessage, array bool) string {
	var s struct {
		Properties map[string]interface{} `json:"properties"`
		Items      struct {
			Properties map[string]interface{} `json:"properties"`
		} `json:"items"`
	}
	if json.Unmarshal(schema, &s) != nil {
		return ""
	}
	props := s.Properties
	if array {
		props = s.Items.Properties
	}
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, fmt.Sprintf("%q", k))
	}
	sort.Strings(keys)
	if len(keys) > 6 {
		keys = append(keys[:6], "…")
	}
	out := "{" + strings.Join(keys, ", ") + "}"
	if array {
		out = "[" + out + ", ...]"
	}
	return out
}

// brief keeps whole paragraphs up to about max characters, and as much of the next one as
// fits in whole sentences. It reports whether it left anything out.
func brief(text string, max int) (string, bool) {
	var kept []string
	n := 0
	for _, p := range strings.Split(text, "\n\n") {
		if n+len(p) <= max {
			kept = append(kept, p)
			n += len(p) + 2
			continue
		}
		if room := max - n; room >= 150 {
			cut := p[:room]
			if i := strings.LastIndex(cut, ". "); i > 0 {
				cut = cut[:i+1]
			}
			kept = append(kept, cut+" …")
		}
		return strings.Join(kept, "\n\n"), true
	}
	return text, false
}

// wrap re-flows each paragraph to width, leaving lists and code lines as they are.
func wrap(text string, width int) string {
	var out []string
	for _, para := range strings.Split(text, "\n\n") {
		if strings.HasPrefix(para, "* ") || strings.HasPrefix(para, "- ") || strings.HasPrefix(para, "    ") || strings.Contains(para, "\n* ") {
			out = append(out, para)
			continue
		}
		var lines []string
		line := ""
		for _, word := range strings.Fields(para) {
			if line != "" && len(line)+1+len(word) > width {
				lines = append(lines, line)
				line = word
			} else if line == "" {
				line = word
			} else {
				line += " " + word
			}
		}
		if line != "" {
			lines = append(lines, line)
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n\n")
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
