package ops

import "strings"

// Complete is what shell completion offers after `qube qb <resource> <verb>`, given the word
// before the cursor: the values of that word's flag when it takes one (its allowed values, if
// the spec lists them; nothing to offer otherwise), else every flag the operation takes,
// qube's own (--data, --wait, --help, and --example when there is one) included.
func (op *Op) Complete(prev string) []string {
	flags := op.Flags()
	if strings.HasPrefix(prev, "--") && !strings.Contains(prev, "=") {
		name := flagName(prev[2:])
		if name == "data" {
			return nil
		}
		for _, f := range flags {
			if f.Name == name && f.Param.Type != "boolean" {
				return f.Param.Enum
			}
		}
	}
	out := make([]string, 0, len(flags)+4)
	for _, f := range flags {
		out = append(out, "--"+f.Name)
	}
	out = append(out, "--data", "--wait", "--help")
	if len(op.Example) > 0 {
		out = append(out, "--example")
	}
	return out
}
