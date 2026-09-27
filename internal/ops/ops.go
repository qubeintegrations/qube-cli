// Package ops is the QuickBooks half of the v2 API as `qube qb` sees it: every operation in
// the host's OpenAPI document that QuickBooks answers (it carries an `answered` callback, the
// webhook sent when the answer comes back). The list is read from the host at run time and
// cached, so the CLI offers exactly the operations, flags and body fields the host has, with
// nothing to regenerate when the server adds one.
package ops

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Format is the shape of a cached Index; a cache written with another one is fetched again.
const Format = 1

// Param is one query parameter.
type Param struct {
	Name        string          `json:"name"`
	Required    bool            `json:"required,omitempty"`
	Type        string          `json:"type,omitempty"`  // string, integer, number, boolean, object, array
	Items       string          `json:"items,omitempty"` // an array's item type
	Enum        []string        `json:"enum,omitempty"`  // a string's values, or an array's items'
	Description string          `json:"description,omitempty"`
	Schema      json.RawMessage `json:"schema,omitempty"`
}

// Op is one operation: `qube qb <Resource> <Verb> <path params...>`.
type Op struct {
	Resource     string          `json:"resource"`
	Verb         string          `json:"verb"`
	Method       string          `json:"method"`
	Path         string          `json:"path"` // relative to /api/v2
	PathParams   []string        `json:"path_params"`
	OperationID  string          `json:"operation_id"`
	Summary      string          `json:"summary,omitempty"`
	Description  string          `json:"description,omitempty"`
	Query        []Param         `json:"query,omitempty"`
	Body         json.RawMessage `json:"body,omitempty"` // the request body's JSON Schema, refs resolved
	BodyRequired bool            `json:"body_required,omitempty"`
}

// Index is every QuickBooks operation one host offers.
type Index struct {
	Format    int       `json:"format"`
	Host      string    `json:"host"`
	FetchedAt time.Time `json:"fetched_at"`
	Version   string    `json:"version"`
	Ops       []Op      `json:"ops"`
}

var methods = map[string]bool{"get": true, "post": true, "put": true, "patch": true, "delete": true}

// A template the CLI will fill and send: static segments and {params}, nothing else.
var safePath = regexp.MustCompile(`^(/([A-Za-z0-9_.-]+|\{[A-Za-z0-9_]+\}))+$`)

// Parse reads the QuickBooks operations out of a v2 OpenAPI document.
func Parse(spec []byte) (*Index, error) {
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas    map[string]json.RawMessage `json:"schemas"`
			Parameters map[string]json.RawMessage `json:"parameters"`
		} `json:"components"`
	}
	if err := json.Unmarshal(spec, &doc); err != nil {
		return nil, fmt.Errorf("the OpenAPI document is not JSON: %v", err)
	}
	if len(doc.Paths) == 0 {
		return nil, fmt.Errorf("the OpenAPI document has no paths")
	}
	r := &resolver{schemas: doc.Components.Schemas, parameters: doc.Components.Parameters}
	ix := &Index{Format: Format, Version: doc.Info.Version}
	for path, item := range doc.Paths {
		if !safePath.MatchString(path) || strings.Contains(path+"/", "/./") || strings.Contains(path+"/", "/../") {
			continue
		}
		var shared []json.RawMessage
		if raw, ok := item["parameters"]; ok {
			_ = json.Unmarshal(raw, &shared)
		}
		for method, raw := range item {
			if !methods[method] {
				continue
			}
			op, ok, err := r.op(path, strings.ToUpper(method), raw, shared)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %v", strings.ToUpper(method), path, err)
			}
			if ok {
				ix.Ops = append(ix.Ops, op)
			}
		}
	}
	nameOps(ix.Ops)
	sort.Slice(ix.Ops, func(i, j int) bool {
		if ix.Ops[i].Resource != ix.Ops[j].Resource {
			return ix.Ops[i].Resource < ix.Ops[j].Resource
		}
		return verbRank(ix.Ops[i].Verb) < verbRank(ix.Ops[j].Verb) ||
			verbRank(ix.Ops[i].Verb) == verbRank(ix.Ops[j].Verb) && ix.Ops[i].Verb < ix.Ops[j].Verb
	})
	if len(ix.Ops) == 0 {
		return nil, fmt.Errorf("the OpenAPI document has no QuickBooks operations")
	}
	return ix, nil
}

