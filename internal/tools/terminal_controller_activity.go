package tools

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// This is the pinned provider's private prompt-evidence protocol. Ordinary
// terminal text never supplies evidence that an unattended shell is idle.
type terminalShellActivity struct {
	directory, observed, invalidated string
	revision                         uint64
}

func (a *terminalShellActivity) read() string {
	f, err := os.Open(filepath.Join(a.directory, "state"))
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1025))
	if err != nil || len(data) > 1024 {
		return ""
	}
	return string(data)
}
func (a *terminalShellActivity) invalidate() { a.invalidated = a.read(); a.revision++ }
func (a *terminalShellActivity) inspect(pid int) (string, uint64) {
	record := a.read()
	if record != a.observed {
		a.observed = record
		a.revision++
	}
	fields := strings.Split(strings.TrimSuffix(record, "\n"), ":")
	if record == a.invalidated || len(fields) != 3 || fields[0] != strconv.Itoa(pid) {
		return "unknown", a.revision
	}
	if _, err := strconv.ParseUint(fields[1], 10, 64); err != nil {
		return "unknown", a.revision
	}
	if fields[2] != "idle" && fields[2] != "busy" {
		return "unknown", a.revision
	}
	return fields[2], a.revision
}
func (a *terminalShellActivity) dispose() { _ = os.RemoveAll(a.directory) }
func terminalShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
func prepareTerminalShellActivity(shell TerminalShell, env []string) (*terminalShellActivity, []string, []string, error) {
	args := append([]string{}, shell.Args...)
	name := filepath.Base(shell.Path)
	if len(args) != 1 || args[0] != "-i" || (name != "bash" && name != "zsh") {
		return nil, args, env, nil
	}
	directory, err := os.MkdirTemp("", "dsgo-shell-activity-")
	if err != nil {
		return nil, nil, nil, err
	}
	a := &terminalShellActivity{directory: directory}
	state := terminalShellQuote(filepath.Join(directory, "state"))
	guards := terminalShellQuote(filepath.Join(directory, "guards"))
	var filename, source string
	if name == "bash" {
		filename = filepath.Join(directory, "bashrc")
		source = fmt.Sprintf(`[[ ! -r ~/.bashrc ]] || builtin source ~/.bashrc
if (( BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4) )) && [[ ! $(declare -p PROMPT_COMMAND PS0 2>/dev/null) =~ declare\ -[^[:space:]]*r ]]; then
  __dsh_shell_pid=$BASHPID; __dsh_shell_sequence=0
  __dsh_shell_idle() {
    local result=$?
    if [[ $BASHPID == "$__dsh_shell_pid" ]]; then
      (( ++__dsh_shell_sequence ))
      local activity=idle
      builtin trap -p >| %s
      [[ ! -s %s ]] || activity=unknown
      builtin printf '%%s:%%s:%%s\n' "$BASHPID" "$__dsh_shell_sequence" "$activity" >| %s
    fi
    return "$result"
  }
  if [[ $(declare -p PROMPT_COMMAND 2>/dev/null) == "declare -a "* ]]; then
    PROMPT_COMMAND+=(__dsh_shell_idle)
  else
    PROMPT_COMMAND="${PROMPT_COMMAND}"$'\n'"__dsh_shell_idle"
  fi
  PS0+=%s
fi
`, guards, guards, state, terminalShellQuote("$(builtin printf '%s:%s:busy' \"$__dsh_shell_pid\" \"$__dsh_shell_sequence\" >| "+state+")"))
		args = []string{"--rcfile", filename, "-i"}
	} else {
		filename = filepath.Join(directory, ".zshenv")
		original := "unset ZDOTDIR"
		for _, entry := range env {
			if strings.HasPrefix(entry, "ZDOTDIR=") {
				original = "ZDOTDIR=" + terminalShellQuote(strings.TrimPrefix(entry, "ZDOTDIR="))
			}
		}
		source = fmt.Sprintf(`%s
[[ ! -r ${ZDOTDIR:-$HOME}/.zshenv ]] || builtin source "${ZDOTDIR:-$HOME}/.zshenv"
typeset -g __dsh_shell_pid=$$ __dsh_shell_sequence=0
__dsh_shell_activity() {
  (( ZSH_SUBSHELL == 0 && $$ == __dsh_shell_pid )) || return
  (( ++__dsh_shell_sequence ))
  builtin printf '%%s:%%s:%%s\n' "$$" "$__dsh_shell_sequence" "$1" >| %s
  return 0
}
__dsh_shell_idle() {
  { builtin trap; zle -F; } >| %s
  if [[ $CONTEXT != start || -n $BUFFER ]]; then __dsh_shell_activity busy
  elif [[ -s %s || -n ${(k)functions[(I)TRAP*]} ]]; then __dsh_shell_activity unknown
  else __dsh_shell_activity idle; fi
}
__dsh_shell_busy() { __dsh_shell_activity busy }
__dsh_shell_init() {
  autoload -Uz add-zle-hook-widget add-zsh-hook
  add-zle-hook-widget line-init __dsh_shell_idle
  add-zle-hook-widget line-finish __dsh_shell_busy
  add-zsh-hook preexec __dsh_shell_busy
  precmd_functions=(${precmd_functions:#__dsh_shell_init})
}
typeset -ga precmd_functions
precmd_functions+=(__dsh_shell_init)
`, original, state, guards, guards)
		env = append(filteredEnv(env, "ZDOTDIR"), "ZDOTDIR="+directory)
	}
	if err = os.WriteFile(filename, []byte(source), 0600); err != nil {
		a.dispose()
		return nil, nil, nil, err
	}
	return a, args, env, nil
}
