package ops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Flag is one command-line flag of an operation: a query parameter, or a top-level field of
// the request body.
type Flag struct {
	Name  string // kebab-case, without dashes: max-returned
	Param Param  // Param.Name is the API's own name: max_returned
	Body  bool
}

// Flags lists the operation's flags: its query parameters, then its body's top-level fields.
func (op *Op) Flags() []Flag {
	var out []Flag
	// --data, --help and --wait are qube's own; the rest are its global flags, which never
	// reach an operation. A body field by one of these names is still set via --data.
	taken := map[string]bool{"data": true, "help": true, "wait": true, "json": true, "host": true, "app": true, "timeout": true, "yes": true}
	for _, p := range op.Query {
		name := flagName(p.Name)
		if !taken[name] {
			taken[name] = true
			out = append(out, Flag{Name: name, Param: p})
		}
	}
	for _, p := range op.BodyFields() {
		name := flagName(p.Name)
		if !taken[name] {
			taken[name] = true
			out = append(out, Flag{Name: name, Param: p, Body: true})
		}
	}
	return out
}

// BodyFields are the top-level properties of the request body, sorted, required ones first.
func (op *Op) BodyFields() []Param {
	if len(op.Body) == 0 {
		return nil
	}
	var schema struct {
		Properties map[string]map[string]interface{} `json:"properties"`
		Required   []string                          `json:"required"`
	}
	if json.Unmarshal(op.Body, &schema) != nil {
		return nil
	}
	required := map[string]bool{}
	for _, r := range schema.Required {
		required[r] = true
	}
	var out []Param
	for name, s := range schema.Properties {
		p := Param{Name: name, Required: required[name], Type: typeOf(s)}
		p.Description, _ = s["description"].(string)
		switch p.Type {
		case "array":
			items, _ := s["items"].(map[string]interface{})
			p.Items, p.Enum = typeOf(items), enumOf(items)
			if p.Items == "object" {
				p.Schema, _ = json.Marshal(s)
			}
		case "object":
			p.Schema, _ = json.Marshal(s)
		default:
			p.Enum = enumOf(s)
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Required != out[j].Required {
			return out[i].Required
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func flagName(apiName string) string { return strings.Replace(apiName, "_", "-", -1) }

// Call is a bound operation, ready to send.
type Call struct {
	Args   []string // the path parameters' values, in order: the connection first
	Method string
	Path   string // /api/v2/...
	Query  url.Values
	Body   []byte // nil: no body
}

// UsageError is a command line that doesn't fit the operation.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// UnknownFlagError is a flag the operation doesn't have. The host may have added it since
// the list was cached, so the caller may refresh and bind again.
type UnknownFlagError struct{ Flag string }

func (e *UnknownFlagError) Error() string { return "unknown flag --" + e.Flag }

func usagef(format string, a ...interface{}) error { return &UsageError{fmt.Sprintf(format, a...)} }

// Bind reads `<path params...> [--flag value...] [--data JSON|@file|-]` into a Call. stdin
// is read for `--data -` (or any `@-`). A flag may be given as --name value, --name=value,
// or with the API's own snake_case name; a boolean alone means true.
func (op *Op) Bind(args []string, stdin io.Reader) (*Call, error) {
	flags := map[string]Flag{}
	for _, f := range op.Flags() {
		flags[f.Name] = f
	}
	var positional []string
	var data string
	var haveData bool
	query := map[string]interface{}{}
	body := map[string]interface{}{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "--") {
			positional = append(positional, a)
			continue
		}
		name, value, hasValue := a[2:], "", false
		if eq := strings.IndexByte(name, '='); eq >= 0 {
			name, value, hasValue = name[:eq], name[eq+1:], true
		}
		name = flagName(name)
		if name == "data" {
			if !hasValue {
				if i+1 >= len(args) {
					return nil, usagef("--data needs a value: JSON, @file, or - for stdin")
				}
				value = args[i+1]
				i++
			}
			data, haveData = value, true
			continue
		}
		f, ok := flags[name]
		if !ok {
			return nil, &UnknownFlagError{Flag: name}
		}
		if !hasValue && f.Param.Type != "boolean" {
			if i+1 >= len(args) {
				return nil, usagef("--%s needs a value", name)
			}
			value, hasValue = args[i+1], true
			i++
		}
		target := query
		if f.Body {
			target = body
		}
		if err := setValue(target, f, value, hasValue, stdin); err != nil {
			return nil, err
		}
	}
	if len(positional) != len(op.PathParams) {
		return nil, usagef("%s takes %s (got %d argument%s)", op.Resource+" "+op.Verb, argList(op.PathParams), len(positional), plural(len(positional)))
	}
	call := &Call{Args: positional, Method: op.Method, Path: "/api/v2" + fill(op.Path, positional), Query: url.Values{}}
	keys := make([]string, 0, len(query))
	for k := range query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		encodeQuery(call.Query, k, query[k])
	}
	var err error
	call.Body, err = op.body(data, haveData, body, stdin)
	if err != nil {
		return nil, err
	}
	return call, nil
}

// body merges --data with the body-field flags; a flag wins over the same field in --data.
func (op *Op) body(data string, haveData bool, fields map[string]interface{}, stdin io.Reader) ([]byte, error) {
	if len(op.Body) == 0 {
		if haveData {
			return nil, usagef("%s %s takes no body (no --data)", op.Resource, op.Verb)
		}
		return nil, nil
	}
	if !haveData && len(fields) == 0 {
		if op.BodyRequired {
			return nil, usagef("%s %s needs a body: --data JSON|@file|-, or its fields as flags (see --help)", op.Resource, op.Verb)
		}
		return nil, nil
	}
	merged := map[string]interface{}{}
	if haveData {
		raw, err := readArg(data, stdin)
		if err != nil {
			return nil, err
		}
		var v interface{}
		if err := decodeJSON(raw, &v); err != nil {
			return nil, usagef("--data is not JSON: %v", err)
		}
		obj, isObj := v.(map[string]interface{})
		if !isObj {
			if len(fields) > 0 {
				return nil, usagef("--data must be a JSON object to combine it with field flags")
			}
			return raw, nil
		}
		merged = obj
	}
	for k, v := range fields {
		merged[k] = v
	}
	return json.Marshal(merged)
}

// setValue parses one flag's value by its type and stores it under the API's name. An
// array flag given more than once collects its values.
func setValue(target map[string]interface{}, f Flag, value string, hasValue bool, stdin io.Reader) error {
	p := f.Param
	switch p.Type {
	case "boolean":
		if !hasValue {
			target[p.Name] = true
			return nil
		}
		b, err := strconv.ParseBool(value)
		if err != nil {
			return usagef("--%s is true or false (got %q)", f.Name, value)
		}
		target[p.Name] = b
	case "array":
		if strings.HasPrefix(value, "[") || strings.HasPrefix(value, "@") {
			v, err := jsonArg(f.Name, value, stdin)
			if err != nil {
				return err
			}
			list, ok := v.([]interface{})
			if !ok {
				return usagef("--%s takes a JSON array, or one value per flag", f.Name)
			}
			existing, _ := target[p.Name].([]interface{})
			target[p.Name] = append(existing, list...)
			return nil
		}
		item, err := scalar(f.Name, p.Items, value, stdin)
		if err != nil {
			return err
		}
		existing, _ := target[p.Name].([]interface{})
		target[p.Name] = append(existing, item)
	default:
		v, err := scalar(f.Name, p.Type, value, stdin)
		if err != nil {
			return err
		}
		target[p.Name] = v
	}
	return nil
}

// scalar parses a value of one type: numbers stay exact (json.Number), objects are JSON.
func scalar(flag, typ, value string, stdin io.Reader) (interface{}, error) {
	switch typ {
	case "integer":
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return nil, usagef("--%s is a whole number (got %q)", flag, value)
		}
		return json.Number(value), nil
	case "number":
		if _, err := strconv.ParseFloat(value, 64); err != nil {
			return nil, usagef("--%s is a number (got %q)", flag, value)
		}
		return json.Number(value), nil
	case "boolean":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return nil, usagef("--%s is true or false (got %q)", flag, value)
		}
		return b, nil
	case "object":
		v, err := jsonArg(flag, value, stdin)
		if err != nil {
			return nil, err
		}
		if _, ok := v.(map[string]interface{}); !ok {
			return nil, usagef("--%s takes a JSON object, e.g. '{\"from\": \"A\"}'", flag)
		}
		return v, nil
	case "string":
		return value, nil
	}
	// No declared type: JSON when it looks like JSON, else the text.
	if strings.HasPrefix(value, "{") || strings.HasPrefix(value, "[") {
		return jsonArg(flag, value, stdin)
	}
	return value, nil
}

