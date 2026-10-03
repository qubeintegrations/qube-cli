package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// --wait keeps a command running until QuickBooks has answered what it queued (or a
// workflow run has stopped), polling the request. It is a convenience for a terminal or a
// script; an integration passes a webhook_url and is told instead.

const defaultWaitLimit = 10 * time.Minute

// waitFirstDelay is the first pause between polls (tests shorten it).
var waitFirstDelay = time.Second

// waitFlag is --wait (up to ten minutes) or --wait=30m. It is a boolean flag to the flag
// package, so a bare --wait never swallows the next argument.
type waitFlag struct {
	on    bool
	limit time.Duration // 0: no limit
}

func (w *waitFlag) IsBoolFlag() bool { return true }

func (w *waitFlag) String() string {
	if w == nil || !w.on {
		return "false"
	}
	return w.limit.String()
}

func (w *waitFlag) Set(s string) error {
	switch s {
	case "true", "":
		w.on, w.limit = true, defaultWaitLimit
	case "false":
		w.on = false
	default:
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return fmt.Errorf("--wait takes a duration such as 30s or 5m (got %q)", s)
		}
		w.on, w.limit = true, d
	}
	return nil
}

// takeWait removes --wait / --wait=D from args (`qube qb` binds the rest to the operation).
func takeWait(args []string) ([]string, *waitFlag, error) {
	w := &waitFlag{}
	var rest []string
	for i, a := range args {
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		switch {
		case a == "--wait":
			_ = w.Set("true")
		case strings.HasPrefix(a, "--wait="):
			if err := w.Set(strings.TrimPrefix(a, "--wait=")); err != nil {
				return nil, nil, err
			}
		default:
			rest = append(rest, a)
		}
	}
	return rest, w, nil
}

var errWaitLimit = errors.New("wait limit reached")

// pacer paces the polling: every second at first, slowing to every five.
type pacer struct {
	bg       context.Context
	deadline time.Time // zero: none
	delay    time.Duration
}

func newPacer(bg context.Context, limit time.Duration) *pacer {
	p := &pacer{bg: bg, delay: waitFirstDelay}
	if limit > 0 {
		p.deadline = time.Now().Add(limit)
	}
	return p
}

func (p *pacer) next() error {
	if !p.deadline.IsZero() && time.Now().Add(p.delay).After(p.deadline) {
		return errWaitLimit
	}
	select {
	case <-p.bg.Done():
		return context.Canceled
	case <-time.After(p.delay):
	}
	if p.delay = p.delay * 3 / 2; p.delay > 5*time.Second {
		p.delay = 5 * time.Second
	}
	return nil
}

// Request states that end a request, and iteration states that end an iteration.
var (
	requestEnded   = map[string]bool{"response_received": true, "error": true, "timed_out": true, "discarded": true}
	iterationEnded = map[string]bool{"": true, "not_applicable": true, "done": true, "max_sandbox_iterations_reached": true}
)

// waitForRequest polls a queued request until QuickBooks has answered it -- every page of
// an iterated query -- or it has failed. It returns every page, in order (one for most
// requests), and whether the last one succeeded.
func (c *ctx) waitForRequest(cl *api.Client, connection string, first map[string]interface{}, limit time.Duration) ([]map[string]interface{}, bool) {
	p := newPacer(c.bg, limit)
	current, id := first, str(first["id"])
	pages := 1
	lastNote := ""
	for {
		state := str(current["state"])
		if note := stateNote(current); note != lastNote {
			ui.Info("%s", note)
			lastNote = note
		}
		if requestEnded[state] {
			if state != "response_received" || iterationEnded[str(current["iteration_state"])] {
				break
			}
			// Another page is on its way: follow the iteration to its newest page.
			all := c.requestPages(cl, connection, id)
			if len(all) > pages {
				pages = len(all)
				current, id = all[len(all)-1], str(all[len(all)-1]["id"])
				continue
			}
		}
		if err := p.next(); err != nil {
			waitStopped(err, connection, id, state)
		}
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}/queued-requests/{id}", connection, id), nil, nil, &out); err != nil {
			fail(err)
		}
		current = out.Data
	}
	all := []map[string]interface{}{current}
	if pages > 1 {
		all = c.requestPages(cl, connection, id)
	}
	if str(current["iteration_state"]) == "max_sandbox_iterations_reached" {
		ui.Info("A sandbox app stops an iterated query after a few pages; production apps get every page.")
	}
	return all, str(current["state"]) == "response_received"
}

func (c *ctx) requestPages(cl *api.Client, connection, id string) []map[string]interface{} {
	var out struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := cl.Do("GET", v2path("/connections/{connection_id}/queued-requests/{id}/pages", connection, id), nil, nil, &out); err != nil {
		fail(err)
	}
	return out.Data
}

