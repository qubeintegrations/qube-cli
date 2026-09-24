package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qubeintegrations/qube-cli/internal/api"
)

func TestParseAnywhereFlagsAfterPositionals(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	data := fs.String("data", "", "")
	pub := fs.Bool("publish", false, "")
	parseAnywhere(fs, []string{"POST", "/api/v2/x", "--data", `{"a":1}`, "--publish", "extra"})
	if *data != `{"a":1}` || !*pub || strings.Join(fs.Args(), " ") != "POST /api/v2/x extra" {
		t.Fatalf("data=%q publish=%v args=%v", *data, *pub, fs.Args())
	}
}

func TestParseAnywhereEqualsFormAndDoubleDash(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	n := fs.Int("page", 1, "")
	parseAnywhere(fs, []string{"conn-1", "--page=3", "--", "--not-a-flag"})
	if *n != 3 || strings.Join(fs.Args(), " ") != "conn-1 --not-a-flag" {
		t.Fatalf("page=%d args=%v", *n, fs.Args())
	}
}

func TestParseAnywhereStdinDashIsPositional(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	parseAnywhere(fs, []string{"-"})
	if len(fs.Args()) != 1 || fs.Arg(0) != "-" {
		t.Fatalf("args=%v", fs.Args())
	}
}

func TestUpsertEnv(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte("RAILS_ENV=development\nexport QUBE_API_KEY=old\n# comment\nQUBE_URL=https://old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := upsertEnv(p, map[string]string{"QUBE_URL": "https://dev.qubesync.com", "QUBE_API_KEY": "sk_new", "QUBE_WEBHOOK_SECRET": "whs"})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	want := "RAILS_ENV=development\nQUBE_API_KEY=sk_new\n# comment\nQUBE_URL=https://dev.qubesync.com\nQUBE_WEBHOOK_SECRET=whs\n"
	if string(got) != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != 0o644 {
			t.Fatalf("existing mode must be kept, got %o", info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("a temp file was left behind: %v", entries)
	}
}

func TestUpsertEnvCreatesPrivateFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	if err := upsertEnv(p, map[string]string{"QUBE_URL": "u", "QUBE_API_KEY": "k", "QUBE_WEBHOOK_SECRET": "s"}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "QUBE_URL=u\nQUBE_API_KEY=k\nQUBE_WEBHOOK_SECRET=s\n" {
		t.Fatalf("got %q", got)
	}
	if info, _ := os.Stat(p); runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode = %o", info.Mode().Perm())
	}
}

func TestPushBodyOmitsAbsentNameAndDescription(t *testing.T) {
	key, body, err := pushBody([]byte(`{"key":"k1","version":1,"initial":"a","states":{}}`), true, "first")
	if err != nil || key != "k1" {
		t.Fatalf("%v %v", key, err)
	}
	if _, has := body["name"]; has {
		t.Fatal("name must be omitted when the chart has none")
	}
	if body["publish"] != true || body["notes"] != "first" {
		t.Fatalf("body = %v", body)
	}
	raw, _ := json.Marshal(body)
	if strings.Contains(string(raw), "null") {
		t.Fatalf("no nulls may be sent: %s", raw)
	}

	_, body, _ = pushBody([]byte(`{"key":"k2","name":"Named","description":"d"}`), false, "")
	if body["name"] != "Named" || body["description"] != "d" || body["publish"] != nil {
		t.Fatalf("body = %v", body)
	}
	if _, _, err := pushBody([]byte(`{"version":1}`), false, ""); err == nil {
		t.Fatal("a chart without a key is refused")
	}
	if _, _, err := pushBody([]byte(`nope`), false, ""); err == nil {
		t.Fatal("non-JSON is refused")
	}
}

func TestApplyFaultFlags(t *testing.T) {
	f := map[string]interface{}{"next_response_error": "xml_error", "latency_ms": float64(50)}
	if err := applyFaultFlags(f, "closed", "3100", 0); err != nil {
		t.Fatal(err)
	}
	if f["connection_error"] != "qb_closed" || f["next_response_error"] != nil || f["latency_ms"] != nil {
		t.Fatalf("faults = %v", f)
	}
	if ns, _ := f["next_status"].(map[string]interface{}); ns["code"] != 3100 {
		t.Fatalf("next_status = %v", f["next_status"])
	}
	if err := applyFaultFlags(f, "ok", "ok", -1); err != nil || len(f) != 0 {
		t.Fatalf("clearing: %v %v", err, f)
	}
	if applyFaultFlags(f, "meteor", "", -1) == nil || applyFaultFlags(f, "", "soon", -1) == nil {
		t.Fatal("bad values are rejected")
	}
}

func TestSameHost(t *testing.T) {
	if err := sameHost("https://dev.qubesync.com", "https://dev.qubesync.com/api"); err != nil {
		t.Fatal(err)
	}
	if err := sameHost("https://evil.example", "https://dev.qubesync.com/api"); err == nil {
		t.Fatal("a key for another host must be refused")
	}
	if err := sameHost("https://x", ""); err != nil {
		t.Fatal("no api_base_url means no check")
	}
}

type fakePoller struct {
	answers []interface{} // *api.TokenGrant or error, in order
	calls   int
}

func (f *fakePoller) PollToken(string) (*api.TokenGrant, error) {
	a := f.answers[f.calls]
	f.calls++
	if g, ok := a.(*api.TokenGrant); ok {
		return g, nil
	}
	return nil, a.(error)
}

type never struct{}

func (never) Done() <-chan struct{} { return nil }

func TestPollUntilApprovedRetriesTransientErrors(t *testing.T) {
	grant := &api.TokenGrant{Token: "qct_ok"}
	p := &fakePoller{answers: []interface{}{api.ErrPending, &api.Error{Status: 502}, errors.New("dial tcp: connection refused"), api.ErrPending, grant}}
	// a plain error is not retryable, so make it a *url.Error-like net failure via api.Retryable rules:
	p.answers[2] = &api.Error{Status: 503}
	got, err := pollUntilApproved(never{}, p, "dc", time.Millisecond, time.Second)
	if err != nil || got.Token != "qct_ok" || p.calls != 5 {
		t.Fatalf("got %v err %v calls %d", got, err, p.calls)
	}
}

func TestPollUntilApprovedStopsOnDenial(t *testing.T) {
	p := &fakePoller{answers: []interface{}{api.ErrPending, &api.Error{Status: 403, Code: "access_denied", Message: "denied"}}}
	_, err := pollUntilApproved(never{}, p, "dc", time.Millisecond, time.Second)
	if err == nil || !strings.Contains(err.Error(), "denied") || p.calls != 2 {
		t.Fatalf("err %v calls %d", err, p.calls)
	}
}

func TestPollUntilApprovedGivesUpAfterFiveFailures(t *testing.T) {
	var answers []interface{}
	for i := 0; i < 6; i++ {
		answers = append(answers, &api.Error{Status: 500})
	}
	p := &fakePoller{answers: answers}
	_, err := pollUntilApproved(never{}, p, "dc", time.Millisecond, 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "after 5 attempts") || p.calls != 5 {
		t.Fatalf("err %v calls %d", err, p.calls)
	}
}

func TestPollUntilApprovedExpires(t *testing.T) {
	p := &fakePoller{answers: []interface{}{api.ErrPending, api.ErrPending, api.ErrPending, api.ErrPending, api.ErrPending, api.ErrPending}}
	_, err := pollUntilApproved(never{}, p, "dc", 2*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err %v", err)
	}
}

func TestParseTimeout(t *testing.T) {
	if parseTimeout("") != api.DefaultTimeout || parseTimeout("30") != 30*time.Second || parseTimeout("2m") != 2*time.Minute {
		t.Fatal("timeout parsing")
	}
}