func jsonArg(flag, value string, stdin io.Reader) (interface{}, error) {
	raw, err := readArg(value, stdin)
	if err != nil {
		return nil, err
	}
	var v interface{}
	if err := decodeJSON(raw, &v); err != nil {
		return nil, usagef("--%s is not JSON: %v", flag, err)
	}
	return v, nil
}

// readArg is a literal, @file, or - (or @-) for stdin.
func readArg(value string, stdin io.Reader) ([]byte, error) {
	switch {
	case value == "-" || value == "@-":
		if stdin == nil {
			return nil, usagef("nothing to read on stdin")
		}
		return ioutil.ReadAll(stdin)
	case strings.HasPrefix(value, "@"):
		data, err := ioutil.ReadFile(value[1:])
		if err != nil {
			return nil, usagef("%v", err)
		}
		return data, nil
	}
	return []byte(value), nil
}

func decodeJSON(raw []byte, v interface{}) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return fmt.Errorf("more than one JSON value")
	}
	return nil
}

// encodeQuery writes a value the way Phoenix reads nested params: list[]=a&list[]=b,
// obj[key]=v, and obj[key][inner]=v further down.
func encodeQuery(q url.Values, key string, v interface{}) {
	switch x := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			encodeQuery(q, key+"["+k+"]", x[k])
		}
	case []interface{}:
		for _, item := range x {
			encodeQuery(q, key+"[]", item)
		}
	case nil:
		q.Add(key, "")
	case bool:
		q.Add(key, strconv.FormatBool(x))
	case json.Number:
		q.Add(key, x.String())
	case float64:
		q.Add(key, strconv.FormatFloat(x, 'f', -1, 64))
	default:
		q.Add(key, fmt.Sprint(x))
	}
}

// fill puts each argument, escaped, into the next {param} of the template.
func fill(template string, args []string) string {
	var b strings.Builder
	rest := template
	for _, a := range args {
		open := strings.IndexByte(rest, '{')
		end := strings.IndexByte(rest, '}')
		if open < 0 || end < open {
			break
		}
		b.WriteString(rest[:open])
		b.WriteString(url.PathEscape(a))
		rest = rest[end+1:]
	}
	b.WriteString(rest)
	return b.String()
}

func argList(params []string) string {
	if len(params) == 0 {
		return "no arguments"
	}
	parts := make([]string, len(params))
	for i, p := range params {
		parts[i] = "<" + strings.TrimSuffix(p, "_id") + ">"
	}
	return strings.Join(parts, " ")
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
