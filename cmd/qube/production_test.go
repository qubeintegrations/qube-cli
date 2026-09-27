package main

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// Without --yes, and with no terminal to ask (--json here), a write in a production app is
// refused with exit 2 before anything is sent. Refusing exits the process, so each case
// runs this test binary again as a child pointed at a server the parent watches.
func TestProductionWritesNeedYes(t *testing.T) {
	if host := os.Getenv("QUBE_TEST_PRODUCTION_HOST"); host != "" {
		c := &ctx{
			host: host,
			cfg:  &config.File{Sessions: map[string]config.Session{host: {Token: "qct_x"}}},
			bg:   context.Background(),
		}
		ui.JSON = true
		args := strings.Split(os.Getenv("QUBE_TEST_PRODUCTION_ARGS"), " ")
		switch args[0] {
		case "connections":
			c.connections(args[1:])
		case "api":
			c.rawAPI(args[1:])
		case "qb":
			c.qb(args[1:])
		}
		os.Exit(0) // not refused
	}
	for _, args := range []string{
		"connections update conn_1 --name Riverbend",
		"api POST /connections/conn_1/customers --data {}",
		"qb customers create conn_1 --name Northwind",
	} {
		var sent []string
		c := newAPITestApp(t, false, map[string]http.HandlerFunc{
			"/api/v2/": func(w http.ResponseWriter, r *http.Request) {
				sent = append(sent, r.Method+" "+r.URL.Path)
			},
			"/api/v2/openapi.json": func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(qbSpec)) },
		})
		cmd := exec.Command(os.Args[0], "-test.run=^TestProductionWritesNeedYes$")
		cmd.Env = append(os.Environ(), "QUBE_TEST_PRODUCTION_HOST="+c.host, "QUBE_TEST_PRODUCTION_ARGS="+args, "QUBE_CACHE_DIR="+t.TempDir())
		out, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != ui.ExitUsage {
			t.Errorf("%s: want exit %d, got %v\n%s", args, ui.ExitUsage, err, out)
		}
		if !strings.Contains(string(out), `in production app "App"? Pass --yes to confirm`) {
			t.Errorf("%s: output = %s", args, out)
		}
		if len(sent) > 0 {
			t.Errorf("%s: the server saw %v", args, sent)
		}
	}
}

func TestProductionWriteGoesAheadWithYes(t *testing.T) {
	var got string
	c := newAPITestApp(t, false, map[string]http.HandlerFunc{
		"/api/v2/connections/conn_1": func(w http.ResponseWriter, r *http.Request) {
			body, _ := ioutil.ReadAll(r.Body)
			got = r.Method + " " + string(body)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"id": "conn_1", "name": "Riverbend"}})
		},
	})
	c.yes = true
	setJSON(t, true)
	_ = captureStdout(t, func() { c.connections([]string{"update", "conn_1", "--name", "Riverbend"}) })
	if got != `PUT {"name":"Riverbend"}` {
		t.Fatalf("server saw %q", got)
	}
}

func TestDeleteAsksInASandboxAppToo(t *testing.T) {
	if os.Getenv("QUBE_TEST_DELETE_HOST") != "" {
		host := os.Getenv("QUBE_TEST_DELETE_HOST")
		c := &ctx{host: host, cfg: &config.File{Sessions: map[string]config.Session{host: {Token: "qct_x"}}}, bg: context.Background()}
		ui.JSON = true
		c.workflows([]string{"delete", "sync_order"})
		os.Exit(0)
	}
	var sent []string
	c := newAPITest(t, map[string]http.HandlerFunc{
		"/api/v2/": func(w http.ResponseWriter, r *http.Request) { sent = append(sent, r.Method+" "+r.URL.Path) },
	})
	cmd := exec.Command(os.Args[0], "-test.run=^TestDeleteAsksInASandboxAppToo$")
	cmd.Env = append(os.Environ(), "QUBE_TEST_DELETE_HOST="+c.host)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != ui.ExitUsage || len(sent) > 0 {
		t.Fatalf("want exit %d and nothing sent; got %v, sent %v\n%s", ui.ExitUsage, err, sent, out)
	}
}
