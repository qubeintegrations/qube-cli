package ops

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

// The rules an object's fields follow beyond their own types, read from its schema: which
// fields exclude each other (an exclusive xsd:choice), and which lists combine (a repeating
// one). Help shows each rule beside the fields it binds.

// choice is an exclusive choice: alternatives of which at most one (or, when required,
// exactly one) is given. An alternative of several fields is a set whose members go
// together and exclude the other alternatives.
type choice struct {
	alts     [][]string
	required bool
	atLeast  bool // the fields need not exclude each other, but one of them is required
}

// combo is a repeating choice: lists that combine, any number of entries in each.
type combo struct {
	members  []string
	required bool // at least one entry across them
}

type rules struct {
	choices  []choice
	combos   []combo
	excludes map[string][]string // conflicts that form no clean choice: field -> what it excludes
}

// rulesOf reads an object schema's rules. The spec writes them as JSON Schema:
//
//   - `not: {anyOf: [{required: [a, b]}, ...]}`: a and b exclude each other. The pairs of a
//     choice are every member of one alternative with every member of another, so an
//     alternative is the fields that exclude exactly the same others.
//   - a `not.anyOf` entry `{properties: {a: false, b: false}}`: not none of them, so a
//     choice of those fields is required.
//   - a `not.anyOf` entry `{properties: {a: {maxItems: 0}, b: {maxItems: 0}}}`: not every
//     list empty, so these lists are a required repeating choice.
//   - `oneOf: [{properties: {a: ...}}, {properties: {b: ...}}]`: exactly one alternative.
//
// An optional repeating choice (an invoice's item and group lines) has no rule to write, so
// the schema leaves it unmarked; combosFromNotes reads it from the field descriptions.
func rulesOf(schema map[string]interface{}) rules {
	var r rules
	var pairs [][2]string
	var absent, empty [][]string
	if not, ok := schema["not"].(map[string]interface{}); ok {
		patterns, _ := not["anyOf"].([]interface{})
		if len(patterns) == 0 {
			patterns = []interface{}{not}
		}
		for _, p := range patterns {
			pat, _ := p.(map[string]interface{})
			if req := strs(pat["required"]); len(req) == 2 {
				pairs = append(pairs, [2]string{req[0], req[1]})
				continue
			}
			props, _ := pat["properties"].(map[string]interface{})
			if len(props) == 0 {
				continue
			}
			names := sortedKeys(props)
			switch {
			case allValues(props, func(v interface{}) bool { return v == false }):
				absent = append(absent, names)
			case allValues(props, func(v interface{}) bool {
				m, _ := v.(map[string]interface{})
				return len(m) == 1 && m["maxItems"] != nil && numberIs(m["maxItems"], 0)
			}):
				empty = append(empty, names)
			}
		}
	}
	r.choices, r.excludes = exclusiveChoices(pairs, absent)
	if branches, ok := schema["oneOf"].([]interface{}); ok {
		var alts [][]string
		for _, b := range branches {
			branch, _ := b.(map[string]interface{})
			props, _ := branch["properties"].(map[string]interface{})
			if len(props) > 0 {
				alts = append(alts, sortedKeys(props))
			}
		}
		if len(alts) > 1 {
			r.choices = append(r.choices, choice{alts: alts, required: true})
		}
	}
	props, _ := schema["properties"].(map[string]interface{})
	r.combos = combosFromNotes(props)
	for _, names := range empty {
		found := false
		for i := range r.combos {
			if sameSet(r.combos[i].members, names) {
				r.combos[i].required, found = true, true
			}
		}
		if !found {
			r.combos = append(r.combos, combo{members: names, required: true})
		}
	}
	return r
}

