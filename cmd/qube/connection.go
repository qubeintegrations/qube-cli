package main

import (
	"fmt"
	"strings"

	"github.com/qubeintegrations/qube-cli/internal/config"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// A command that works on a connection takes it as its first argument, and may leave it out:
// then it is --connection, else the connection `qube use --connection` chose for the app.

// connectionAnd splits a command's positional arguments into its connection and the `others`
// arguments that follow it. With others+1 arguments the first is the connection; with
// `others`, the connection is --connection or the default. `usage` is shown otherwise.
func (c *ctx) connectionAnd(args []string, others int, usage string) (string, []string) {
	switch len(args) {
	case others + 1:
		return args[0], args[1:]
	case others:
		return c.chosenConnection(usage), args
	}
	ui.Usage("usage: %s", usage)
	return "", nil
}

// chosenConnection is --connection, else the app's default connection; without either it
// stops with `usage` and how to choose one.
func (c *ctx) chosenConnection(usage string) string {
	if c.connection != "" {
		return c.connection
	}
	if id := c.defaultConnection(); id != "" {
		return id
	}
	ui.Usage("which connection? usage: %s\n(or choose one for every command: `qube use --connection <id|name>`)", usage)
	return ""
}

// defaultConnection is the connection `qube use --connection` chose for the current app, or "".
func (c *ctx) defaultConnection() string {
	s := c.cfg.Sessions[c.host]
	if len(s.DefaultConnections) == 0 {
		return ""
	}
	return s.DefaultConnections[c.currentApp().ID].ID
}

// explicitConnection is a connection named on the command line (an argument or --connection)
// and never the default: for commands that remove one.
func (c *ctx) explicitConnection(args []string, usage string) string {
	switch {
	case len(args) == 1:
		return args[0]
	case len(args) == 0 && c.connection != "":
		return c.connection
	}
	ui.Usage("usage: %s (name the connection: the default isn't used here)", usage)
	return ""
}

// useConnection makes a connection of the current app its default: by id, exact name, or a
// name prefix only one connection has. "none" forgets the default.
func (c *ctx) useConnection(ref string) {
	app := c.currentApp()
	s := c.cfg.Sessions[c.host]
	if ref == "none" || ref == "-" {
		delete(s.DefaultConnections, app.ID)
		c.cfg.Sessions[c.host] = s
		if err := c.cfg.Save(); err != nil {
			fail(err)
		}
		ui.Info("No default connection for %s: commands need one named.", app.Name)
		return
	}
	cl, _ := c.appClient()
	var out struct {
		Data []map[string]interface{} `json:"data"`
	}
	if err := cl.Do("GET", v2path("/connections"), nil, nil, &out); err != nil {
		fail(err)
	}
	cn, err := resolveConnection(out.Data, ref)
	if err != nil {
		ui.Fail("%v (`qube connections list` shows them)", err)
	}
	c.setDefaultConnection(cn)
}

// setDefaultConnection saves cn as the current app's default connection.
func (c *ctx) setDefaultConnection(cn config.Connection) {
	app := c.currentApp()
	s := c.cfg.Sessions[c.host]
	if s.DefaultConnections == nil {
		s.DefaultConnections = map[string]config.Connection{}
	}
	s.DefaultConnections[app.ID] = cn
	c.cfg.Sessions[c.host] = s
	if err := c.cfg.Save(); err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(map[string]interface{}{"app": app, "connection": cn})
		return
	}
	ui.Info("Default connection for %s: %s", app.Name, connectionLabel(cn))
}

// forgetConnection drops a removed connection wherever it is a default.
func (c *ctx) forgetConnection(id string) {
	s := c.cfg.Sessions[c.host]
	changed := false
	for app, cn := range s.DefaultConnections {
		if cn.ID == id {
			delete(s.DefaultConnections, app)
			changed = true
		}
	}
	if changed {
		c.cfg.Sessions[c.host] = s
		if err := c.cfg.Save(); err != nil {
			fail(err)
		}
		ui.Info("It was the default connection; choose another with `qube use --connection`.")
	}
}

func resolveConnection(list []map[string]interface{}, ref string) (config.Connection, error) {
	lower := strings.ToLower(ref)
	var prefix []config.Connection
	for _, cn := range list {
		c := config.Connection{ID: str(cn["id"]), Name: str(cn["name"])}
		if c.ID == ref || strings.ToLower(c.Name) == lower {
			return c, nil
		}
		if c.Name != "" && strings.HasPrefix(strings.ToLower(c.Name), lower) {
			prefix = append(prefix, c)
		}
	}
	switch len(prefix) {
	case 1:
		return prefix[0], nil
	case 0:
		return config.Connection{}, fmt.Errorf("no connection matches %q", ref)
	}
	return config.Connection{}, fmt.Errorf("%q matches several connections; use the id or the full name", ref)
}

func connectionLabel(cn config.Connection) string {
	if cn.Name == "" {
		return cn.ID
	}
	return cn.Name + " (" + cn.ID + ")"
}

func accessOf(s config.Session) string {
	if s.ReadOnly() {
		return "read_only"
	}
	return "read_write"
}
