package client

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	lua "github.com/yuin/gopher-lua"
	"golang.org/x/term"

	"github.com/FyrmForge/gtmux/internal/config"
	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/proto"
	"github.com/FyrmForge/gtmux/internal/workspace"
)

// workspaceQuery sends one WorkspaceRequest and returns the reply.
func workspaceQuery(req *proto.WorkspaceRequest) (*proto.WorkspaceReply, error) {
	reply, err := oneShotReply(&proto.ClientMsg{Workspace: req})
	if err != nil {
		return nil, err
	}
	if reply.Workspace == nil {
		return &proto.WorkspaceReply{}, nil
	}
	return reply.Workspace, nil
}

// insideSession returns the session this process runs in when it's a pane of
// THIS daemon ($GTMUX = "sock,pid,name"), else "". $GTMUX's name is from when
// the pane spawned, so after a rename-session it's stale: confirm it still
// holds our pane ($GTMUX_PANE), else find the session that does.
func insideSession() string {
	parts := strings.SplitN(os.Getenv("GTMUX"), ",", 3)
	if len(parts) != 3 || parts[0] != proto.SockPath() {
		return ""
	}
	name, pane := parts[2], os.Getenv("GTMUX_PANE")
	if pane == "" {
		return name
	}
	holds := func(s string) bool {
		out, err := Command(s, []string{"display-message", "-p", "-t", pane, "#{pane_id}"})
		return err == nil && strings.TrimSpace(out) == pane
	}
	if holds(name) {
		return name
	}
	if sessions, err := listSessions(); err == nil {
		for _, s := range sessions {
			if s.Name != name && holds(s.Name) {
				return s.Name
			}
		}
	}
	return name
}

// Workspace is `gtmux workspace|ws [refresh] [<ws> [<project>]]` and
// `gtmux ws new [<ws>] [<name>] [--template T] [--gh] [--private]`.
func Workspace(args []string) error {
	if err := ensureServer(); err != nil {
		return err
	}
	inside := insideSession()
	if len(args) > 0 && args[0] == "home" { // gtmux ws home [<ws>]
		if len(args) > 2 {
			return fmt.Errorf("usage: gtmux ws home [<ws>]")
		}
		ws := ""
		if len(args) == 2 {
			ws = args[1]
		}
		return runAction(inside, []string{"workspace-open", ws, workspace.HomeProject})
	}
	if len(args) > 0 && args[0] == "refresh" {
		ws := ""
		if len(args) > 1 {
			ws = args[1]
		}
		_, err := workspaceQuery(&proto.WorkspaceRequest{Op: "refresh", Workspace: ws, Session: inside})
		return err
	}
	var action []string // the session command that does the work
	if len(args) > 0 && args[0] == "new" {
		var pos []string
		tpl, gh, priv, flags := "", false, false, false
		for i := 1; i < len(args); i++ {
			switch a := args[i]; a {
			case "--gh":
				gh, flags = true, true
			case "--private":
				priv, flags = true, true
			case "--template":
				if i+1 >= len(args) {
					return fmt.Errorf("--template needs a name")
				}
				i++
				tpl, flags = args[i], true
			default:
				if strings.HasPrefix(a, "-") {
					return fmt.Errorf("ws new: unknown flag %s", a)
				}
				pos = append(pos, a)
			}
		}
		if len(pos) > 2 {
			return fmt.Errorf("ws new: too many arguments: %s", strings.Join(pos[2:], " "))
		}
		if flags && len(pos) < 2 {
			return fmt.Errorf("ws new: flags need both <ws> and <name>")
		}
		if priv && !gh {
			return fmt.Errorf("ws new: --private needs --gh")
		}
		pos = append(pos, "", "")
		if flags { // fully specified: no wizard
			b := map[bool]string{true: "1", false: "0"}
			action = []string{"workspace-new", pos[0], pos[1], tpl, b[gh], b[priv]}
		} else {
			var ok bool
			var err error
			if action, ok, err = pickStandalone(pos[0], inside, true, pos[1]); err != nil || !ok {
				return err
			}
		}
	} else {
		ws, project := "", ""
		if len(args) > 0 {
			ws = args[0]
		}
		if len(args) > 1 {
			project = args[1]
		}
		action = []string{"workspace-open", ws, project}
		if project == "" {
			var ok bool
			var err error
			if action, ok, err = pickStandalone(ws, inside, false, ""); err != nil || !ok {
				return err
			}
		}
	}
	return runAction(inside, action)
}