// exclusiveChoices groups conflicting pairs into choices: the fields linked by conflicts are
// one choice, and its alternatives are the fields with the same conflicts. A group whose
// conflicts are not "every alternative excludes every other" (the spec writes none) is
// kept as each field's list of exclusions instead.
func exclusiveChoices(pairs [][2]string, absent [][]string) ([]choice, map[string][]string) {
	adj := map[string]map[string]bool{}
	var order []string // fields in the order the rule names them: the alternatives' order
	for _, p := range pairs {
		for k, a := range p {
			if adj[a] == nil {
				adj[a] = map[string]bool{}
				order = append(order, a)
			}
			adj[a][p[1-k]] = true
		}
	}
	var out []choice
	var excludes map[string][]string
	seen := map[string]bool{}
	usedAbsent := make([]bool, len(absent))
	for _, start := range order {
		if seen[start] {
			continue
		}
		// the group: every field linked to start
		group := map[string]bool{start: true}
		queue := []string{start}
		for len(queue) > 0 {
			f := queue[0]
			queue = queue[1:]
			for g := range adj[f] {
				if !group[g] {
					group[g] = true
					queue = append(queue, g)
				}
			}
		}
		var members []string
		for _, f := range order {
			if group[f] {
				members = append(members, f)
				seen[f] = true
			}
		}
		// alternatives: fields with the same conflicts
		var alts [][]string
		altOf := map[string]int{}
		for _, f := range members {
			key := strings.Join(sortedKeys(boolMap(adj[f])), ",")
			if i, ok := altOf[key]; ok {
				alts[i] = append(alts[i], f)
			} else {
				altOf[key] = len(alts)
				alts = append(alts, []string{f})
			}
		}
		for _, alt := range alts {
			sort.Strings(alt) // a set's members, as the spec's note lists them
		}
		if !completeMultipartite(alts, adj) {
			if excludes == nil {
				excludes = map[string][]string{}
			}
			for _, f := range members {
				excludes[f] = sortedKeys(boolMap(adj[f]))
			}
			continue
		}
		c := choice{alts: alts}
		for i, names := range absent {
			if sameSet(names, members) {
				c.required, usedAbsent[i] = true, true
			}
		}
		out = append(out, c)
	}
	for i, names := range absent {
		if !usedAbsent[i] {
			if len(names) == 1 {
				continue // a required field, said by `required`
			}
			out = append(out, choice{alts: [][]string{names}, atLeast: true})
		}
	}
	return out, excludes
}

func completeMultipartite(alts [][]string, adj map[string]map[string]bool) bool {
	for i := range alts {
		for j := range alts {
			if i == j {
				continue
			}
			for _, a := range alts[i] {
				for _, b := range alts[j] {
					if !adj[a][b] {
						return false
					}
				}
			}
		}
	}
	return true
}

// The generator writes a repeating choice's note into each of its lists' descriptions in
// one of two sentences:
//
//	`a` and `b` combine: any number of each.
//	At least one entry is required across `a` and `b`; they combine, any number of each.
//
// The schema carries the rule only for a required one (its `maxItems: 0` entry above), so
// for an optional one this sentence is all there is. It is generated, not written by hand,
// so its wording is stable; the spec check fails should it change (every list whose
// description says "any number of each" must show the hint).
var (
	combineNote         = regexp.MustCompile("(?:^|\\n\\n)((?:`[a-z0-9_]+`(?:, | and )?)+) combine: any number of each\\.")
	requiredCombineNote = regexp.MustCompile("At least one entry is required across ((?:`[a-z0-9_]+`(?:, | and )?)+); they combine, any number of each\\.")
	backticked          = regexp.MustCompile("`([a-z0-9_]+)`")
)

func combosFromNotes(props map[string]interface{}) []combo {
	var out []combo
	for _, name := range sortedKeys(props) {
		p, _ := props[name].(map[string]interface{})
		desc, _ := p["description"].(string)
		var list string
		required := false
		if m := requiredCombineNote.FindStringSubmatch(desc); m != nil {
			list, required = m[1], true
		} else if m := combineNote.FindStringSubmatch(desc); m != nil {
			list = m[1]
		} else {
			continue
		}
		var members []string
		for _, m := range backticked.FindAllStringSubmatch(list, -1) {
			if _, ok := props[m[1]]; ok {
				members = append(members, m[1])
			}
		}
		if len(members) < 2 {
			continue
		}
		dup := false
		for _, c := range out {
			if sameSet(c.members, members) {
				dup = true
			}
		}
		if !dup {
			out = append(out, combo{members: members, required: required})
		}
	}
	return out
}

// OpenAPI query parameters are independent of each other: there is no schema in which to
// say that two of them exclude each other. The generator says it in each member's
// description instead, in one sentence:
//
//	Choose at most one of: `a`, or `b`, or the set (`c`, `d`). Members of a set combine ...
//	Choose exactly one of: `a`, or `b`.
//
// For query parameters, that sentence is what the rule is read from. Like the repeating
// choice's note it is generated, so stable, and the spec check holds help to it.
var choiceNote = regexp.MustCompile("Choose (at most|exactly) one of: ((?:(?:`[a-z0-9_]+`|the set \\((?:`[a-z0-9_]+`(?:, )?)+\\))(?:, or )?)+)\\.")

