package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

func (c *ctx) appsList() ([]api.App, string) {
	apps, scope, err := c.cli().Apps()
	if err != nil {
		fail(err)
	}
	return apps, scope
}

func (c *ctx) apps() {
	apps, scope := c.appsList()
	if ui.JSON {
		ui.PrintJSON(map[string]interface{}{"scope": scope, "apps": apps})
		return
	}
	def := c.cfg.Sessions[c.host].DefaultApp
	var rows [][]string
	for _, a := range apps {
		kind := "production"
		if a.Sandbox {
			kind = "sandbox"
		}
		mark := ""
		if a.ID == def {
			mark = "*"
		}
		rows = append(rows, []string{mark, a.ID, a.Name, kind})
	}
	empty := "No apps yet: create one in the dashboard."
	if scope == "sandbox" {
		empty = "No sandbox apps (this session cannot see production apps): create a sandbox app in the dashboard."
	}
	ui.Table(os.Stdout, []string{"", "ID", "NAME", "KIND"}, rows, empty)
	if scope == "sandbox" && len(rows) > 0 {
		ui.Info("(sandbox-scoped session: production apps are not listed)")
	}
}

// use picks the default app for this host, or (`qube use --host H`) the default host.
func (c *ctx) use(args []string) {
	if len(args) == 0 {
		if !c.hostExplicit {
			ui.Usage("usage: qube use <app>  |  qube use --host <host>")
		}
		if _, ok := c.cfg.Sessions[c.host]; !ok {
			ui.Fail("not logged in to %s (run `qube login --host %s`)", c.host, strings.TrimPrefix(c.host, "https://"))
		}
		c.cfg.CurrentHost = c.host
		if err := c.cfg.Save(); err != nil {
			fail(err)
		}
		ui.Info("Default host: %s", c.host)
		return
	}
	apps, _ := c.appsList()
	app, err := api.ResolveApp(apps, args[0])
	if err != nil {
		fail(err)
	}
	s := c.cfg.Sessions[c.host]
	s.DefaultApp, s.DefaultAppName = app.ID, app.Name
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		fail(err)
	}
	ui.Info("Default app for %s: %s", c.host, app.Name)
}

// the app to act as: --app, else the default from `qube use`, else the only app
func (c *ctx) currentApp() *api.App {
	apps, _ := c.appsList()
	ref := c.app
	if ref == "" {
		ref = c.cfg.Sessions[c.host].DefaultApp
	}
	app, err := api.ResolveApp(apps, ref)
	if err != nil {
		fail(err)
	}
	return app
}

// appClient is an API client carrying the current app's key, for /api/v1 and /api/v2.
// The key is only ever used against the host that issued it.
func (c *ctx) appClient() (*api.Client, *api.Credentials) {
	app := c.currentApp()
	creds, err := c.cli().Credentials(app.ID)
	if err != nil {
		fail(err)
	}
	if err := sameHost(c.host, creds.APIBaseURL); err != nil {
		fail(err)
	}
	cl := api.New(c.host, c.timeout)
	cl.Ctx = c.bg
	cl.APIKey = creds.APIKey
	return cl, creds
}

// sameHost refuses to send an app's key anywhere but the host its credentials name.
func sameHost(host, apiBaseURL string) error {
	if apiBaseURL == "" {
		return nil
	}
	u, err := url.Parse(apiBaseURL)
	if err != nil {
		return nil
	}
	h, err := url.Parse(host)
	if err != nil {
		return nil
	}
	if !strings.EqualFold(u.Host, h.Host) {
		return fmt.Errorf("the credentials from %s are for %s://%s; not sending them to %s", host, u.Scheme, u.Host, host)
	}
	return nil
}

func (c *ctx) env(args []string) {
	fs := flag.NewFlagSet("env", flag.ExitOnError)
	write := fs.String("write", "", "append/replace QUBE_* lines in this file (e.g. .env)")
	show := fs.Bool("print", false, "print the values to stdout (they are otherwise never shown)")
	parseAnywhere(fs, args)
	_, creds := c.appClient()
	lines := map[string]string{
		"QUBE_URL":            c.host,
		"QUBE_API_KEY":        creds.APIKey,
		"QUBE_WEBHOOK_SECRET": creds.WebhookSecret,
	}
	if ui.JSON {
		if *show {
			ui.PrintJSON(map[string]interface{}{"app": creds.App, "env": lines})
		} else {
			ui.PrintJSON(map[string]interface{}{"app": creds.App, "env": map[string]string{"QUBE_URL": c.host, "QUBE_API_KEY": ui.Mask(creds.APIKey), "QUBE_WEBHOOK_SECRET": ui.Mask(creds.WebhookSecret)}})
		}
		if *write != "" {
			if err := upsertEnv(*write, lines); err != nil {
				fail(err)
			}
		}
		return
	}
	if *write != "" {
		if err := upsertEnv(*write, lines); err != nil {
			fail(err)
		}
		fmt.Printf("Wrote QUBE_URL, QUBE_API_KEY (%s) and QUBE_WEBHOOK_SECRET (%s) for %q to %s\n", ui.Mask(creds.APIKey), ui.Mask(creds.WebhookSecret), creds.App.Name, *write)
		return
	}
	if *show {
		for _, k := range envKeys {
			fmt.Printf("export %s=%s\n", k, lines[k])
		}
		return
	}
	fmt.Printf("App %q: QUBE_API_KEY=%s QUBE_WEBHOOK_SECRET=%s\n", creds.App.Name, ui.Mask(creds.APIKey), ui.Mask(creds.WebhookSecret))
	fmt.Println("Use --write .env to store them, or --print to show them (eval \"$(qube env --print)\").")
}

var envKeys = []string{"QUBE_URL", "QUBE_API_KEY", "QUBE_WEBHOOK_SECRET"}

// upsertEnv replaces the QUBE_* lines of a dotenv file (or appends them), keeps every
// other line byte-for-byte, and writes atomically so an interrupted write loses nothing.
func upsertEnv(path string, kv map[string]string) error {
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out []string
	seen := map[string]bool{}
	body := strings.TrimRight(string(existing), "\n")
	var lines []string
	if body != "" {
		lines = strings.Split(body, "\n")
	}
	for _, line := range lines {
		key := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(line, "=", 2)[0], "export "))
		if v, ok := kv[key]; ok && strings.Contains(line, "=") {
			out = append(out, key+"="+v)
			seen[key] = true
		} else {
			out = append(out, line)
		}
	}
	for _, k := range envKeys {
		if !seen[k] {
			out = append(out, k+"="+kv[k])
		}
	}
	text := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	mode := os.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return config.WriteFileAtomic(path, []byte(text), mode)
}
