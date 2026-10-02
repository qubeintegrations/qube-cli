package main

import (
	"fmt"

	"github.com/qubeintegrations/qube-cli/internal/ui"
)

// The command tree the completion scripts describe. Keep in step with main() and usage().
var commandTree = map[string][]string{
	"":            {"login", "logout", "status", "whoami", "sessions", "apps", "use", "env", "connections", "requests", "simulator", "workflows", "qb", "api", "completion", "version", "help"},
	"sessions":    {"revoke"},
	"connections": {"list", "create", "show", "update", "delete", "qwc", "password", "onboarding-url"},
	"requests":    {"list", "show", "pages", "tail", "discard"},
	"simulator":   {"show", "reset", "sync", "faults"},
	"workflows":   {"list", "show", "push", "validate", "publish", "unpublish", "delete", "versions", "usage", "schema", "templates", "install", "run", "runs", "decide", "cancel", "delete-run"},
	"api":         {"GET", "POST", "PUT", "PATCH", "DELETE"},
	"completion":  {"bash", "zsh", "fish"},
}

var globalFlags = []string{"--json", "--host", "--app", "--timeout", "--yes", "--connection"}

// `qb` resources, verbs and each operation's flags (and their values, where the spec lists
// them) come from the host's operation list, which the scripts read with
// `qube qb --complete [resource [verb [previous word]]]` (cache only, so a TAB never waits
// on the network).

func completion(args []string) {
	if len(args) < 1 {
		ui.Usage("usage: qube completion bash|zsh|fish")
	}
	subs := ""
	for _, cmd := range commandTree[""] {
		if len(commandTree[cmd]) > 0 {
			subs += fmt.Sprintf("    %s) words=%q ;;\n", cmd, join(commandTree[cmd]))
		}
	}
	switch args[0] {
	case "bash":
		fmt.Printf(`# qube bash completion: eval "$(qube completion bash)"  or save to /etc/bash_completion.d/qube
_qube() {
  local cur prev words
  cur="${COMP_WORDS[COMP_CWORD]}"
  prev="${COMP_WORDS[1]}"
  words=%q
  if [ "$prev" = qb ] && [ "$COMP_CWORD" -eq 2 ]; then
    words=$(qube qb --complete 2>/dev/null)
  elif [ "$prev" = qb ] && [ "$COMP_CWORD" -eq 3 ]; then
    words=$(qube qb --complete "${COMP_WORDS[2]}" 2>/dev/null)
  elif [ "$prev" = qb ]; then
    words=$(qube qb --complete "${COMP_WORDS[2]}" "${COMP_WORDS[3]}" "${COMP_WORDS[COMP_CWORD-1]}" 2>/dev/null)
    COMPREPLY=($(compgen -W "$words" -- "$cur"))
    return
  elif [ "$COMP_CWORD" -ge 2 ]; then
    case "$prev" in
%s    *) words="" ;;
    esac
  fi
  case "$cur" in
    -*) words=%q ;;
  esac
  COMPREPLY=($(compgen -W "$words" -- "$cur"))
}
complete -F _qube qube
`, join(commandTree[""]), subs, join(globalFlags))
	case "zsh":
		fmt.Printf(`#compdef qube
# qube zsh completion: eval "$(qube completion zsh)"  or save as _qube in a directory on $fpath
_qube() {
  local -a words_list
  local prev="${words[2]}"
  if (( CURRENT == 2 )); then
    words_list=(%s)
  elif [[ "$prev" == qb ]] && (( CURRENT == 3 )); then
    words_list=(${(f)"$(qube qb --complete 2>/dev/null)"})
  elif [[ "$prev" == qb ]] && (( CURRENT == 4 )); then
    words_list=(${(f)"$(qube qb --complete "${words[3]}" 2>/dev/null)"})
  elif [[ "$prev" == qb ]]; then
    words_list=(${(f)"$(qube qb --complete "${words[3]}" "${words[4]}" "${words[CURRENT-1]}" 2>/dev/null)"})
    compadd -- "${words_list[@]}"
    return
  else
    case "$prev" in
%s    *) words_list=() ;;
    esac
  fi
  if [[ "${words[CURRENT]}" == -* ]]; then
    words_list=(%s)
  fi
  compadd -- "${words_list[@]}"
}
compdef _qube qube
`, join(commandTree[""]), zshSubs(), join(globalFlags))
	case "fish":
		fmt.Printf("# qube fish completion: qube completion fish > ~/.config/fish/completions/qube.fish\n")
		fmt.Printf("complete -c qube -f\n")
		for _, cmd := range commandTree[""] {
			fmt.Printf("complete -c qube -n '__fish_use_subcommand' -a %s\n", cmd)
			for _, sub := range commandTree[cmd] {
				fmt.Printf("complete -c qube -n '__fish_seen_subcommand_from %s' -a %s\n", cmd, sub)
			}
		}
		fmt.Printf("complete -c qube -n '__fish_seen_subcommand_from qb; and test (count (commandline -opc)) -eq 2' -a '(qube qb --complete 2>/dev/null)'\n")
		fmt.Printf("complete -c qube -n '__fish_seen_subcommand_from qb; and test (count (commandline -opc)) -eq 3' -a '(qube qb --complete (commandline -opc)[3] 2>/dev/null)'\n")
		fmt.Printf("complete -c qube -n '__fish_seen_subcommand_from qb; and test (count (commandline -opc)) -ge 4' -a '(qube qb --complete (commandline -opc)[3] (commandline -opc)[4] (commandline -opc)[-1] 2>/dev/null)'\n")
		for _, f := range globalFlags {
			fmt.Printf("complete -c qube -l %s\n", f[2:])
		}
	default:
		ui.Usage("usage: qube completion bash|zsh|fish")
	}
}

func zshSubs() string {
	out := ""
	for _, cmd := range commandTree[""] {
		if len(commandTree[cmd]) > 0 {
			out += fmt.Sprintf("      %s) words_list=(%s) ;;\n", cmd, join(commandTree[cmd]))
		}
	}
	return out
}

func join(list []string) string {
	out := ""
	for i, s := range list {
		if i > 0 {
			out += " "
		}
		out += s
	}
	return out
}
