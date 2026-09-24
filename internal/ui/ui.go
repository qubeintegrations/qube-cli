// Package ui: printing for humans and for agents (--json).
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"text/tabwriter"
)

var JSON bool // --json: print machine-readable output only

// Exit codes: 0 ok, 1 the command failed, 2 the command line was wrong, 130 interrupted.
const (
	ExitFailure     = 1
	ExitUsage       = 2
	ExitInterrupted = 130
)

func PrintJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// PrintRaw writes bytes and makes sure there is a trailing newline.
func PrintRaw(b []byte) {
	os.Stdout.Write(b)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		fmt.Println()
	}
}

// Table prints a header and rows; with no rows it prints `empty` instead of a lonely header.
func Table(w io.Writer, header []string, rows [][]string, empty string) {
	if len(rows) == 0 {
		fmt.Fprintln(w, empty)
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, r := range rows {
		fmt.Fprintln(tw, strings.Join(r, "\t"))
	}
	tw.Flush()
}

// Info is a note for a human (stderr); silent under --json.
func Info(format string, a ...interface{}) {
	if JSON {
		return
	}
	fmt.Fprintf(os.Stderr, format+"\n", a...)
}

// Warn is always printed (stderr), even under --json.
func Warn(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "qube: "+format+"\n", a...)
}

func Fail(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "qube: "+format+"\n", a...)
	os.Exit(ExitFailure)
}

// Usage reports a wrong command line and exits 2.
func Usage(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "qube: "+format+"\n", a...)
	os.Exit(ExitUsage)
}

// Mask shows a secret's shape without its value: sk_62pC…Xwo.
func Mask(s string) string {
	if len(s) <= 10 {
		return strings.Repeat("•", len(s))
	}
	return s[:7] + "…" + s[len(s)-3:]
}

// OpenBrowser tries the platform opener; failing is fine, the URL is printed anyway.
func OpenBrowser(u string) bool {
	if os.Getenv("QUBE_NO_BROWSER") != "" {
		return false
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start() == nil
}
