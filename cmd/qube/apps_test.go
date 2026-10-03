package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testAPIKey = "sk_62pCq9ZrT4vYx1LmN8sWdXwo"
	testSecret = "V6rNBWlk3JqP0aZs9TyUe7HxU="
)

// envTest is a fake host whose credentials are a realistic key and secret.
func envTest(t *testing.T) *ctx {
	t.Helper()
	return newAPITest(t, map[string]http.HandlerFunc{
		"/api/cli/apps/app1/credentials": func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{
				"app":            map[string]interface{}{"id": "app1", "name": "App", "sandbox": true},
				"api_key":        testAPIKey,
				"webhook_secret": testSecret,
				"api_base_url":   "http://" + r.Host,
			}})
		},
	})
}

// leaks reports any run of 3 or more characters of either secret, beyond the fixed sk_ prefix.
func leaks(out string) string {
	for _, secret := range []string{strings.TrimPrefix(testAPIKey, "sk_"), testSecret} {
		for i := 0; i+3 <= len(secret); i++ {
			if strings.Contains(out, secret[i:i+3]) {
				return secret[i : i+3]
			}
		}
	}
	return ""
}

// `qube env`, `--write` and `--json` name the secrets and the app, and print nothing of either value.
func TestEnvNeverPrintsPartOfASecret(t *testing.T) {
	for _, tc := range []struct {
		name string
		json bool
		args func(dir string) []string
		want []string
	}{
		{"plain", false, func(string) []string { return nil }, []string{`App "App"`, "not shown", "--print"}},
		{"write", false, func(dir string) []string { return []string{"--write", filepath.Join(dir, ".env")} },
			[]string{`Wrote QUBE_URL, QUBE_API_KEY and QUBE_WEBHOOK_SECRET for "App" to `}},
		{"json", true, func(string) []string { return nil }, []string{`"not_shown"`, `"QUBE_API_KEY"`}},
		{"json write", true, func(dir string) []string { return []string{"--write", filepath.Join(dir, ".env")} }, []string{`"written"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := envTest(t)
			setJSON(t, tc.json)
			dir := t.TempDir()
			args := tc.args(dir)
			out := captureStdout(t, func() { c.env(args) })
			if frag := leaks(out); frag != "" {
				t.Fatalf("output shows %q of a secret:\n%s", frag, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			if len(args) > 0 {
				written, err := os.ReadFile(args[1])
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(written), "QUBE_API_KEY="+testAPIKey+"\n") || !strings.Contains(string(written), "QUBE_WEBHOOK_SECRET="+testSecret+"\n") {
					t.Fatalf(".env = %s", written)
				}
			}
		})
	}
}

// --print is the explicit way to see the values, in full.
func TestEnvPrintShowsTheValues(t *testing.T) {
	c := envTest(t)
	setJSON(t, false)
	out := captureStdout(t, func() { c.env([]string{"--print"}) })
	if !strings.Contains(out, "export QUBE_API_KEY="+testAPIKey+"\n") || !strings.Contains(out, "export QUBE_WEBHOOK_SECRET="+testSecret+"\n") {
		t.Fatalf("output = %s", out)
	}
}
