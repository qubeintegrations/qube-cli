package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// `qube match` asks a connection's name index which records a name could mean
// (POST /connections/{connection_id}/matches). Only realtime and simulated connections have
// an index. A match changes nothing, so it never asks first, and a read-only session may run it.

// matchKinds are the kinds a match takes: the `kind` enum of the match body in qube's
// connections spec, in its order (TestMatchKindsMatchSpec holds them to it). They are for
// help and completion only: the server says which kinds it takes when it refuses one.
var matchKinds = []string{
	"customer", "vendor", "employee", "other_name", "entity", "item", "account", "class",
	"standard_terms", "date_driven_terms", "sales_tax_code", "payment_method", "ship_method",
	"price_level", "customer_type", "vendor_type", "job_type", "sales_rep", "customer_msg",
}

const matchUsage = "qube match [connection] <kind> <name> [--parent P] [--exact-only] [--min-score S] [--email E] [--phone P] [--account-number N] [--company-name C] [--city C] [--postal-code Z]"

// matchFields are the flags that go in the body's `fields`, what else the app knows about a
// customer, vendor, employee or other name: flag, body field, help.
var matchFields = [][3]string{
	{"account-number", "account_number", "the record's account number: the same one is a strong match, whatever the name"},
	{"email", "email", "the record's email: the same one, with a name at least 0.4 alike, is a strong match"},
	{"phone", "phone", "the record's phone: the same one, with a name at least 0.4 alike, is a strong match"},
	{"postal-code", "postal_code", "the record's postal code: the same one, with a name at least 0.4 alike, is a strong match"},
	{"city", "city", "the record's city: ranks a candidate higher"},
	{"company-name", "company_name", "the record's company name: ranks a candidate higher (not for employee)"},
}

func (c *ctx) match(args []string) {
	fs := flag.NewFlagSet("match", flag.ExitOnError)
	parent := fs.String("parent", "", "the parent's full name or ListID, to match only its children (a job, a sub-item, a subaccount)")
	exactOnly := fs.Bool("exact-only", false, "answer only exact or none: the one record with this full name, never a fuzzy match")
	minScore := fs.Float64("min-score", 0, "this call's candidate threshold, 0.3 to 1 (default 0.44): higher lists only close names")
	fields := map[string]*string{}
	for _, f := range matchFields {
		fields[f[1]] = fs.String(f[0], "", f[2])
	}
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n\n%s\nflags (the record's fields are for customer, vendor, employee, other_name and entity):\n", matchUsage, matchHelp())
		fs.PrintDefaults()
	}
	parseAnywhere(fs, args)
	// a connection is an id, never a kind: `customer Acme Inc` is a name that wasn't quoted
	if a := fs.Args(); len(a) == 3 && contains(matchKinds, a[0]) {
		ui.Usage("usage: %s\n(a name with spaces goes in quotes: qube match %s %q)", matchUsage, a[0], strings.Join(a[1:], " "))
	}
	conn, rest := c.connectionAnd(fs.Args(), 2, matchUsage)
	body := map[string]interface{}{"kind": rest[0], "name": rest[1]}
	if *parent != "" {
		body["parent"] = *parent
	}
	known := map[string]string{}
	for k, v := range fields {
		if *v != "" {
			known[k] = *v
		}
	}
	if len(known) > 0 {
		body["fields"] = known
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "exact-only":
			body["exact_only"] = *exactOnly
		case "min-score":
			body["min_score"] = *minScore
		}
	})

	cl, _ := c.appClient()
	var out map[string]interface{}
	if err := cl.Do("POST", v2path("/connections/{connection_id}/matches", conn), nil, body, &out); err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) && apiErr.Code == "name_index_building" {
			ui.Fail("%v\n%s", err, indexBuildingHint(conn))
		}
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(out)
		return
	}
	printMatch(out)
}

// matchHelp is what `qube match -h` says before the flags: what a match answers, and the kinds.
func matchHelp() string {
	var b strings.Builder
	b.WriteString("The records in the connection's company file a name could mean, ranked: exact, strong,\n" +
		"candidates or none. From QuBe Sync's index of the file's names, so realtime and simulated\n" +
		"connections only.\n\nkinds:")
	line := len("kinds:")
	for i, k := range matchKinds {
		if i > 0 {
			b.WriteString(",")
			line++
		}
		if line+1+len(k) > 92 {
			b.WriteString("\n      ")
			line = len("      ")
		}
		b.WriteString(" " + k)
		line += 1 + len(k)
	}
	b.WriteString("\n  (entity is customers, vendors, employees and other names together; item is every item type)\n")
	return b.String()
}

// indexBuildingHint follows a 409 `name_index_building`: the index fills as the Web Connector
// checks in, and a simulated connection can be made to check in now.
func indexBuildingHint(conn string) string {
	return "Match again once it's in: `qube connections show " + conn + "` shows the name index's state.\n" +
		"On a simulated connection, `qube simulator sync " + conn + "` runs a Web Connector session now."
}

// printMatch shows a match's answer: the tier, then the record or the candidates.
func printMatch(out map[string]interface{}) {
	var records []interface{}
	if r, ok := out["record"].(map[string]interface{}); ok {
		records = append(records, r)
	}
	candidates, _ := out["candidates"].([]interface{})
	records = append(records, candidates...)
	switch tier := str(out["match"]); tier {
	case "exact":
		fmt.Println("Exact: the one record with this full name.")
	case "strong":
		fmt.Println("Strong: likely, not certain (Acme Inc and Acme LLC are strong for each other). Confirm it first.")
	case "candidates":
		fmt.Println("Candidates, best first: none is certain, so confirm the one you mean.")
	case "none":
		fmt.Println("No match.")
	default:
		fmt.Printf("Match: %s\n", tier)
	}
	var rows [][]string
	for _, x := range records {
		r, _ := x.(map[string]interface{})
		active := "yes"
		if r["is_active"] == false {
			active = "no"
		}
		rows = append(rows, []string{str(r["list_id"]), str(r["full_name"]), str(r["list"]), active, score(r["score"]), strings.Join(strs(r["reasons"]), "; ")})
	}
	if len(rows) > 0 {
		ui.Table(os.Stdout, []string{"LIST ID", "FULL NAME", "LIST", "ACTIVE", "SCORE", "REASONS"}, rows, "")
	}
	if total, ok := out["candidates_total"].(float64); ok && int(total) > len(candidates) {
		qualified := fmt.Sprintf("%d qualified", int(total))
		if out["candidates_total_complete"] == false {
			qualified = "At least " + qualified
		}
		fmt.Printf("%s; these are the first %d. Match again with what tells them apart: the number in the name, --city, --postal-code or --account-number.\n", qualified, len(candidates))
	}
	if index, ok := out["index"].(map[string]interface{}); ok && str(index["as_of"]) != "" {
		ui.Info("From the name index as of %s: changes made in QuickBooks since then aren't in it yet.", str(index["as_of"]))
	}
}

// score prints a match score with two decimals.
func score(v interface{}) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', 2, 64)
	}
	return str(v)
}

// nameIndexSummary is the `name_index` row of `connections show`: its state, and how many
// records it holds and how current it is when the API says.
func nameIndexSummary(index map[string]interface{}) string {
	var details []string
	if n, ok := index["records"].(float64); ok {
		details = append(details, fmt.Sprintf("%d records", int(n)))
	}
	if asOf := str(index["as_of"]); asOf != "" {
		details = append(details, "as of "+asOf)
	}
	if len(details) == 0 {
		return str(index["state"])
	}
	return str(index["state"]) + " (" + strings.Join(details, ", ") + ")"
}
