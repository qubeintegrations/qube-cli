package main

import (
	"encoding/json"
	"flag"
	"fmt"
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
