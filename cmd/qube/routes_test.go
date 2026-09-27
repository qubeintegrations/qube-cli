package main

import (
	"encoding/json"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The v2 operations that are not QuickBooks operations -- connections, queued requests, the
// simulator, workflows and their templates and runs -- are hand-written commands. (QuickBooks
// operations, the ones answered through the Web Connector, are `qube qb`, built from the
// host's spec at run time.) testdata/v2_routes.txt lists them, one "METHOD /template" a line.

var updateRoutes = flag.Bool("update", false, "rewrite testdata/v2_routes.txt from the qube checkout's spec")

const routesGolden = "testdata/v2_routes.txt"

// Routes no command calls, and why that is fine.
var uncalledRoutes = map[string]string{
	"GET /workflows/schema.json": "the unauthenticated copy of GET /workflows/schema",
}

// Every route in the list has a command, and every command's route is in the list.
func TestV2RoutesHaveCommands(t *testing.T) {
	want := readGolden(t)
	called := calledRoutes(t)
	for _, r := range want {
		if _, ok := called[r]; !ok && uncalledRoutes[r] == "" {
			t.Errorf("no command calls %s", r)
		}
	}
	golden := map[string]bool{}
	for _, r := range want {
		golden[r] = true
	}
	for r, where := range called {
		if !golden[r] {
			t.Errorf("%s calls %s, which is not a v2 route in %s", where, r, routesGolden)
		}
	}
}

// The list matches the spec, when a qube checkout sits beside this one (or QUBE_REPO names
// one). CI has no checkout and relies on the list; `-update` rewrites it.
func TestV2RoutesMatchSpec(t *testing.T) {
	repo := os.Getenv("QUBE_REPO")
	if repo == "" {
		repo = filepath.Join("..", "..", "..", "qube")
	}
	var spec []string
	for _, f := range []string{"connections.json", "workflows.json"} {
		data, err := os.ReadFile(filepath.Join(repo, "priv", "openapi", f))
		if err != nil {
			t.Skipf("no qube checkout at %s (set QUBE_REPO): %v", repo, err)
		}
		routes, err := specRoutes(data)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		spec = append(spec, routes...)
	}
	sort.Strings(spec)
	if *updateRoutes {
		if err := os.WriteFile(routesGolden, []byte(strings.Join(spec, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if got := readGolden(t); strings.Join(got, "\n") != strings.Join(spec, "\n") {
		t.Fatalf("%s is out of date with %s: run `go test ./cmd/qube -run TestV2RoutesMatchSpec -update`\nlist: %v\nspec: %v", routesGolden, repo, got, spec)
	}
}

// specRoutes reads an OpenAPI document's operations, leaving out the QuickBooks ones (they
// carry an `answered` callback: the webhook that says QuickBooks answered).
func specRoutes(data []byte) ([]string, error) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	var out []string
	for path, item := range doc.Paths {
		for method, raw := range item {
			switch method {
			case "get", "post", "put", "patch", "delete":
			default:
				continue
			}
			var op struct {
				Callbacks map[string]json.RawMessage `json:"callbacks"`
			}
			if err := json.Unmarshal(raw, &op); err != nil {
				return nil, err
			}
			if len(op.Callbacks) == 0 {
				out = append(out, strings.ToUpper(method)+" "+path)
			}
		}
	}
	return out, nil
}

func readGolden(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(routesGolden)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// calledRoutes finds every `x.Do("METHOD", v2path("/template", ...), ...)` (and x.Raw) in
// this package's source: route -> where it is called.
func calledRoutes(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Do" && sel.Sel.Name != "Raw") {
				return true
			}
			inner, ok := call.Args[1].(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := inner.Fun.(*ast.Ident); !ok || id.Name != "v2path" || len(inner.Args) == 0 {
				return true
			}
			method, ok1 := stringLit(call.Args[0])
			template, ok2 := stringLit(inner.Args[0])
			if !ok1 || !ok2 {
				t.Errorf("%s: v2path calls need a literal method and template", fset.Position(call.Pos()))
				return true
			}
			out[method+" "+template] = fset.Position(call.Pos()).String()
			return true
		})
	}
	return out
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}