// stateNote says where a request is, for stderr while waiting.
func stateNote(r map[string]interface{}) string {
	page := ""
	if n, ok := r["page"].(float64); ok && n > 1 {
		page = fmt.Sprintf("page %d: ", int(n))
	}
	switch str(r["state"]) {
	case "waiting":
		return page + "queued; waiting for the connection's Web Connector to run"
	case "ticketed":
		return page + "the Web Connector has started a session"
	case "sent":
		return page + "sent to QuickBooks"
	case "retryable":
		msg := "QuickBooks couldn't take it"
		if e, ok := r["error"].(map[string]interface{}); ok && str(e["user_message"]) != "" {
			msg += ": " + str(e["user_message"])
		}
		return page + msg + ". QuBe keeps trying on the Web Connector's next runs."
	case "response_received":
		if str(r["iteration_state"]) == "continue" {
			return page + "answered; QuickBooks has more pages"
		}
		return page + "answered"
	case "error":
		return page + "failed"
	case "timed_out":
		return page + "timed out without an answer (QuickBooks may still have applied it)"
	case "discarded":
		return page + "discarded before QuickBooks got it"
	}
	return page + str(r["state"])
}

// printAnswer shows what QuickBooks said: the whole request(s) under --json, else each
// page's response_json (and the error, if it failed). It exits 1 unless it succeeded.
func printAnswer(pages []map[string]interface{}, ok bool) {
	if ui.JSON {
		if len(pages) == 1 {
			ui.PrintJSON(pages[0])
		} else {
			ui.PrintJSON(pages)
		}
	} else {
		for _, p := range pages {
			if len(pages) > 1 {
				fmt.Printf("# page %v\n", p["page"])
			}
			if p["response_json"] != nil {
				ui.PrintJSON(p["response_json"])
			}
		}
		last := pages[len(pages)-1]
		if e, isMap := last["error"].(map[string]interface{}); isMap && !ok {
			ui.Warn("%s", errorLine(e))
		}
	}
	if !ok {
		os.Exit(ui.ExitFailure)
	}
}

func errorLine(e map[string]interface{}) string {
	parts := []string{}
	for _, k := range []string{"error_type", "error_code", "user_message"} {
		if s := str(e[k]); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return compactJSON(e)
	}
	return strings.Join(parts, ": ")
}

// waitStopped ends a wait that ran out of time or was interrupted; the request (or run)
// carries on without us.
func waitStopped(err error, connection, id, state string) {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "qube: stopped waiting; %s is still %s. `qube requests show %s %s` shows it later.\n", id, state, connection, id)
		os.Exit(ui.ExitInterrupted)
	}
	ui.Fail("still %s when the wait ran out; %s stays queued. `qube requests show %s %s` shows it later, or wait longer with --wait=30m.", state, id, connection, id)
}

// Workflow run states that stop a run, for now or for good.
var runStopped = map[string]bool{"awaiting_input": true, "completed": true, "failed": true, "cancelled": true}

// waitForRun polls a workflow run until it stops: it ends, or it needs a decision.
func (c *ctx) waitForRun(cl *api.Client, connection string, run map[string]interface{}, limit time.Duration) map[string]interface{} {
	p := newPacer(c.bg, limit)
	id := str(run["id"])
	for !runStopped[str(run["state"])] {
		if err := p.next(); err != nil {
			if errors.Is(err, context.Canceled) {
				fmt.Fprintf(os.Stderr, "qube: stopped waiting; run %s is still %s.\n", id, str(run["state"]))
				os.Exit(ui.ExitInterrupted)
			}
			ui.Fail("run %s is still %s when the wait ran out. `qube workflows runs %s %s` shows it later.", id, str(run["state"]), connection, id)
		}
		var out struct {
			Data map[string]interface{} `json:"data"`
		}
		if err := cl.Do("GET", v2path("/connections/{connection_id}/workflow-runs/{run_id}", connection, id), nil, nil, &out); err != nil {
			fail(err)
		}
		run = out.Data
	}
	return run
}

// printRun shows where a run stopped and exits 1 if it failed or was cancelled.
func printRun(connection string, run map[string]interface{}) {
	id, state := str(run["id"]), str(run["state"])
	if ui.JSON {
		ui.PrintJSON(run)
	} else {
		switch state {
		case "awaiting_input":
			fmt.Printf("Run %s is waiting for a decision:\n", id)
			ui.PrintJSON(run["prompt"])
			fmt.Printf("Answer with `qube workflows decide %s %s <option> [--data JSON]`.\n", connection, id)
		case "completed":
			fmt.Printf("Run %s completed (%s).\n", id, str(run["outcome"]))
			if run["output"] != nil {
				ui.PrintJSON(run["output"])
			}
		default:
			fmt.Printf("Run %s %s (%s).\n", id, state, str(run["outcome"]))
			if s := str(run["error"]); s != "" {
				fmt.Println(s)
			}
		}
	}
	if state == "failed" || state == "cancelled" {
		os.Exit(ui.ExitFailure)
	}
}
