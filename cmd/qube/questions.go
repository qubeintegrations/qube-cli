package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// A workflow that waits at a question carries a prompt an app can show without knowing the
// question: a `message`, `details` (label and value rows) and `choices`, each with an `id`, a
// `label`, an optional `description` and `detail`, and the `fields` to fill in before picking it.
// Everything is answered the same way, `workflows decide --choice ID [--data JSON]`, and a
// `record` field is filled from `workflows search`, which asks the run's `links.search`.

// mapOf is v as a JSON object, or nil.
func mapOf(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

// listOf is the objects in v, a JSON array, skipping anything else in it.
func listOf(v interface{}) []map[string]interface{} {
	items, _ := v.([]interface{})
	var out []map[string]interface{}
	for _, item := range items {
		if m := mapOf(item); m != nil {
			out = append(out, m)
		}
	}
	return out
}

// promptChoices are the choices of a waiting run's prompt: none for a prompt in the older
// shape, which has `options` only and is shown as the JSON it is.
func promptChoices(run map[string]interface{}) []map[string]interface{} {
	return listOf(mapOf(run["prompt"])["choices"])
}

// printable makes text from a server safe to print on a terminal: names come from QuickBooks,
// so escape sequences and other control characters are dropped, and, unless keepLines, so are
// line breaks.
func printable(s string, keepLines bool) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			if keepLines {
				return r
			}
			return ' '
		case unicode.IsControl(r):
			return -1
		}
		return r
	}, s)
}

// fieldLine is one field of a choice: `name (type, required)`, and its label after a dash.
func fieldLine(f map[string]interface{}) string {
	need := "optional"
	if b, _ := f["required"].(bool); b {
		need = "required"
	}
	line := fmt.Sprintf("%s (%s, %s)", printable(str(f["name"]), false), printable(str(f["type"]), false), need)
	if label := printable(str(f["label"]), false); label != "" {
		line += " - " + label
	}
	return line
}

// recordFields are the names of the `record` fields among the choices' fields, in order, and
// the id of the first choice that has each.
func recordFields(choices []map[string]interface{}) (names []string, choiceOf map[string]string) {
	choiceOf = map[string]string{}
	for _, ch := range choices {
		for _, f := range listOf(ch["fields"]) {
			name := str(f["name"])
			if str(f["type"]) != "record" || name == "" {
				continue
			}
			if _, seen := choiceOf[name]; !seen {
				names = append(names, name)
				choiceOf[name] = str(ch["id"])
			}
		}
	}
	return names, choiceOf
}

// questionText is a waiting run's question as a person reads it: the message, the details as
// `label: value`, the choices, and how to answer them.
func questionText(connection string, run map[string]interface{}) string {
	prompt := mapOf(run["prompt"])
	choices := promptChoices(run)
	runID := str(run["id"])
	var b strings.Builder

	if msg := strings.TrimSpace(printable(str(prompt["message"]), true)); msg != "" {
		fmt.Fprintf(&b, "\n%s\n", msg)
	}
	if details := listOf(prompt["details"]); len(details) > 0 {
		b.WriteString("\n")
		for _, d := range details {
			fmt.Fprintf(&b, "  %s: %s\n", printable(str(d["label"]), false), printable(str(d["value"]), false))
		}
	}

	width := 0
	for _, ch := range choices {
		if n := utf8.RuneCountInString(printable(str(ch["id"]), false)); n > width {
			width = n
		}
	}
	b.WriteString("\nChoices:\n")
	for _, ch := range choices {
		id := printable(str(ch["id"]), false)
		line := fmt.Sprintf("  %-*s  %s", width, id, printable(str(ch["label"]), false))
		if detail := printable(str(ch["detail"]), false); detail != "" {
			line += " - " + detail
		}
		b.WriteString(line + "\n")
		indent := strings.Repeat(" ", 2+width+2)
		if desc := printable(str(ch["description"]), false); desc != "" {
			b.WriteString(indent + desc + "\n")
		}
		for i, f := range listOf(ch["fields"]) {
			lead := strings.Repeat(" ", len("fields: "))
			if i == 0 {
				lead = "fields: "
			}
			b.WriteString(indent + lead + fieldLine(f) + "\n")
		}
	}

	anyFields := false
	for _, ch := range choices {
		if len(listOf(ch["fields"])) > 0 {
			anyFields = true
		}
	}
	b.WriteString("\n")
	answer := fmt.Sprintf("qube workflows decide %s %s --choice ID", connection, runID)
	if anyFields {
		fmt.Fprintf(&b, "Answer with `%s [--data JSON]`; a choice with fields takes them in --data.\n", answer)
	} else {
		fmt.Fprintf(&b, "Answer with `%s`.\n", answer)
	}
	if names, _ := recordFields(choices); len(names) > 0 && str(mapOf(run["links"])["search"]) != "" {
		fmt.Fprintf(&b, "Find the record for `%s` with `qube workflows search %s %s --field %s QUERY`.\n",
			names[0], connection, runID, names[0])
	}
	return b.String()
}

