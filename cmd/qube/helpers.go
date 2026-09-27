package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// parseAnywhere lets flags follow positional arguments (`qube api POST /path --data ...`),
// which Go's flag package does not: it stops at the first non-flag. Flags are moved in front,
// each value-taking flag together with its value; `--` ends flag parsing.
func parseAnywhere(fs *flag.FlagSet, args []string) {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			continue
		}
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
			continue
		}
		if i+1 < len(args) {
			flags = append(flags, args[i+1])
			i++
		}
	}
	_ = fs.Parse(append(flags, positional...))
}

// v2path turns a route template from the v2 spec into a request path, escaping each
// argument into the next {param}: v2path("/connections/{connection_id}/simulator", id) is
// "/api/v2/connections/<id>/simulator". Every hand-written command names its route this
// way, as `cl.Do("METHOD", v2path("<template>", ...), ...)` with literal method and
// template, so TestV2RoutesHaveCommands can check the commands against the spec.
func v2path(template string, args ...string) string {
	var b strings.Builder
	b.WriteString("/api/v2")
	rest := template
	for _, a := range args {
		open := strings.IndexByte(rest, '{')
		end := strings.IndexByte(rest, '}')
		if open < 0 || end < open {
			panic("v2path: more arguments than {params} in " + template)
		}
		b.WriteString(rest[:open])
		b.WriteString(url.PathEscape(a))
		rest = rest[end+1:]
	}
	if strings.IndexByte(rest, '{') >= 0 {
		panic("v2path: a {param} of " + template + " has no argument")
	}
	b.WriteString(rest)
	return b.String()
}

// confirm guards a request that removes something, or changes a production app. With yes,
// it returns at once. Under --json, or when stdin isn't a terminal, there is no one to ask,
// so it fails as a usage error naming --yes. Otherwise it asks on stderr and reads one line
// from stdin, going ahead only on "y" or "yes" (case-insensitive).
func confirm(question string, yes bool) {
	if yes {
		return
	}
	if ui.JSON || !interactive() {
		ui.Usage("%s Pass --yes to confirm (qube asks only in a terminal, and not under --json).", question)
	}
	fmt.Fprint(os.Stderr, question+" [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
	default:
		ui.Fail("not confirmed")
	}
}

// interactive says whether stdin is a terminal a person could answer from. /dev/null is a
// character device too, so it is ruled out by name.
func interactive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(fi, null) {
		return false
	}
	return true
}

// readJSONArg parses a JSON literal or `@file`.
func readJSONArg(s string) interface{} {
	var raw []byte
	if strings.HasPrefix(s, "@") {
		var err error
		raw, err = os.ReadFile(s[1:])
		if err != nil {
			ui.Fail("%v", err)
		}
	} else {
		raw = []byte(s)
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		ui.Usage("not JSON: %v", err)
	}
	return v
}

func str(v interface{}) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func strs(v interface{}) []string {
	list, _ := v.([]interface{})
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, str(x))
	}
	return out
}
