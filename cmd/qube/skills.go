package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/qubeintegrations/qube-cli/internal/api"
	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// `qube skills install [NAME]` puts an agent skill QuBe Sync publishes where Claude Code
// finds it: GET <host>/skills/NAME/SKILL.md, written to DIR/.claude/skills/NAME/SKILL.md. It
// is the curl the docs give (curl -o .claude/skills/qube-sync-audit/SKILL.md --create-dirs
// ...), with no credentials: it needs no login, no app, and never an act-as grant.

const skillsInstallUsage = "qube skills install [NAME] [--dir DIR] [--json]"

const skillsInstallHelp = `Installs an agent skill QuBe Sync publishes: <host>/skills/NAME/SKILL.md, written to
DIR/.claude/skills/NAME/SKILL.md (directories created). NAME defaults to qube-sync-audit, the
integration audit. Run it again to update the skill; a file that is already current isn't
rewritten. No login needed.
`

// defaultSkill is the skill `qube skills install` installs when no NAME is given.
const defaultSkill = "qube-sync-audit"

var skillNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func (c *ctx) skills(args []string) {
	if len(args) == 0 || args[0] != "install" {
		ui.Usage("usage: %s", skillsInstallUsage)
	}
	fs := flag.NewFlagSet("skills install", flag.ExitOnError)
	dir := fs.String("dir", ".", "the project the skill is for: it goes in DIR/.claude/skills/NAME")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s\n\n%s\nflags:\n", skillsInstallUsage, skillsInstallHelp)
		fs.PrintDefaults()
	}
	parseAnywhere(fs, args[1:])
	if fs.NArg() > 1 {
		ui.Usage("usage: %s", skillsInstallUsage)
	}
	name := defaultSkill
	if fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if !skillNamePattern.MatchString(name) {
		ui.Usage("a skill's name is lower-case letters, digits and dashes, like %s (got %q)", defaultSkill, name)
	}
	cl := api.New(c.host, c.timeout) // no session: the skills are public
	cl.Ctx = c.bg
	res, err := installSkill(cl, name, *dir)
	if err != nil {
		fail(err)
	}
	if ui.JSON {
		ui.PrintJSON(res)
		return
	}
	switch res.Status {
	case skillWritten:
		fmt.Printf("Wrote %s (new).\n", res.Path)
	case skillUpdated:
		fmt.Printf("Updated %s: the skill had changed.\n", res.Path)
	default:
		fmt.Printf("%s is already current; nothing written.\n", res.Path)
	}
}

// What installSkill did to the file: wrote a new one, replaced one that differed, or nothing.
const (
	skillWritten = "written"
	skillUpdated = "updated"
	skillCurrent = "current"
)

type skillInstall struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// installSkill fetches the skill and writes it under dir, unless the file there already holds
// the same bytes.
func installSkill(cl *api.Client, name, dir string) (skillInstall, error) {
	body, err := cl.Skill(name)
	if err != nil {
		return skillInstall{}, skillError(cl.Host, name, err)
	}
	path := filepath.Join(dir, ".claude", "skills", name, "SKILL.md")
	res := skillInstall{Path: path, Status: skillWritten}
	old, err := os.ReadFile(path)
	switch {
	case err == nil && bytes.Equal(old, body):
		res.Status = skillCurrent
		return res, nil
	case err == nil:
		res.Status = skillUpdated
	case !os.IsNotExist(err):
		return skillInstall{}, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return skillInstall{}, err
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return skillInstall{}, err
	}
	return res, nil
}

// skillError names the status of an answer that wasn't the skill; a 404 means the host
// publishes no skill by that name.
func skillError(host, name string, err error) error {
	var e *api.Error
	if !errors.As(err, &e) {
		return err
	}
	if e.Status == http.StatusNotFound {
		return fmt.Errorf("%s has no skill named %q (GET %s is a 404)", host, name, api.SkillPath(name))
	}
	return fmt.Errorf("GET %s%s answered HTTP %d", host, api.SkillPath(name), e.Status)
}