// Temp is `gtmux temp|t [<name>]` (open/create; no name = next number) and
// `gtmux temp rm <name>`.
func Temp(args []string) error {
	if err := ensureServer(); err != nil {
		return err
	}
	if len(args) > 0 && args[0] == "rm" {
		if len(args) != 2 {
			return fmt.Errorf("usage: gtmux temp rm <name>")
		}
		_, err := workspaceQuery(&proto.WorkspaceRequest{Op: "temp-rm", Project: args[1]})
		return err
	}
	if len(args) > 1 {
		return fmt.Errorf("usage: gtmux temp [<name>] | gtmux temp rm <name>")
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	return runAction(insideSession(), []string{"workspace-open", "temp", name})
}

// runAction carries out a workspace-open/-new action: inside gtmux the daemon
// builds and switches the acting client (never a nested attach); outside, a
// one-shot request builds it and this process becomes `gtmux attach`.
func runAction(inside string, action []string) error {
	if inside != "" {
		_, err := Command(inside, action)
		return err
	}
	req := &proto.WorkspaceRequest{
		Op: "open", Workspace: action[1], Project: action[2],
		Env: map[string]string{"SSH_AUTH_SOCK": os.Getenv("SSH_AUTH_SOCK")},
	}
	if action[0] == "workspace-new" {
		req.Op, req.Template, req.GitHub, req.Private = "new", action[3], action[4] == "1", action[5] == "1"
		fmt.Fprintf(os.Stderr, "creating %s…\n", req.Project)
	} else {
		fmt.Fprintf(os.Stderr, "opening %s %s…\n", req.Workspace, req.Project)
	}
	rep, err := workspaceQuery(req)
	if err != nil {
		return err
	}
	// Exec, not an in-process Attach: the picker's stdin reader is parked in a
	// blocking Read and would steal the first keystroke from the attach.
	self, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(self, []string{"gtmux", "attach", rep.Session}, os.Environ())
}

// pickStandalone runs gtmux.workspace.picker on a blank screen — no attached
// session, no compositor: the component paints a canvas, keys feed its on_key,
// and a workspace-open/-new op ends the loop with that action. Esc/close = !ok.
func pickStandalone(ws, session string, isNew bool, name string) ([]string, bool, error) {
	// A bad workspace name is a plain CLI error, not a full-screen picker
	// holding only that error.
	if _, err := workspaceQuery(&proto.WorkspaceRequest{Op: "list", Workspace: ws, Session: session}); err != nil {
		return nil, false, err
	}
	cfg, binds := config.LoadClient(config.ClientConfigPath())
	defer binds.Close()
	binds.Hooks = config.WidgetHooks{
		Workspace: workspaceQuery,
		Context:   func() map[string]string { return map[string]string{"session": session} },
	}
	var modal *config.ModalOpen
	for _, op := range binds.RunWorkspacePicker(ws, isNew, name) {
		if op.Modal != nil {
			modal = op.Modal
		}
	}
	if modal == nil || modal.Component == nil {
		return nil, false, fmt.Errorf("gtmux.workspace.picker did not open a picker (client.lua)")
	}

	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, false, err
	}
	os.Stdout.WriteString("\x1b[?1049h\x1b[?25l") // alt screen, hide cursor
	defer func() {
		os.Stdout.WriteString("\x1b[?25h\x1b[?1049l")
		term.Restore(fd, old)
	}()

	keys := make(chan []byte)
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				close(keys)
				return
			}
			keys <- append([]byte(nil), buf[:n]...)
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	tick := time.NewTicker(time.Second) // live rows while a refresh lands
	defer tick.Stop()

	var state *lua.LTable // created by the first RunComponent, then threaded through
	paint := func(clear bool) {
		cols, rows, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil {
			cols, rows = 80, 24
		}
		w, h := min(modal.Width, cols), min(modal.Height, rows)
		var cv *config.Canvas
		cv, _, state, _ = binds.RunComponent(modal.Component, state, w, h, cfg.StatusFG, cfg.StatusBG, cfg.StatusAttr)
		var b strings.Builder
		b.WriteString("\x1b[?2026h")
		if clear {
			b.WriteString("\x1b[2J")
		}
		ox, oy := (cols-w)/2, (rows-h)/2
		for y := 0; y < cv.H; y++ {
			line := make(emu.Line, cv.W)
			for x := range line {
				line[x], _ = cv.At(x, y)
			}
			fmt.Fprintf(&b, "\x1b[%d;%dH", oy+y+1, ox+1)
			emu.WriteLine(&b, line)
		}
		b.WriteString("\x1b[?2026l")
		os.Stdout.WriteString(b.String())
	}
	paint(true)
	for {
		select {
		case <-winch:
			paint(true)
		case <-tick.C:
			paint(false)
		case data, okRead := <-keys:
			if !okRead {
				return nil, false, nil
			}
			for _, k := range decodeKeys(data) {
				ops, closed := binds.RunKey(modal.OnKey, k, state)
				for _, op := range ops {
					if (len(op.Action) == 3 && op.Action[0] == "workspace-open") ||
						(len(op.Action) == 6 && op.Action[0] == "workspace-new") {
						return op.Action, true, nil
					}
				}
				if closed {
					return nil, false, nil
				}
			}
			paint(false)
		}
	}
}