func queryRules(params []Param) rules {
	known := map[string]bool{}
	for _, p := range params {
		known[p.Name] = true
	}
	var r rules
	seen := map[string]bool{}
	for _, p := range params {
		for _, m := range choiceNote.FindAllStringSubmatch(p.Description, -1) {
			var alts [][]string
			ok := true
			for _, alt := range strings.Split(m[2], ", or ") {
				var names []string
				for _, n := range backticked.FindAllStringSubmatch(alt, -1) {
					names = append(names, n[1])
					ok = ok && known[n[1]]
				}
				alts = append(alts, names)
			}
			key := m[1] + ":" + m[2]
			if !ok || len(alts) < 2 || seen[key] {
				continue
			}
			seen[key] = true
			r.choices = append(r.choices, choice{alts: alts, required: m[1] == "exactly"})
		}
	}
	return r
}

// hints are the rules a field takes part in, each a phrase: "at most one of: a | b".
// name writes a field's name (as a flag, or as a JSON key).
func (r rules) hints(field string, name func(string) string) []string {
	var out []string
	for _, c := range r.choices {
		if c.has(field) {
			out = append(out, c.hint(field, name))
		}
	}
	for _, c := range r.combos {
		if !contains(c.members, field) {
			continue
		}
		var others []string
		for _, m := range c.members {
			if m != field {
				others = append(others, name(m))
			}
		}
		s := "can be combined with " + joinAnd(others)
		if c.required {
			s += "; at least one entry is required across them"
		}
		out = append(out, s)
	}
	if ex := r.excludes[field]; len(ex) > 0 {
		var names []string
		for _, e := range ex {
			names = append(names, name(e))
		}
		out = append(out, "not with "+joinOr(names))
	}
	return out
}

// all is every rule, each a phrase, for showing an object's rules in one place.
func (r rules) all(name func(string) string) []string {
	var out []string
	for _, c := range r.choices {
		out = append(out, c.phrase(name))
	}
	for _, c := range r.combos {
		var names []string
		for _, m := range c.members {
			names = append(names, name(m))
		}
		s := joinAnd(names) + " combine"
		if c.required {
			s += ", at least one entry across them"
		}
		out = append(out, s)
	}
	for _, f := range sortedKeys(stringsMap(r.excludes)) {
		var names []string
		for _, e := range r.excludes[f] {
			names = append(names, name(e))
		}
		out = append(out, name(f)+" not with "+joinOr(names))
	}
	return out
}

func (r rules) empty() bool {
	return len(r.choices) == 0 && len(r.combos) == 0 && len(r.excludes) == 0
}

func (c choice) has(field string) bool {
	for _, alt := range c.alts {
		if contains(alt, field) {
			return true
		}
	}
	return false
}

// longHint is how long a choice's phrase may be before a member of one of its sets is told
// only what it excludes.
const longHint = 120

// hint is the choice as one of its fields sees it: the whole phrase, or when that is long
// and the field is one of a set (a query's eight filters), the fields it excludes.
func (c choice) hint(field string, name func(string) string) string {
	full := c.phrase(name)
	if c.atLeast || len(full) <= longHint {
		return full
	}
	var others []string
	inSet := false
	for _, alt := range c.alts {
		if contains(alt, field) {
			inSet = len(alt) > 1
			continue
		}
		for _, f := range alt {
			others = append(others, name(f))
		}
	}
	if !inSet {
		return full
	}
	s := "not with " + joinOr(others)
	if c.required {
		s += ", and one alternative is required"
	}
	return s
}

// phrase: "at most one of: a | b | (c, d)", a parenthesized set being fields that go together.
func (c choice) phrase(name func(string) string) string {
	lead := "at most one of: "
	switch {
	case c.atLeast:
		var names []string
		for _, f := range c.alts[0] {
			names = append(names, name(f))
		}
		return "at least one of: " + strings.Join(names, ", ")
	case c.required:
		lead = "exactly one of: "
	}
	var parts []string
	for _, alt := range c.alts {
		var names []string
		for _, f := range alt {
			names = append(names, name(f))
		}
		if len(names) == 1 {
			parts = append(parts, names[0])
		} else {
			parts = append(parts, "("+strings.Join(names, ", ")+")")
		}
	}
	return lead + strings.Join(parts, " | ")
}

// ---------------------------------------------------------------- helpers

func strs(v interface{}) []string {
	list, _ := v.([]interface{})
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func boolMap(m map[string]bool) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func stringsMap(m map[string][]string) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func allValues(m map[string]interface{}, pred func(interface{}) bool) bool {
	for _, v := range m {
		if !pred(v) {
			return false
		}
	}
	return len(m) > 0
}

func numberIs(v interface{}, n float64) bool {
	switch x := v.(type) {
	case float64:
		return x == n
	case int:
		return float64(x) == n
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == n
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func joinAnd(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func joinOr(names []string) string {
	if len(names) <= 1 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}