func (r *resolver) op(path, method string, raw json.RawMessage, shared []json.RawMessage) (Op, bool, error) {
	var spec struct {
		OperationID string                     `json:"operationId"`
		Summary     string                     `json:"summary"`
		Description string                     `json:"description"`
		Parameters  []json.RawMessage          `json:"parameters"`
		Callbacks   map[string]json.RawMessage `json:"callbacks"`
		RequestBody *struct {
			Ref      string `json:"$ref"`
			Required bool   `json:"required"`
			Content  map[string]struct {
				Schema json.RawMessage `json:"schema"`
			} `json:"content"`
		} `json:"requestBody"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return Op{}, false, err
	}
	if _, answered := spec.Callbacks["answered"]; !answered {
		return Op{}, false, nil
	}
	op := Op{Method: method, Path: path, OperationID: spec.OperationID, Summary: spec.Summary, Description: spec.Description}
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") {
			op.PathParams = append(op.PathParams, strings.Trim(seg, "{}"))
		}
	}
	seen := map[string]bool{}
	for _, list := range [][]json.RawMessage{spec.Parameters, shared} {
		for _, p := range list {
			param, in, err := r.param(p)
			if err != nil {
				return Op{}, false, err
			}
			if in != "query" || seen[param.Name] {
				continue
			}
			seen[param.Name] = true
			op.Query = append(op.Query, param)
		}
	}
	if rb := spec.RequestBody; rb != nil {
		if c, ok := rb.Content["application/json"]; ok && len(c.Schema) > 0 {
			var schema interface{}
			if err := json.Unmarshal(c.Schema, &schema); err != nil {
				return Op{}, false, err
			}
			op.Body, _ = json.Marshal(r.deref(schema, 0, map[string]bool{}))
			op.BodyRequired = rb.Required
		}
	}
	return op, true, nil
}

// nameOps gives each operation its resource (the static path segments after
// /connections/{connection_id}) and verb (the operationId's leading word: list, create,
// update, delete, execute, record...). Should two operations of a resource share a verb,
// both fall back to the whole operationId.
func nameOps(ops []Op) {
	count := map[string]int{}
	for i := range ops {
		var segs []string
		for _, seg := range strings.Split(strings.TrimPrefix(ops[i].Path, "/"), "/") {
			if seg != "" && !strings.HasPrefix(seg, "{") {
				segs = append(segs, seg)
			}
		}
		if len(segs) > 1 && segs[0] == "connections" {
			segs = segs[1:]
		}
		ops[i].Resource = strings.Join(segs, "/")
		ops[i].Verb = leadingWord(ops[i].OperationID)
		if ops[i].Verb == "" {
			ops[i].Verb = strings.ToLower(ops[i].Method)
		}
		count[ops[i].Resource+" "+ops[i].Verb]++
	}
	for i := range ops {
		if count[ops[i].Resource+" "+ops[i].Verb] > 1 && ops[i].OperationID != "" {
			ops[i].Verb = kebab(ops[i].OperationID)
		}
	}
}

func leadingWord(id string) string {
	for i, c := range id {
		if c < 'a' || c > 'z' {
			return id[:i]
		}
	}
	return id
}

// kebab: listCustomers -> list-customers, executeQBCommandExec -> execute-qb-command-exec.
func kebab(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i, c := range rs {
		upper := c >= 'A' && c <= 'Z'
		if upper && i > 0 {
			prevLower := rs[i-1] >= 'a' && rs[i-1] <= 'z'
			nextLower := i+1 < len(rs) && rs[i+1] >= 'a' && rs[i+1] <= 'z'
			if prevLower || nextLower && rs[i-1] >= 'A' && rs[i-1] <= 'Z' {
				b.WriteByte('-')
			}
		}
		if upper {
			c += 'a' - 'A'
		}
		b.WriteRune(c)
	}
	return b.String()
}

func verbRank(v string) int {
	switch v {
	case "list":
		return 0
	case "create":
		return 1
	case "update":
		return 2
	case "delete":
		return 3
	}
	return 4
}

// Resources lists each resource once, sorted.
func (ix *Index) Resources() []string {
	var out []string
	for i := range ix.Ops {
		if len(out) == 0 || out[len(out)-1] != ix.Ops[i].Resource {
			out = append(out, ix.Ops[i].Resource)
		}
	}
	return out
}

// Of returns a resource's operations.
func (ix *Index) Of(resource string) []*Op {
	var out []*Op
	for i := range ix.Ops {
		if ix.Ops[i].Resource == resource {
			out = append(out, &ix.Ops[i])
		}
	}
	return out
}

// Find returns the operation, or nil.
func (ix *Index) Find(resource, verb string) *Op {
	for _, op := range ix.Of(resource) {
		if op.Verb == verb {
			return op
		}
	}
	return nil
}

// ---------------------------------------------------------------- refs

type resolver struct {
	schemas    map[string]json.RawMessage
	parameters map[string]json.RawMessage
}

func (r *resolver) param(raw json.RawMessage) (Param, string, error) {
	var p struct {
		Ref         string                 `json:"$ref"`
		Name        string                 `json:"name"`
		In          string                 `json:"in"`
		Required    bool                   `json:"required"`
		Description string                 `json:"description"`
		Schema      map[string]interface{} `json:"schema"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return Param{}, "", err
	}
	if p.Ref != "" {
		target, ok := r.parameters[strings.TrimPrefix(p.Ref, "#/components/parameters/")]
		if !ok || !strings.HasPrefix(p.Ref, "#/components/parameters/") {
			return Param{}, "", fmt.Errorf("unresolvable parameter %s", p.Ref)
		}
		return r.param(target)
	}
	schema, _ := r.deref(p.Schema, 0, map[string]bool{}).(map[string]interface{})
	param := Param{Name: p.Name, Required: p.Required, Description: p.Description, Type: typeOf(schema)}
	switch param.Type {
	case "array":
		items, _ := schema["items"].(map[string]interface{})
		param.Items = typeOf(items)
		param.Enum = enumOf(items)
		if param.Items == "object" {
			param.Schema, _ = json.Marshal(schema)
		}
	case "object":
		param.Schema, _ = json.Marshal(schema)
	default:
		param.Enum = enumOf(schema)
	}
	return param, p.In, nil
}