// searchLink is the run's `links.search`, or the reason it has none.
func searchLink(run map[string]interface{}) (string, error) {
	if link := str(mapOf(run["links"])["search"]); link != "" {
		return link, nil
	}
	id, state := str(run["id"]), str(run["state"])
	if state != "awaiting_input" {
		return "", fmt.Errorf("run %s is %s, not waiting at a question, so there is nothing to search for", id, state)
	}
	return "", errors.New("run " + id + " has no search: its question has no record field to fill, or the connection has no name index (only a realtime connection has one)")
}

// searchQuery is the query of the search route: whatever the run's `links.search` carries (the
// prompt_token, which ties a search to the question as it is now) with the field and the text.
func searchQuery(link, field, text string) url.Values {
	q := url.Values{}
	if u, err := url.Parse(link); err == nil {
		q = u.Query()
	}
	q.Set("field", field)
	q.Set("q", text)
	return q
}

// searchAnswerHint is how to use the first match in the answer: the choice that takes the field,
// with the match's value as that field's data.
func searchAnswerHint(connection string, run map[string]interface{}, field string, matches []map[string]interface{}) string {
	_, choiceOf := recordFields(promptChoices(run))
	choice := choiceOf[field]
	if choice == "" {
		choice = "ID"
	}
	value := "VALUE"
	if len(matches) > 0 {
		value = str(matches[0]["value"])
	}
	data, _ := json.Marshal(map[string]string{field: value})
	if strings.Contains(string(data), "'") {
		data, _ = json.Marshal(map[string]string{field: "VALUE"})
	}
	return fmt.Sprintf("Answer with one: qube workflows decide %s %s --choice %s --data '%s'", connection, str(run["id"]), choice, data)
}

// workflowsSearch is `workflows search`: the records a waiting run's `record` field can take,
// from the run's search link (the connection's name index), for the answer's data.
func (c *ctx) workflowsSearch(args []string) {
	fs := flag.NewFlagSet("workflows search", flag.ExitOnError)
	field := fs.String("field", "", "the record field to fill, as the question lists it (list_id, ...)")
	parseAnywhere(fs, args)
	const usageLine = "qube workflows search [connection] <run-id> --field NAME QUERY"
	if *field == "" {
		ui.Usage("usage: %s\n(`qube workflows runs [connection] <run-id>` names the field)", usageLine)
	}
	conn, rest := c.connectionAnd(fs.Args(), 2, usageLine)
	id, text := rest[0], rest[1]

	cl, _ := c.appClient()
	var got struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := cl.Do("GET", v2path("/connections/{connection_id}/workflow-runs/{run_id}", conn, id), nil, nil, &got); err != nil {
		fail(err)
	}
	link, err := searchLink(got.Data)
	if err != nil {
		ui.Fail("%v", err)
	}
	// The route is built here, not followed from the link: the API key goes to the host chosen
	// with --host and no other, whatever a response says.
	var out struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := cl.Do("GET", v2path("/connections/{connection_id}/workflow-runs/{run_id}/search", conn, id), searchQuery(link, *field, text), nil, &out); err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(out.Data)
		return
	}
	var rows [][]string
	for _, m := range out.Data {
		rows = append(rows, []string{printable(str(m["value"]), false), printable(str(m["label"]), false), printable(str(m["detail"]), false)})
	}
	ui.Table(os.Stdout, []string{"VALUE", "LABEL", "DETAIL"}, rows, fmt.Sprintf("No %s matches %q.", *field, text))
	if len(out.Data) > 0 {
		ui.Info("%s", searchAnswerHint(conn, got.Data, *field, out.Data))
	}
}
