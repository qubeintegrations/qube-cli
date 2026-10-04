package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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
	if len(args) == 0 && c.connection != "" {
		c.useConnection(c.connection)
		return
	}
	if len(args) == 0 {
		if !c.hostExplicit {
			ui.Usage("usage: qube use <app>  |  qube use --connection <id|name|none>  |  qube use --host <host>")
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
	c.appCache, c.app = app, app.ID
	if c.connection != "" {
		c.useConnection(c.connection)
	} else if cn, ok := s.DefaultConnections[app.ID]; ok {
		ui.Info("Default connection: %s", connectionLabel(cn))
	}
}

// the app to act as: --app, else the default from `qube use`, else the only app. Looked up
// once per command.
func (c *ctx) currentApp() *api.App {
	if c.appCache != nil {
		return c.appCache
	}
	apps, _ := c.appsList()
	ref := c.app
	if ref == "" {
		ref = c.cfg.Sessions[c.host].DefaultApp
	}
	app, err := api.ResolveApp(apps, ref)
	if err != nil {
		fail(err)
	}
	c.appCache = app
	return app
}

// appClient is an API client for /api/v1 and /api/v2 acting as the current app. It sends the
// session token, not the app's key, so the server holds a read-only session to reads and a
// revoked session stops working at once.
func (c *ctx) appClient() (*api.Client, *api.App) {
	app := c.currentApp()
	cl := c.cli()
	cl.AppID = app.ID
	return cl, app
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
	if c.cfg.Sessions[c.host].ReadOnly() {
		ui.Fail("this session is read-only, so it can't read the app's API key or webhook secret. `qube login` again and choose read and write access.")
	}
	creds, err := c.cli().Credentials(c.currentApp().ID)
	if err != nil {
		fail(err)
	}
	if err := sameHost(c.host, creds.APIBaseURL); err != nil {
		fail(err)
	}
	lines := map[string]string{
		"QUBE_URL":            c.host,
		"QUBE_API_KEY":        creds.APIKey,
		"QUBE_WEBHOOK_SECRET": creds.WebhookSecret,
	}
	if ui.JSON {
		if *write != "" {
			if err := upsertEnv(*write, lines); err != nil {
				fail(err)
			}
		}
		out := map[string]interface{}{"app": creds.App}
		if *show {
			out["env"] = lines
		} else {
			// the secrets are named, never shown in part: --print is the way to see them
			out["env"] = map[string]string{"QUBE_URL": c.host}
			out["not_shown"] = secretKeys
		}
		if *write != "" {
			out["written"] = *write
		}
		ui.PrintJSON(out)
		return
	}
	if *write != "" {
		if err := upsertEnv(*write, lines); err != nil {
			fail(err)
		}
		fmt.Printf("Wrote QUBE_URL, QUBE_API_KEY and QUBE_WEBHOOK_SECRET for %q to %s\n", creds.App.Name, *write)
		return
	}
	if *show {
		for _, k := range envKeys {
			fmt.Printf("export %s=%s\n", k, lines[k])
		}
		return
	}
	fmt.Printf("App %q: QUBE_URL=%s; QUBE_API_KEY and QUBE_WEBHOOK_SECRET are not shown.\n", creds.App.Name, c.host)
	fmt.Println("Use --write .env to store them, or --print to show them (eval \"$(qube env --print)\").")
}

var envKeys = []string{"QUBE_URL", "QUBE_API_KEY", "QUBE_WEBHOOK_SECRET"}

// secretKeys are the env values no output shows any part of, unless --print asks for them.
var secretKeys = []string{"QUBE_API_KEY", "QUBE_WEBHOOK_SECRET"}

// upsertEnv replaces the QUBE_* lines of a dotenv file (or appends them), keeps every
// other line byte-for-byte, and writes atomically so an interrupted write loses nothing.
// A replaced line keeps its `export `, and appended lines get one when the file already
// exports, so a file that is `source`d still exports every QUBE_* variable. When path is a
// symbolic link, the file it points to is rewritten and the link stays a link.
func upsertEnv(path string, kv map[string]string) error {
	target, err := linkTarget(path)
	if err != nil {
		return err
	}
	existing, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var out []string
	seen := map[string]bool{}
	export := ""
	body := strings.TrimRight(string(existing), "\n")
	var lines []string
	if body != "" {
		lines = strings.Split(body, "\n")
	}
	for _, line := range lines {
		left := strings.SplitN(line, "=", 2)[0]
		key := strings.TrimSpace(left)
		if strings.HasPrefix(key, "export ") {
			key = strings.TrimSpace(strings.TrimPrefix(key, "export "))
			export = "export "
		}
		if v, ok := kv[key]; ok && strings.Contains(line, "=") {
			// whatever came before the name (indentation, `export `) stays as it was
			out = append(out, left[:strings.Index(left, key)]+key+"="+v)
			seen[key] = true
		} else {
			out = append(out, line)
		}
	}
	for _, k := range envKeys {
		if !seen[k] {
			out = append(out, export+k+"="+kv[k])
		}
	}
	text := strings.TrimRight(strings.Join(out, "\n"), "\n") + "\n"
	mode := os.FileMode(0o600)
	if info, err := os.Stat(target); err == nil {
		mode = info.Mode().Perm()
	}
	return config.WriteFileAtomic(target, []byte(text), mode)
}

// linkTarget follows path through any symbolic links to the file they name, so an atomic
// rename replaces that file rather than the link. A link to a file that doesn't exist yet
// resolves to where that file would be created.
func linkTarget(path string) (string, error) {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	for i := 0; i < 40; i++ {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return path, nil
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return path, nil
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(dest) {
			dest = filepath.Join(filepath.Dir(path), dest)
		}
		path = dest
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", path)
}
