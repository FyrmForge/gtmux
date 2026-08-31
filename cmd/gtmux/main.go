// Command gtmux is a proof-of-concept Go port of tmux's client-server model.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/FyrmForge/gtmux/internal/client"
	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/server"
)

const usageText = `gtmux — a tmux-like terminal multiplexer

Usage:
  gtmux <command> [args]

Commands:
  server                                run the daemon (foreground)
  new [session] [-d]                    create a session and attach; -d creates detached (no attach); no name = auto-named
  attach [-r] [session]                 attach to an existing session (-r read-only); no name = the only one
  list, ls                              list sessions
  run <session> <command...>            send a command to a session
  rename, rename-session <old> <new>    rename a session
  kill, kill-session <session>          kill one session
  has-session, has <session>            exit 0 if the session exists, else non-zero
  kill-server                           shut down the daemon and all sessions
  upgrade                               re-exec the daemon into the installed binary, sessions kept
  init-config [--force]                 write default server.lua + client.lua to ~/.config/gtmux
  help, -h, --help                      show this help
`

func usage(w io.Writer) { fmt.Fprint(w, usageText) }

func main() {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		os.Exit(1)
	}

	// Empty means "no session named": bare `gtmux attach` lets the server
	// auto-name a fresh session (tmux's numbered new-session). Every other
	// subcommand requires an explicit name below, so it never sees "".
	session := ""
	if len(os.Args) > 2 {
		session = os.Args[2]
	}

	var err error
	switch os.Args[1] {
	case "server":
		// server [--resume <state-file>]: --resume is set by an in-place upgrade
		// (the old daemon exec'd us with its PTYs and socket inherited).
		resume := ""
		for i := 2; i+1 < len(os.Args); i++ {
			if os.Args[i] == "--resume" {
				resume = os.Args[i+1]
			}
		}
		err = server.Run(resume)
	case "upgrade":
		err = client.Upgrade()
	case "new", "new-session":
		// new-session [<name>] [-d] [-t <group>]: -d creates without attaching
		// (build via `run`, attach later); -t joins a group. Name is the first
		// non-flag arg.
		name, group, detached := "", "", false
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "-t" && i+1 < len(os.Args) {
				group = os.Args[i+1]
				i++
			} else if os.Args[i] == "-d" {
				detached = true
			} else if !strings.HasPrefix(os.Args[i], "-") && name == "" {
				name = os.Args[i]
			}
		}
		if detached {
			err = client.NewDetached(name, group)
		} else {
			err = client.RunGroup(name, true, group, false)
		}
	case "a","attach":
		// attach [-r] [<session>]: -r is read-only; first non-flag is the name.
		readOnly, name := false, ""
		for i := 2; i < len(os.Args); i++ {
			if os.Args[i] == "-r" {
				readOnly = true
			} else if !strings.HasPrefix(os.Args[i], "-") && name == "" {
				name = os.Args[i]
			}
		}
		err = client.Attach(name, readOnly)
	case "list", "ls":
		err = client.List()
	case "kill-session", "kill":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: gtmux kill-session <session>")
			os.Exit(1)
		}
		err = client.KillSession(session)
	case "has-session", "has":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: gtmux has-session <session>")
			os.Exit(1)
		}
		// Scripting contract: silent, exit 0 if it exists, non-zero otherwise.
		if client.HasSession(session) != nil {
			os.Exit(1)
		}
	case "run":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: gtmux run <session> <command...>")
			os.Exit(1)
		}
		var out string
		out, err = client.Command(os.Args[2], os.Args[3:])
		if err == nil && out != "" {
			fmt.Println(out)
		}
	case "kill-server":
		err = client.KillServer()
	case "init-config":
		force := session == "--force" || session == "-f"
		failed := false
		for _, write := range []func(bool) (string, error){config.WriteDefaultServer, config.WriteDefaultClient} {
			path, e := write(force)
			if e != nil {
				fmt.Fprintln(os.Stderr, e)
				failed = true
				continue
			}
			fmt.Printf("wrote %s\n", path)
		}
		if failed {
			os.Exit(1)
		}
	case "rename-session", "rename":
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "usage: gtmux rename-session <old> <new>")
			os.Exit(1)
		}
		err = client.RenameSession(os.Args[2], os.Args[3])
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		usage(os.Stderr)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