// deref inlines every #/components/schemas ref; one that refers to itself stays a $ref.
func (r *resolver) deref(v interface{}, depth int, active map[string]bool) interface{} {
	if depth > 40 {
		return v
	}
	switch x := v.(type) {
	case map[string]interface{}:
		if ref, ok := x["$ref"].(string); ok && strings.HasPrefix(ref, "#/components/schemas/") {
			name := strings.TrimPrefix(ref, "#/components/schemas/")
			raw, ok := r.schemas[name]
			if !ok || active[name] {
				return x
			}
			var target interface{}
			if json.Unmarshal(raw, &target) != nil {
				return x
			}
			active[name] = true
			out := r.deref(target, depth+1, active)
			delete(active, name)
			return out
		}
		out := make(map[string]interface{}, len(x))
		for k, val := range x {
			out[k] = r.deref(val, depth+1, active)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(x))
		for i, val := range x {
			out[i] = r.deref(val, depth+1, active)
		}
		return out
	}
	return v
}

// typeOf reads `type`, which OpenAPI 3.1 also allows as a list such as ["string", "null"].
func typeOf(schema map[string]interface{}) string {
	switch t := schema["type"].(type) {
	case string:
		return t
	case []interface{}:
		for _, x := range t {
			if s, ok := x.(string); ok && s != "null" {
				return s
			}
		}
	}
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	return ""
}

func enumOf(schema map[string]interface{}) []string {
	list, _ := schema["enum"].([]interface{})
	var out []string
	for _, v := range list {
		if v != nil {
			out = append(out, fmt.Sprint(v))
		}
	}
	return out
}

// ---------------------------------------------------------------- cache

// Source loads a host's Index, from the cache while it is fresh, else from the host.
type Source struct {
	Host     string
	CacheDir string        // "" keeps nothing
	MaxAge   time.Duration // a cache older than this is fetched again
	Fetch    func() ([]byte, error)
	Now      func() time.Time
}

// Load returns the host's operations. With refresh it fetches them even when the cache is
// fresh. When fetching fails but a cache exists, however old, Load returns the cache along
// with the fetch error as a warning.
func (s *Source) Load(refresh bool) (ix *Index, fetched bool, warning error, err error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	cached, _ := s.read()
	if cached != nil && !refresh && now().Sub(cached.FetchedAt) < s.MaxAge {
		return cached, false, nil, nil
	}
	spec, ferr := s.Fetch()
	if ferr == nil {
		ix, ferr = Parse(spec)
	}
	if ferr != nil {
		if cached != nil {
			return cached, false, ferr, nil
		}
		return nil, false, nil, ferr
	}
	ix.Host, ix.FetchedAt = s.Host, now().UTC()
	if werr := s.write(ix); werr != nil {
		warning = fmt.Errorf("could not cache the operation list: %v", werr)
	}
	return ix, true, warning, nil
}

// CachePath is where a host's operations are kept.
func (s *Source) CachePath() string {
	if s.CacheDir == "" {
		return ""
	}
	return filepath.Join(s.CacheDir, "openapi-v2-"+slug(s.Host)+".json")
}

// Cached returns the cached operations, however old, or nil; it never fetches. Shell
// completion uses it so a TAB never waits on the network.
func (s *Source) Cached() *Index {
	ix, _ := s.read()
	return ix
}

func (s *Source) read() (*Index, error) {
	p := s.CachePath()
	if p == "" {
		return nil, nil
	}
	data, err := ioutil.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var ix Index
	if err := json.Unmarshal(data, &ix); err != nil {
		return nil, err
	}
	if ix.Format != Format || ix.Host != s.Host || len(ix.Ops) == 0 {
		return nil, nil
	}
	return &ix, nil
}

// write replaces the cache file atomically: a second qube running at once reads either the
// old list or the new one, never half of one.
func (s *Source) write(ix *Index) error {
	p := s.CachePath()
	if p == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	tmp, err := ioutil.TempFile(filepath.Dir(p), ".openapi-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

func slug(host string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	var b strings.Builder
	for _, c := range h {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
