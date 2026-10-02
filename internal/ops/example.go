package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

// An operation's example is the spec's own: the request body example it publishes for the
// operation (`requestBody.content["application/json"].example`), or for a query, its
// required parameters. Nothing here is written per operation.

// ExampleJSON is the operation's example request body, indented, or nil when it has none.
// `qube qb <resource> <verb> --example` prints it.
func (op *Op) ExampleJSON() []byte {
	if len(op.Example) == 0 {
		return nil
	}
	var b bytes.Buffer
	if json.Indent(&b, op.Example, "", "  ") != nil {
		return nil
	}
	return b.Bytes()
}

// exampleInline is how long a body may be to show whole on the example's command line;
// a longer one is shortened there, and --example prints it whole.
const exampleInline = 100

// writeExample ends help with a command that runs the operation on the spec's example.
func (op *Op) writeExample(w io.Writer, prog string) {
	cmd := prog + " " + op.Resource + " " + op.Verb
	args := argList(op.PathParams)
	if len(op.Example) > 0 {
		var compact bytes.Buffer
		if json.Compact(&compact, op.Example) != nil {
			return
		}
		fmt.Fprintln(w, "\nExample:")
		if utf8.RuneCount(compact.Bytes()) <= exampleInline {
			fmt.Fprintf(w, "  %s %s --data %s\n", cmd, args, shellQuote(compact.String()))
			return
		}
		file := exampleFile(op)
		var schema struct {
			Required []string `json:"required"`
		}
		_ = json.Unmarshal(op.Body, &schema)
		fmt.Fprintf(w, "  %s %s --data %s\n", cmd, args, shellQuote(shortenJSON(op.Example, exampleInline, schema.Required)))
		fmt.Fprintf(w, "\n  That body is shortened: {…} and […] stand for more. The whole of it, to edit and send:\n")
		fmt.Fprintf(w, "  %s --example > %s\n", cmd, file)
		fmt.Fprintf(w, "  %s %s --data @%s\n", cmd, args, file)
		return
	}
	var flags []string
	for _, f := range op.Flags() {
		if !f.Param.Required || f.Body {
			continue
		}
		flags = append(flags, "--"+f.Name)
		if v := exampleValue(f.Param); v != "" {
			flags = append(flags, v)
		}
	}
	if len(flags) > 0 {
		fmt.Fprintln(w, "\nExample:")
		fmt.Fprintf(w, "  %s %s %s\n", cmd, args, strings.Join(flags, " "))
	}
}

// exampleValue is a query parameter's value in an example: the spec's example for it, else
// its first allowed value, else a placeholder naming its type (none for a boolean, which is
// true alone).
func exampleValue(p Param) string {
	if len(p.Example) > 0 {
		var s string
		if json.Unmarshal(p.Example, &s) == nil {
			return shellQuote(s)
		}
		var b bytes.Buffer
		if json.Compact(&b, p.Example) == nil {
			return shellQuote(b.String())
		}
	}
	switch {
	case len(p.Enum) > 0:
		return shellQuote(p.Enum[0])
	case p.Type == "boolean":
		return ""
	}
	return "<" + strings.TrimSuffix(typeHint(p), "...") + ">"
}

// exampleFile names the file --example is saved to, after the operation: createInvoice
// saves invoice.json.
func exampleFile(op *Op) string {
	if rest := kebab(strings.TrimPrefix(op.OperationID, leadingWord(op.OperationID))); rest != "" {
		return rest + ".json"
	}
	return "body.json"
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_./:@%+=,-]+$`)

// shellQuote quotes a word for a POSIX shell.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.Replace(s, "'", `'\''`, -1) + "'"
}

// ---------------------------------------------------------------- shortening

// jnode is a JSON value that keeps its object keys in the order written.
type jnode struct {
	kind    byte // 's' scalar, 'o' object, 'a' array
	scalar  string
	keys    []string
	kids    []*jnode
	elided  bool   // shown as {…} or […]
	omitted []bool // which of its members are left out, each run of them shown as …
}

// shortenJSON writes a JSON value compactly in at most about max characters, leaving out the
// contents of its largest objects and lists first ({…}, […]) and then, if need be, top-level
// members from the last: those not in keep (the body's required fields) before those in it.
func shortenJSON(raw []byte, max int, keep []string) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	root, err := readJNode(dec)
	if err != nil {
		return string(raw)
	}
	for utf8.RuneCountInString(root.String()) > max {
		var best *jnode
		bestLen := 0
		root.walk(func(n *jnode) {
			if n != root && n.kind != 's' && !n.elided && len(n.kids) > 0 {
				if l := utf8.RuneCountInString(n.String()); l > bestLen {
					best, bestLen = n, l
				}
			}
		})
		if best == nil {
			break
		}
		best.elided = true
	}
	if root.kind == 's' {
		return root.String()
	}
	root.omitted = make([]bool, len(root.kids))
	for _, kept := range []bool{false, true} {
		for i := len(root.kids) - 1; i > 0 && utf8.RuneCountInString(root.String()) > max; i-- {
			if root.kind == 'o' && contains(keep, root.keys[i]) == kept {
				root.omitted[i] = true
			} else if root.kind == 'a' && kept {
				root.omitted[i] = true
			}
		}
	}
	return root.String()
}

func readJNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		s, err := marshalText(tok)
		return &jnode{kind: 's', scalar: s}, err
	}
	n := &jnode{kind: 'a'}
	if d == '{' {
		n.kind = 'o'
	}
	for dec.More() {
		if n.kind == 'o' {
			key, err := dec.Token()
			if err != nil {
				return nil, err
			}
			n.keys = append(n.keys, fmt.Sprint(key))
		}
		kid, err := readJNode(dec)
		if err != nil {
			return nil, err
		}
		n.kids = append(n.kids, kid)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return n, nil
}

// marshalText writes a JSON scalar without escaping <, > and & (help is not HTML).
func marshalText(v interface{}) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

func (n *jnode) walk(fn func(*jnode)) {
	fn(n)
	if n.elided {
		return
	}
	for _, k := range n.kids {
		k.walk(fn)
	}
}

func (n *jnode) String() string {
	var b strings.Builder
	n.write(&b)
	return b.String()
}

func (n *jnode) write(b *strings.Builder) {
	if n.kind == 's' {
		b.WriteString(n.scalar)
		return
	}
	open, close := "[", "]"
	if n.kind == 'o' {
		open, close = "{", "}"
	}
	if n.elided {
		b.WriteString(open + "…" + close)
		return
	}
	b.WriteString(open)
	for i, k := range n.kids {
		if n.omitted != nil && n.omitted[i] {
			if !n.omitted[i-1] {
				b.WriteString(",…")
			}
			continue
		}
		if i > 0 {
			b.WriteByte(',')
		}
		if n.kind == 'o' {
			key, _ := marshalText(n.keys[i])
			b.WriteString(key + ":")
		}
		k.write(b)
	}
	b.WriteString(close)
}
