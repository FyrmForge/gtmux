//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/gtmux/internal/emu"
	"github.com/FyrmForge/gtmux/internal/harness"
)

// promptReady waits for the shell prompt to draw on the top row (the status
// bar renders immediately, so waiting for it isn't enough).
func promptReady(c *harness.Client) {
	c.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })
}

// TestCopyMode covers the whole copy-mode flow: enter, search to a scrollback
// line, select+yank (which exits), and paste the buffer back.
func TestCopyMode(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.TypeLine("for i in $(seq 1 100); do echo row$i; done")
	c.WaitForText("row100")

	c.Prefix("[")
	// The copy-mode help line is wider than 80 cols, so the status bar keeps
	// its tail (statusrender.go) — "(copy-mode)" clips off the left. Assert on
	// tail-visible markers instead: the help ends in "q quit", and the
	// position reads "line N/M".
	c.WaitForStatus("q quit")

	c.Type("g") // jump to the top so the forward search can find row50 below
	c.WaitFor(func(s *harness.Screen) bool { return s.Status().Has("line 1/") })
	c.Type("/")
	c.TypeLine("row50") // Enter submits the search
	c.WaitForText("row50")

	c.Type("v")  // start selection
	c.Type("jj") // extend down two lines
	c.Type("y")  // yank -> exits copy-mode (help line, and its "q quit", gone)
	c.WaitFor(func(s *harness.Screen) bool { return !s.Status().Has("q quit") })

	c.TypeLine("cat")
	c.Prefix("]") // paste the yanked lines into cat
	c.WaitForText("row50")
	c.Ctrl('c')
}

// TestCopyCommand: with copy-command set, a copy-mode yank pipes the selection
// to the command's stdin (server-side), on top of the normal paste-buffer set.
func TestCopyCommand(t *testing.T) {
	out := filepath.Join(t.TempDir(), "piped.txt")
	c := harness.Start(t)
	promptReady(c)
	c.Run("run", "default", "set-option", "copy-command", "cat > "+out)

	c.TypeLine("echo PIPEME")
	c.WaitForText("PIPEME")

	c.Prefix("[")
	c.WaitForStatus("q quit")
	c.Type("/")
	c.TypeLine("PIPEME")
	c.WaitForText("PIPEME")
	c.Type("v") // select the PIPEME token
	c.Type("$")
	c.Type("y") // yank -> pipes through copy-command

	// The pipe runs async off the session actor; poll the file.
	var got []byte
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(out); err == nil && strings.Contains(string(b), "PIPEME") {
			got = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(got), "PIPEME") {
		t.Fatalf("copy-command did not receive the selection; file=%q", string(got))
	}
}

// TestUpdateEnvironment: the update-environment list is configurable, and a
// reattaching client refreshes the listed vars into the session env so panes
// spawned afterward inherit them. The var is set only in the test process (not
// the server's own env), so the peer's attach is the only path into a pane.
func TestUpdateEnvironment(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)

	if opts := c.Run("run", "default", "show-options"); !strings.Contains(opts, "update-environment DISPLAY") {
		t.Fatalf("default update-environment list missing from show-options:\n%s", opts)
	}

	c.Run("run", "default", "set-option", "update-environment", "GTMUX_UENV_TEST")
	os.Setenv("GTMUX_UENV_TEST", "reattached")
	defer os.Unsetenv("GTMUX_UENV_TEST")

	peer := c.NewPeer(80, 24) // its env carries GTMUX_UENV_TEST -> refreshed into the session
	// Wait for the peer to render: its attachEvent (and the env refresh) is then
	// fully processed before the new-window action is handled.
	peer.WaitFor(func(s *harness.Screen) bool { return s.Row(0).String() != "" })

	c.Run("run", "default", "new-window", "printf 'V=%s\\n' \"$GTMUX_UENV_TEST\"; sleep 30")
	c.WaitForText("V=reattached")
}

// TestFocusEvents: with focus-events on, a pane that requested focus reporting
// (DECSET 1004) receives ESC[O when it loses the active-pane focus and ESC[I
// when it regains it. The pane captures its raw input to a file; we switch away
// (new window) and back and assert the focus-in escape arrived.
func TestFocusEvents(t *testing.T) {
	out := filepath.Join(t.TempDir(), "focus.out")
	c := harness.Start(t)
	promptReady(c)
	c.Run("run", "default", "set-option", "focus-events", "on")

	// Non-canonical + no echo so cat streams each byte to the file without a
	// newline and the focus escapes aren't echoed back into the grid. READY
	// prints only after DECSET 1004 is processed, so we know reporting is on.
	c.TypeLine("stty -icanon -echo min 1 time 0; printf '\\033[?1004hREADY\\n'; cat > " + out)
	c.WaitForText("READY")

	c.Run("run", "default", "new-window")          // switch away -> ESC[O
	c.Run("run", "default", "select-window", "-t", "1") // switch back -> ESC[I

	var got []byte
	for i := 0; i < 50; i++ {
		if b, err := os.ReadFile(out); err == nil && strings.Contains(string(b), "\x1b[I") {
			got = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(string(got), "\x1b[I") {
		t.Fatalf("focus-in escape not delivered; got %q", string(got))
	}
	if !strings.Contains(string(got), "\x1b[O") {
		t.Fatalf("focus-out escape not delivered; got %q", string(got))
	}

	// Also exercise the select-pane funnel (the window-switch funnel is above):
	// split off a second pane, then select-pane away and back. The captured
	// pane must see another focus-out/in pair, so the escape count grows.
	before := strings.Count(string(got), "\x1b[")
	c.Run("run", "default", "split-window")            // focus moves to the new pane
	c.Run("run", "default", "select-pane", "-t", ".1") // back to the captured pane (offset .1) -> ESC[I
	for i := 0; i < 50; i++ {
		b, _ := os.ReadFile(out)
		if strings.Count(string(b), "\x1b[") > before {
			got = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if strings.Count(string(got), "\x1b[") <= before {
		t.Fatalf("select-pane did not deliver a focus escape; got %q", string(got))
	}
}

// TestSetOptionUnset: set-option -u removes an override so it falls back to the
// default. A @foo user option is set, read back via display-message, unset, and
// then reads empty again.
func TestSetOptionUnset(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)

	c.Run("run", "default", "set-option", "@greet", "howdy")
	if got := c.Run("run", "default", "display-message", "-p", "#{@greet}"); !strings.Contains(got, "howdy") {
		t.Fatalf("@greet not set; got %q", got)
	}
	c.Run("run", "default", "set-option", "-u", "@greet")
	if got := c.Run("run", "default", "display-message", "-p", "[#{@greet}]"); !strings.Contains(got, "[]") {
		t.Fatalf("@greet still set after -u; got %q", got)
	}
}

// TestExitEmpty: with exit-empty on (default), the daemon exits once its last
// session closes. `list` dials the server socket, so it errors once the daemon
// is gone (unlike has-session, which errors for an empty-but-alive server too).
func TestExitEmpty(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	if err := c.RunErr("list"); err != nil {
		t.Fatalf("server should be reachable with a session: %v", err)
	}
	c.Run("run", "default", "kill-session") // the only session -> daemon exits

	alive := true
	for i := 0; i < 50; i++ {
		if c.RunErr("list") != nil {
			alive = false
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alive {
		t.Fatal("server still reachable after last session closed (exit-empty)")
	}
}

// TestDefaultCommand: with default-command set, a pane spawned without an
// explicit command runs it (via the shell) instead of a login shell.
func TestDefaultCommand(t *testing.T) {
	lua := `gtmux.set_option("default_command", "echo DEFCMDOK; sleep 30")`
	c := harness.StartWithConfig(t, "", lua)
	c.WaitForText("DEFCMDOK")
}

// TestActivityActionNone: activity-action "none" suppresses the monitor-activity
// alert that the default "other" would raise. Same setup as TestMonitorActivity
// (which proves the flag DOES get set by default), but with action none the flag
// must stay clear even though window 1 produced output (confirmed by switching
// back and seeing its marker).
func TestActivityActionNone(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	trig := filepath.Join(t.TempDir(), "trigger")
	c.Run("run", "default", "set-option", "-g", "monitor-activity", "on")
	c.Run("run", "default", "set-option", "-g", "activity-action", "none")
	c.TypeLine("while [ ! -f " + trig + " ]; do sleep 0.1; done; echo DONEWIN1")
	c.WaitForText("DONEWIN1")
	c.Prefix("c")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
	c.Run("run", "default", "run-shell", "touch "+trig) // unblock window 1's output

	time.Sleep(1500 * time.Millisecond) // give the output time to land
	out := c.Run("run", "default", "list-windows", "-F", "#{window_index}:#{window_activity_flag}")
	if strings.Contains(out, "1:1") {
		t.Fatalf("activity-action none should suppress the alert; got %q", out)
	}
	// Prove the output actually happened (else the assertion above is vacuous).
	c.Prefix("p")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
	c.WaitForText("DONEWIN1")
}

// TestRenameWindow covers the client-owned rename-window prompt (prefix+,):
// typing a name and committing shows it in the status window list.
func TestRenameWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix(",")
	c.WaitForStatus("(rename-window") // prompt open before typing
	// Prefilled with the current name; clear it, then type a new one.
	for i := 0; i < 20; i++ {
		c.Key(0x7f) // backspace
	}
	c.TypeLine("mywin")
	c.WaitForStatus("1:mywin")
}

// TestCommandPromptMultiPrompt covers command-prompt -p "a,b": the overlay asks
// once per comma-separated label, and %1/%2 in the template take the answers in
// order. Bound via gtmux.command_prompt with a comma-joined prompt string.
func TestCommandPromptMultiPrompt(t *testing.T) {
	lua := `gtmux.bind("m", function() gtmux.command_prompt("first,second", "", "rename-window %1%2") end)`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:")
	c.Prefix("m")
	c.WaitForStatus("(first") // first stage label
	c.TypeLine("alpha")
	c.WaitForStatus("(second") // advanced to the second prompt
	c.TypeLine("beta")
	c.WaitForStatus("1:alphabeta") // template ran with both answers substituted
}

// TestRenameSession covers the client-owned rename-session prompt (prefix+$).
func TestRenameSession(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Prefix("$")
	c.WaitForStatus("(rename-session")
	for i := 0; i < 20; i++ {
		c.Key(0x7f)
	}
	c.TypeLine("renamed")
	c.WaitForStatus("renamed")
}

// TestChooseWindowDefault covers the choose-window picker and the bare-Enter
// default fix: it selects the top window (1), not the current one (3).
func TestChooseWindowDefault(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.Prefix("c")
	c.WaitForStatus("3:")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 3 })

	c.Prefix("W") // choose-window moved to W (prefix+w is now choose-tree)
	c.WaitForText("choose window")
	c.Key('\r') // bare Enter -> default is window 1, not current (3)
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
}

// TestChooseTree covers choose-tree: a cross-session tree, type-to-filter, and
// a window-granular switch (selecting a window in another session switches the
// client there AND focuses that window).
func TestChooseTree(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")
	// work gets two windows: WTARGET (index 1) then WOTHER (index 2, now the
	// active one). Selecting WTARGET in the tree must land the client on window
	// 1, NOT work's active window 2 — proving the focus-before-handoff plumbing.
	c.Run("run", "work", "rename-window", "WTARGET")
	c.Run("run", "work", "new-window")
	c.Run("run", "work", "rename-window", "WOTHER")

	c.Prefix("w")
	c.WaitForText("choose tree")
	c.WaitForText("WTARGET") // work's non-active window shows in the tree
	c.Type("WTARGET")        // filter down to just that window row
	c.Key('\r')

	c.WaitForStatus("work") // client switched to the work session
	// Landed on window 1 (WTARGET), not the active window 2 (WOTHER).
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
}

// TestChooseSessionSwitch covers the choose-session picker actually switching
// the client to another session.
func TestChooseSessionSwitch(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")

	c.Prefix("s")
	c.WaitForText("choose session")
	c.Type("j") // sorted [default, work]; move to work
	c.Key('\r')
	c.WaitForStatus("work") // c re-attached to the work session
}

// TestResizePane covers prefix+Ctrl-arrow resize by verifying the divider
// column moves.
func TestResizePane(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // vertical split
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	before := c.Screen().Col(3, '│')

	c.PrefixCtrlArrow("left")
	c.WaitFor(func(s *harness.Screen) bool {
		d := s.Col(3, '│')
		return d >= 0 && d != before
	})
}

// TestRunShellClears is the regression for the fixed bug: a run-shell status
// message must auto-clear after ~3s instead of sticking forever.
func TestRunShellClears(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	c.Prefix(":")
	c.WaitForStatus("(:") // command prompt open (client-owned) before typing
	c.TypeLine("run-shell echo HELLO")
	c.WaitForStatus("HELLO") // message shown
	c.WaitForUntil(5*time.Second, func(s *harness.Screen) bool {
		return !s.Status().Has("HELLO") // and then cleared
	})
}

// TestCLIRunAndList covers the scripting subcommands against the daemon.
func TestCLIRunAndList(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "new-window")
	c.WaitForStatus("2:")

	if list := c.Run("list"); !strings.Contains(list, "default: 2 windows") {
		t.Fatalf("list = %q, want to contain 'default: 2 windows'", list)
	}
}

// TestMouseFocus covers click-to-focus: clicking the left pane and typing lands
// the text in the left pane (left of the divider).
func TestMouseFocus(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // split -> left|right, active = right
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	div := c.Screen().Col(3, '│')

	c.Click(2, 3) // click into the left pane
	c.TypeLine("echo LMARK")
	c.WaitForText("LMARK")

	s := c.Screen()
	leftOfDivider := false
	for y := 0; y < 24; y++ {
		if i := strings.Index(string(s.Row(y)), "LMARK"); i >= 0 {
			leftOfDivider = i < div
			break
		}
	}
	if !leftOfDivider {
		t.Fatalf("LMARK not in left pane (divider col %d); screen:\n%s", div, s.String())
	}
}

// TestMouseSelectWindow covers the client-resolved status-bar click: clicking
// window 1's label switches to it (a select-window action the client builds
// from its own status mirror, no server-side hit-testing).
func TestMouseSelectWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c") // create window 2, now active
	c.WaitForStatus("2:")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })

	status := string(c.Screen().Status())
	col := strings.Index(status, "1:")
	if col < 0 {
		t.Fatalf("no '1:' label in status: %q", status)
	}
	c.Click(col+1, 24) // status is the bottom row of the 80x24 client
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
}

// TestStatusShellSplit proves the #client()/#server() split end to end: a
// custom status_left runs one command locally and one on the server, and both
// outputs land in the bar. Exercises the whole wire — client extracts the
// #server() body, ships it in Attach, the server runs it and streams the
// populated ServerShell map back, and the client expands both sides.
func TestStatusShellSplit(t *testing.T) {
	lua := `gtmux.set_option("status_left", "#client(echo CLIENTOK) #server(echo SERVEROK) ")`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForText("CLIENTOK") // client-side exec + local expand
	c.WaitForText("SERVEROK") // extract -> Attach -> server run -> gob map -> lookup
}

// TestSelectWindowByIndex covers the select_window primitive: prefix then a
// digit jumps straight to that window.
func TestSelectWindowByIndex(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
	c.Prefix("1") // jump to window 1 by number
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
}

// TestRepeatResize covers bind_repeat (tmux -r): one prefix+h then bare h's
// keep resizing, so the divider moves further than a single press would.
func TestRepeatResize(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // vertical split -> divider
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	before := c.Screen().Col(3, '│')

	// resize -L 2 opening the repeat window, plus two bare repeats — one
	// write, so the burst stays within repeat-time even under -slowmo.
	c.Prefix("hhh")
	c.WaitFor(func(s *harness.Screen) bool {
		d := s.Col(3, '│')
		return d >= 0 && (d-before >= 4 || before-d >= 4) // >2 => repeated
	})
}

// TestVimNavSwitchesPane covers no-prefix vim-aware nav over a non-vim pane:
// bare C-h moves to the left pane (a shell), where typed text then lands.
func TestVimNavSwitchesPane(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // left|right, active = right
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	div := c.Screen().Col(3, '│')

	c.Ctrl('h') // no-prefix vim-nav left; pane runs zsh, not vim -> switch
	c.TypeLine("echo LMARK")
	c.WaitForText("LMARK")

	s := c.Screen()
	for y := 0; y < 24; y++ {
		if i := strings.Index(string(s.Row(y)), "LMARK"); i >= 0 {
			if i >= div {
				t.Fatalf("LMARK at col %d, not left of divider %d", i, div)
			}
			return
		}
	}
	t.Fatalf("LMARK not found; screen:\n%s", s.String())
}

// TestDisplayMessage covers the display-message info command: it expands a
// #{...} format server-side and returns the text to the CLI.
func TestDisplayMessage(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	if out := c.Run("run", "default", "display-message", "-p", "#{session}"); !strings.Contains(out, "default") {
		t.Fatalf("display-message = %q, want to contain 'default'", out)
	}
}

// TestRemainOnExit: with remain-on-exit on, a pane whose process exits stays
// frozen in the layout (window not destroyed) and respawn-pane revives it.
func TestRemainOnExit(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-option", "remain-on-exit", "on")

	c.TypeLine("exit")         // exit the only pane's shell
	c.WaitForText("pane dead") // kept frozen, window survives

	c.Run("run", "default", "respawn-pane")
	c.WaitFor(func(s *harness.Screen) bool { return !strings.Contains(s.String(), "pane dead") })
	c.TypeLine("echo ALIVE") // the revived shell responds
	c.WaitForText("ALIVE")
}

// TestRemainOnExitFailed: with remain-on-exit "failed", only a non-zero exit
// keeps the pane. Split first so the death doesn't end the session.
func TestRemainOnExitFailed(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("%") // split; the new pane is active
	c.Run("run", "default", "set-option", "remain-on-exit", "failed")
	c.TypeLine("exit 5") // non-zero -> kept, with the code shown
	c.WaitForText("pane dead: exit 5")
}

// TestRemainOnExitFailedClean: with remain-on-exit "failed", a clean (exit 0)
// process closes the pane normally — the reap-then-close fall-through path.
func TestRemainOnExitFailedClean(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("%") // split; the new pane is active, 2 panes total
	c.WaitFor(func(*harness.Screen) bool {
		return strings.Count(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n") == 1
	})
	c.Run("run", "default", "set-option", "remain-on-exit", "failed")
	c.TypeLine("exit 0") // clean exit -> not kept: pane closes, no dead marker
	c.WaitFor(func(s *harness.Screen) bool {
		panes := strings.Count(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n")
		return panes == 0 && !strings.Contains(s.String(), "pane dead")
	})
}

// TestSynchronizePanes covers synchronize-panes: with the option on, one line
// typed at the client reaches every pane in the window, so both panes run it.
// Proven by capturing each pane and requiring the command's output marker.
func TestSynchronizePanes(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // two panes side by side
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })

	var ids []string
	for _, ln := range strings.Split(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n") {
		ids = append(ids, paneID(ln))
	}
	if len(ids) != 2 {
		t.Fatalf("want 2 panes, got %v", ids)
	}

	c.Run("run", "default", "set-option", "synchronize-panes", "on")
	c.TypeLine("echo SYNCMARK")

	// Both panes must show the marker; without the fan-out only the active one would.
	for _, id := range ids {
		id := id
		c.WaitForUntil(4*time.Second, func(s *harness.Screen) bool {
			return strings.Contains(c.Run("run", "default", "capture-pane", "-p", "-t", id), "SYNCMARK")
		})
	}
}

// TestRuntimeBindKey covers runtime bind-key / unbind-key / list-keys: a bind
// set live is pushed to the attached client and fires; list-keys reports it;
// unbind-key removes it.
func TestRuntimeBindKey(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")

	c.Run("run", "default", "bind-key", "x", "new-window")
	if out := c.Run("run", "default", "list-keys"); !strings.Contains(out, "bind-key -T prefix x new-window") {
		t.Fatalf("list-keys missing the runtime bind, got %q", out)
	}
	// A later status change is an ordering barrier: the bind was pushed to the
	// client before it (in-order on the same stream), so once the marker shows,
	// the client has applied the bind. Then prefix+x must fire new-window.
	c.Run("run", "default", "rename-window", "BINDMARK")
	c.WaitForStatus("BINDMARK")
	c.Prefix("x")
	c.WaitForStatus("2:")

	// unbind-key removes the runtime bind (server record gone).
	c.Run("run", "default", "unbind-key", "x")
	if out := c.Run("run", "default", "list-keys"); strings.Contains(out, "new-window") {
		t.Errorf("list-keys still shows the unbound key: %q", out)
	}
}

// TestSwitchClient covers switch-client -n / -l: from the client's own binding,
// -n retargets it to the next session in the sorted list, and -l returns it to
// the one it came from (registry last-session tracking).
func TestSwitchClient(t *testing.T) {
	lua := `
gtmux.bind("N", function() gtmux.switch_client("-n") end)
gtmux.bind("L", function() gtmux.switch_client("-l") end)
`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("default")
	peer := c.AttachSession("work") // sorted sessions: [default, work]
	peer.WaitForStatus("work")

	c.Prefix("N")           // -n from default -> work
	c.WaitForStatus("work") // c re-attached to the next session
	c.Prefix("L")           // -l back to where it came from
	c.WaitForStatus("default")
}

// TestPreviousLayout covers previous-layout: it steps backward through the
// preset cycle. From even-vertical (panes stacked, distinct tops) the previous
// preset wraps to even-horizontal (panes side by side, equal tops).
func TestPreviousLayout(t *testing.T) {
	paneTops := func(c *harness.Client) []string {
		return strings.Fields(strings.TrimSpace(c.Run("run", "default", "list-panes", "-F", "#{pane_top}")))
	}
	allEqual := func(xs []string) bool {
		for _, x := range xs {
			if x != xs[0] {
				return false
			}
		}
		return true
	}

	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // two panes
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })

	c.Run("run", "default", "select-layout", "even-vertical") // stacked -> tops differ
	c.WaitForUntil(4*time.Second, func(s *harness.Screen) bool {
		tops := paneTops(c)
		return len(tops) == 2 && !allEqual(tops)
	})
	c.Run("run", "default", "previous-layout") // wraps to even-horizontal -> tops equal
	c.WaitForUntil(4*time.Second, func(s *harness.Screen) bool {
		tops := paneTops(c)
		return len(tops) == 2 && allEqual(tops)
	})
}

// TestClearHistory covers clear-history: after generating scrollback, an early
// line is retrievable via capture-pane -S - (all history); clear-history drops
// the scrollback so that line is gone, while the visible screen is unaffected.
func TestClearHistory(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.TypeLine("for i in $(seq 1 60); do printf 'HL%04d\\n' $i; done")
	c.WaitForText("HL0060")

	// HL0001 scrolled off-screen but lives in scrollback.
	c.WaitForUntil(4*time.Second, func(s *harness.Screen) bool {
		return strings.Contains(c.Run("run", "default", "capture-pane", "-p", "-S", "-"), "HL0001")
	})
	// Clear it: the full capture no longer has the scrolled-off line.
	c.Run("run", "default", "clear-history")
	c.WaitForUntil(4*time.Second, func(s *harness.Screen) bool {
		return !strings.Contains(c.Run("run", "default", "capture-pane", "-p", "-S", "-"), "HL0001")
	})
}

// TestJoinPane covers join-pane -s/-t: a pane is moved out of window 2 into
// window 1 (side by side via -h). Window 2 held only that pane, so it collapses;
// window 1 ends with two panes and the moved pane is active and still live.
func TestJoinPane(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("c") // window 2 with its own pane; now the current window
	c.WaitForStatus("2:")
	promptReady(c)

	c.Run("run", "default", "join-pane", "-h", "-s", ":2", "-t", ":1")

	// Window 2 is gone (its only pane moved out); one window remains, two panes.
	c.WaitForUntil(4*time.Second, func(s *harness.Screen) bool {
		wins := strings.Fields(strings.TrimSpace(c.Run("run", "default", "list-windows", "-F", "#{window_index}")))
		return len(wins) == 1
	})
	if panes := strings.Split(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n"); len(panes) != 2 {
		t.Fatalf("want 2 panes after join, got %d: %q", len(panes), panes)
	}
	// The moved pane became active and its shell survived the move.
	c.TypeLine("echo MOVEDOK")
	c.WaitForText("MOVEDOK")
}

// TestListPanes covers list-panes: after a split it reports both panes, one
// per line, with the active flag on exactly one.
func TestListPanes(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // two panes now
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })

	out := c.Run("run", "default", "list-panes")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("list-panes = %q, want 2 lines", out)
	}
	if strings.Count(out, "(active)") != 1 {
		t.Fatalf("list-panes = %q, want exactly one (active)", out)
	}
}

// TestSelectPaneTarget covers `-t` targeting: select-pane -t :1 focuses window
// 1 regardless of which window is currently active.
// paneID pulls the "%N" token out of a list-panes line.
func paneID(line string) string {
	i := strings.IndexByte(line, '%')
	if i < 0 {
		return ""
	}
	j := i + 1
	for j < len(line) && line[j] >= '0' && line[j] <= '9' {
		j++
	}
	return line[i:j]
}

// TestSelectPaneTargetByID exercises the %N pane-id resolution path (the form
// vim-aware tools actually use) plus per-pane var lookup: the inactive pane's
// pane_active must be empty, the active pane's must be "1".
func TestSelectPaneTargetByID(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // two panes
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })

	var active, inactive string
	for _, ln := range strings.Split(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n") {
		if strings.Contains(ln, "(active)") {
			active = paneID(ln)
		} else {
			inactive = paneID(ln)
		}
	}
	if active == "" || inactive == "" {
		t.Fatalf("could not extract pane ids: active=%q inactive=%q", active, inactive)
	}
	if got := strings.TrimSpace(c.Run("run", "default", "display-message", "-t", inactive, "-p", "#{pane_active}")); got != "" {
		t.Fatalf("inactive %s pane_active = %q, want empty (-t %%N did not resolve)", inactive, got)
	}
	if got := strings.TrimSpace(c.Run("run", "default", "display-message", "-t", active, "-p", "#{pane_active}")); got != "1" {
		t.Fatalf("active %s pane_active = %q, want \"1\"", active, got)
	}
}

func TestSelectPaneTarget(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })

	c.Run("run", "default", "select-pane", "-t", ":1")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
}

// TestBreakPane covers break-pane: splitting then breaking the active pane into
// its own window.
func TestBreakPane(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("%") // 2 panes, still 1 window
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	c.Prefix("!") // break active pane out -> 2 windows
	c.WaitForStatus("2:")
}

// TestCapturePane covers capture-pane: the visible screen by default, and
// scrollback via -S -.
func TestCapturePane(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.TypeLine("echo CAPMARK")
	c.WaitForText("CAPMARK")

	if out := c.Run("run", "default", "capture-pane"); !strings.Contains(out, "CAPMARK") {
		t.Fatalf("capture-pane missing marker; got:\n%s", out)
	}

	// Push the marker into scrollback, then -S - must still find it.
	c.TypeLine("for i in $(seq 1 100); do echo crow$i; done")
	c.WaitForText("crow100")
	if out := c.Run("run", "default", "capture-pane", "-S", "-"); !strings.Contains(out, "CAPMARK") {
		t.Fatalf("capture-pane -S - missing scrollback marker; got last 200 bytes:\n%s", out[max(0, len(out)-200):])
	}
}

// TestLastWindow covers last-window: after visiting 1 -> 2 -> 3, last-window
// returns to 2, and again to 3 (the pair swaps).
func TestLastWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
	c.Prefix("c")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 3 })

	c.Run("run", "default", "last-window")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
	c.Run("run", "default", "last-window")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 3 })
}

// paneWH pulls the WxH pair out of a list-panes line's "[WxH]" field.
func paneWH(line string) (w, h int) {
	i := strings.IndexByte(line, '[')
	j := strings.IndexByte(line, ']')
	if i < 0 || j < i {
		return 0, 0
	}
	fmt.Sscanf(line[i+1:j], "%dx%d", &w, &h)
	return w, h
}

// paneDims returns every pane's WxH from list-panes.
func paneDims(c *harness.Client) (ws, hs []int) {
	for _, ln := range strings.Split(strings.TrimSpace(c.Run("run", "default", "list-panes")), "\n") {
		if w, h := paneWH(ln); w > 0 {
			ws, hs = append(ws, w), append(hs, h)
		}
	}
	return ws, hs
}

func allEqual(xs []int) bool {
	for _, x := range xs {
		if x != xs[0] {
			return false
		}
	}
	return len(xs) > 0
}

// TestSelectLayout covers select-layout: even-horizontal puts panes side by
// side (equal heights, differing left edges), even-vertical stacks them
// (equal widths).
func TestSelectLayout(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // 2 panes
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	c.Prefix("%") // 3 panes
	c.WaitFor(func(s *harness.Screen) bool { return len(paneDimsWS(c)) == 3 })

	// even-horizontal: side by side -> equal heights, each narrower than full.
	c.Run("run", "default", "select-layout", "even-horizontal")
	ws, hs := paneDims(c)
	if len(hs) != 3 || !allEqual(hs) || ws[0] >= 80 {
		t.Fatalf("even-horizontal: want equal heights + split widths; got w=%v h=%v", ws, hs)
	}

	// even-vertical: stacked -> equal full widths, each shorter than full.
	c.Run("run", "default", "select-layout", "even-vertical")
	ws, hs = paneDims(c)
	if len(ws) != 3 || !allEqual(ws) || ws[0] != 80 || hs[0] >= 23 {
		t.Fatalf("even-vertical: want equal full widths + split heights; got w=%v h=%v", ws, hs)
	}
}

func paneDimsWS(c *harness.Client) []int { ws, _ := paneDims(c); return ws }

// mainVerticalWidth returns the width of the main-vertical main pane: the one
// at the left edge (pane_left == 0).
func mainVerticalWidth(c *harness.Client) int {
	for _, ln := range strings.Split(strings.TrimSpace(c.Run("run", "default", "list-panes",
		"-F", "#{pane_left} #{pane_width}")), "\n") {
		var left, w int
		if _, err := fmt.Sscanf(ln, "%d %d", &left, &w); err == nil && left == 0 {
			return w
		}
	}
	return -1
}

// TestSetOptionMainPaneWidth covers step-3 server-option routing: set-option
// main_pane_width shrinks the main pane on the next main-vertical layout.
func TestSetOptionMainPaneWidth(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return len(paneDimsWS(c)) == 2 })
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return len(paneDimsWS(c)) == 3 })

	c.Run("run", "default", "select-layout", "main-vertical")
	before := mainVerticalWidth(c) // default main_pane_width=80 -> main ~ full width

	c.Run("run", "default", "set-option", "main_pane_width", "30")
	c.Run("run", "default", "select-layout", "main-vertical")
	after := mainVerticalWidth(c)

	if after >= before || after > 32 {
		t.Fatalf("main_pane_width=30 not honored: main width %d -> %d", before, after)
	}
}

// TestTargetSyntaxWithinSession covers the extended -t grammar: window by
// index/name/relative and pane by index within a window.pane target.
func TestTargetSyntaxWithinSession(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.Prefix("c")
	c.WaitForStatus("3:") // active window is now 3

	dm := func(tgt, f string) string {
		return strings.TrimSpace(c.Run("run", "default", "display-message", "-t", tgt, "-p", f))
	}
	cases := []struct{ tgt, want string }{
		{":1", "1"},       // absolute index
		{":{start}", "1"}, // first window
		{":{end}", "3"},   // last window
		{":-", "2"},       // relative: previous from 3
		{":+", "1"},       // relative: next from 3 wraps to 1
	}
	for _, tc := range cases {
		if got := dm(tc.tgt, "#{window_index}"); got != tc.want {
			t.Errorf("-t %s window_index = %q, want %q", tc.tgt, got, tc.want)
		}
	}

	// window by name
	c.Prefix(",")
	c.WaitForStatus("(rename-window")
	for i := 0; i < 20; i++ {
		c.Key(0x7f)
	}
	c.TypeLine("named")
	c.WaitForStatus("3:named")
	if got := dm("named", "#{window_index}"); got != "3" {
		t.Errorf("-t named window_index = %q, want 3", got)
	}

	// pane targets in window 3: split -> 2 panes, pane 2 active
	c.Prefix("%")
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	if got := dm("3.2", "#{pane_active}"); got != "1" {
		t.Errorf("-t 3.2 pane_active = %q, want 1 (active)", got)
	}
	if got := dm("3.1", "#{pane_active}"); got != "" {
		t.Errorf("-t 3.1 pane_active = %q, want empty (inactive)", got)
	}
}

// TestTargetCrossSession covers cross-session routing: a `-t sess:...` target
// runs the command on that session's goroutine.
func TestTargetCrossSession(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")

	// Read: from the "default" client, address the "work" session.
	got := strings.TrimSpace(c.Run("run", "default", "display-message", "-t", "work:1", "-p", "#{session}"))
	if got != "work" {
		t.Fatalf("cross-session -t work:1 #{session} = %q, want work", got)
	}

	// Mutation: give work a 2nd window (active), then switch it back to window 1
	// from default's client via a cross-session select-pane, and confirm it took.
	peer.Run("run", "work", "new-window")
	peer.WaitForStatus("2:")
	c.Run("run", "default", "select-pane", "-t", "work:1")
	peer.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 1 })
}

// TestRespawnPane restarts a pane's process with a new command; its output must
// render, and the session must survive the old process's death (the stale PTY
// reader from before the respawn is dropped, not applied as a teardown).
func TestRespawnPane(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "respawn-pane", "echo RESPAWNEDOK; sleep 3")
	c.WaitForText("RESPAWNEDOK")
	// Session still alive: status bar still shows the window.
	c.WaitForStatus("1:")
}

// TestSetEnvironment: a var set via set-environment is present in panes spawned
// afterward.
func TestSetEnvironment(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-environment", "GTMUXTEST", "envok123")
	c.Prefix("c") // new window inherits the session env
	c.WaitForStatus("2:")
	c.TypeLine("echo E=$GTMUXTEST")
	c.WaitForText("E=envok123")
}

// TestSetEnvironmentGlobal: set-environment -g reaches a new window's pane and
// shows under show-environment -g.
func TestSetEnvironmentGlobal(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-environment", "-g", "GTMUXGLOBAL", "globalok")
	if s := c.Run("run", "default", "show-environment", "-g"); !strings.Contains(s, "GTMUXGLOBAL=globalok") {
		t.Errorf("show-environment -g = %q, want GTMUXGLOBAL=globalok", s)
	}
	c.Prefix("c") // new window's pane inherits the global env
	c.WaitForStatus("2:")
	c.TypeLine("echo G=$GTMUXGLOBAL")
	c.WaitForText("G=globalok")
}

// TestLoadBuffer: load-buffer reads a file's contents into a named buffer,
// retrievable via show-buffer (the inverse of save-buffer).
func TestLoadBuffer(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	path := filepath.Join(t.TempDir(), "buf.txt")
	if err := os.WriteFile(path, []byte("LOADEDCONTENT"), 0644); err != nil {
		t.Fatal(err)
	}
	c.Run("run", "default", "load-buffer", "-b", "lb", path)
	if s := strings.TrimSpace(c.Run("run", "default", "show-buffer", "-b", "lb")); s != "LOADEDCONTENT" {
		t.Errorf("loaded buffer = %q, want LOADEDCONTENT", s)
	}
}

// TestAttachReadOnly: an attach -r client renders but its keystrokes never
// reach the pane; a normal peer's input still does.
func TestAttachReadOnly(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	ro := c.NewPeerReadOnly(80, 24)
	ro.WaitForStatus("1:") // read-only client still receives renders

	ro.TypeLine("echo FROMRO") // must be dropped server-side
	c.TypeLine("echo FROMRW")  // read-write input reaches the shell
	c.WaitForText("FROMRW")    // once this shows, earlier RO input would have too

	if s := c.Screen().String(); strings.Contains(s, "FROMRO") {
		t.Errorf("read-only client's input reached the pane:\n%s", s)
	}
}

// TestShowMessages: a transient status message (here run-shell output) is
// recorded in the per-session log that show-messages prints.
func TestShowMessages(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "run-shell", "echo LOGGEDMSG")
	c.WaitForStatus("LOGGEDMSG") // shown => logged
	if out := c.Run("run", "default", "show-messages"); !strings.Contains(out, "LOGGEDMSG") {
		t.Errorf("show-messages = %q, want it to contain LOGGEDMSG", out)
	}
}

// TestSetBufferAppend: set-buffer -a concatenates onto the existing buffer.
func TestSetBufferAppend(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-buffer", "-b", "ab", "foo")
	c.Run("run", "default", "set-buffer", "-a", "-b", "ab", "bar")
	if s := strings.TrimSpace(c.Run("run", "default", "show-buffer", "-b", "ab")); s != "foobar" {
		t.Errorf("appended buffer = %q, want foobar", s)
	}
}

// TestShowOptions: a server option round-trips through set-option/show-options,
// accepting tmux's hyphenated names.
func TestShowOptions(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-option", "main-pane-width", "123")
	if s := strings.TrimSpace(c.Run("run", "default", "show-options", "-v", "main-pane-width")); s != "123" {
		t.Errorf("show-options -v main-pane-width = %q, want 123", s)
	}
	// history-limit is config-time only (shipped lua sets 5000) but must still be
	// introspectable; display-time is the shipped 3000 and runtime-settable.
	if s := strings.TrimSpace(c.Run("run", "default", "show-options", "-v", "history-limit")); s != "5000" {
		t.Errorf("show-options -v history-limit = %q, want 5000", s)
	}
	c.Run("run", "default", "set-option", "display-time", "1500")
	if s := strings.TrimSpace(c.Run("run", "default", "show-options", "-v", "display-time")); s != "1500" {
		t.Errorf("show-options -v display-time = %q, want 1500", s)
	}
}

// TestResizeWindow: under window-size manual, resize-window sets the grid,
// visible via #{window_width} in list-windows.
func TestResizeWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-option", "window-size", "manual")
	c.Run("run", "default", "resize-window", "-x", "50")
	if s := strings.TrimSpace(c.Run("run", "default", "list-windows", "-F", "#{window_width}")); s != "50" {
		t.Errorf("window_width after resize = %q, want 50", s)
	}
}

// TestBaseIndex: base-index/pane-base-index offset the displayed and targeted
// window/pane numbers (default_server.lua ships 1; here we override to prove the
// offset threads through vars, the status bar, and target resolution).
func TestBaseIndex(t *testing.T) {
	serverLua := "gtmux.set_option(\"base_index\", 5)\ngtmux.set_option(\"pane_base_index\", 3)"
	c := harness.StartWithConfig(t, "", serverLua)
	c.WaitForStatus("5:") // first window numbered from base-index 5
	if s := strings.TrimSpace(c.Run("run", "default", "list-windows", "-F", "#{window_index}")); s != "5" {
		t.Errorf("window_index = %q, want 5", s)
	}
	if s := strings.TrimSpace(c.Run("run", "default", "list-panes", "-F", "#{pane_index}")); s != "3" {
		t.Errorf("pane_index = %q, want 3", s)
	}
	c.Prefix("c")
	c.WaitForStatus("6:") // second window is 6
	// Target the first window by its offset display index.
	c.Run("run", "default", "select-window", "-t", ":5")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 5 })
	// Split so the active window has panes 3 and 4 (pane-base 3), then target
	// the first pane by its offset number: select-pane -t .3 must land on it.
	c.Run("run", "default", "split-window")
	c.Run("run", "default", "select-pane", "-t", ".3")
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{pane_index}")); s != "3" {
		t.Errorf("active pane_index after select-pane -t .3 = %q, want 3", s)
	}
	// display-panes overlay (prefix+q) must flash the base-adjusted numbers 3/4,
	// not the raw slice positions 1/2. Scan the pane rows (0..22) for the '4'
	// glyph — it appears only as pane 4's flashed number under the fix; the
	// status row (23) is skipped since its clock can hold stray digits.
	c.Prefix("q")
	c.WaitFor(func(s *harness.Screen) bool {
		for y := 0; y < 23; y++ {
			if s.Col(y, '4') >= 0 {
				return true
			}
		}
		return false
	})
}

// TestWindowNaming covers automatic-rename-format (the auto name follows a
// custom template) and allow-rename (an app's OSC title escape renames the
// window only when the option is on).
func TestWindowNaming(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.WaitForStatus("1:")

	// automatic-rename-format: the live auto name gains the "-auto" suffix.
	c.Run("run", "default", "set-option", "automatic-rename-format", "#{pane_command}-auto")
	c.WaitForStatus("-auto")

	// allow-rename off (default): an app OSC 2 title is ignored.
	c.TypeLine(`printf '\033]2;APPTITLE\007'`)
	c.WaitForStatus("-auto") // still the auto name, not APPTITLE
	if strings.Contains(c.Screen().Status().String(), "APPTITLE") {
		t.Fatal("allow-rename off: OSC title must not rename the window")
	}

	// allow-rename on: the same escape now renames the window.
	c.Run("run", "default", "set-option", "allow-rename", "on")
	c.TypeLine(`printf '\033]2;APPTITLE\007'`)
	c.WaitForStatus("APPTITLE")
}

// TestStatusStyling covers window-status-format + window-status-separator: the
// window list renders from the custom entry template, joined by the separator.
func TestStatusStyling(t *testing.T) {
	lua := `
gtmux.options.window_status_separator = "|"
gtmux.options.window_status_format = "w#{window_index}"
gtmux.options.window_status_current_format = "w#{window_index}"
`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("w1")
	c.Prefix("c")
	c.WaitForStatus("w1|w2") // custom template + separator
}

// TestStatusPosition covers status-position top: the bar moves to the top row,
// and a click on a window label there still selects it (the client offsets the
// mouse Y it forwards, and resolveMouse uses the recorded click spans).
func TestStatusPosition(t *testing.T) {
	lua := `gtmux.options.status_position = "top"`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitFor(func(s *harness.Screen) bool { return strings.Contains(s.Row(0).String(), "1:") })
	if strings.Contains(c.Screen().Status().String(), "1:") {
		t.Error("window list must not be on the bottom row when status-position=top")
	}
	c.Prefix("c")
	c.WaitFor(func(s *harness.Screen) bool { return strings.Contains(s.Row(0).String(), "2:") })

	// Click window 1's label on the top row; the active green marker moves to it.
	row0 := c.Screen().Row(0).String()
	col := strings.Index(row0, "1:")
	if col < 0 {
		t.Fatalf("no '1:' label on the top row: %q", row0)
	}
	c.Click(col+1, 1)
	c.WaitFor(func(s *harness.Screen) bool {
		for x := 0; x < 80; x++ {
			if g := s.Cell(0, x); g.BG == emu.Green && g.Char == '1' {
				return true
			}
		}
		return false
	})
}

// TestWindowOptionScope covers the per-window option store: a setw override on
// one window doesn't touch the other windows or the session default.
func TestWindowOptionScope(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")

	c.Run("run", "default", "setw", "-t", ":1", "automatic-rename", "off")
	if s := strings.TrimSpace(c.Run("run", "default", "show-window-options", "-t", ":1", "-v", "automatic-rename")); s != "off" {
		t.Errorf("window 1 automatic-rename = %q, want off (its override)", s)
	}
	if s := strings.TrimSpace(c.Run("run", "default", "show-window-options", "-t", ":2", "-v", "automatic-rename")); s != "on" {
		t.Errorf("window 2 automatic-rename = %q, want on (session default, untouched)", s)
	}
	if s := strings.TrimSpace(c.Run("run", "default", "show-options", "-v", "automatic-rename")); s != "on" {
		t.Errorf("session automatic-rename = %q, want on (default unchanged)", s)
	}
}

// TestFormatOperators covers the format DSL operators server-side: arithmetic
// and a comparison used inside a conditional, via display-message.
func TestFormatOperators(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{e:6*7}")); s != "42" {
		t.Errorf("#{e:6*7} = %q, want 42", s)
	}
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{?#{==:#{window_index},1},yes,no}")); s != "yes" {
		t.Errorf("#{==} in conditional = %q, want yes", s)
	}
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{m:*sh,zsh}#{a:66}")); s != "1B" {
		t.Errorf("#{m}/#{a} = %q, want 1B", s)
	}
}

// TestStatusFormatOperator covers the operators reaching the client status bar:
// a window-status-current-format using ==/conditional renders on the bar.
func TestStatusFormatOperator(t *testing.T) {
	lua := `gtmux.options.window_status_current_format = "#{?#{==:#{window_index},1},<#{window_index}>,#{window_index}}"`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("<1>") // active window 1, rendered through the operator + conditional
}

// TestUserOptionsAndAlias covers @foo user options (readable in formats) and
// command-alias (a command name resolved to its expansion at dispatch).
func TestUserOptionsAndAlias(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-option", "-g", "@greeting", "howdy")
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{@greeting}")); s != "howdy" {
		t.Errorf("#{@greeting} = %q, want howdy", s)
	}
	c.Run("run", "default", "set-option", "command-alias", "greet=display-message -p ALIASOK")
	if s := strings.TrimSpace(c.Run("run", "default", "greet")); s != "ALIASOK" {
		t.Errorf("command-alias greet = %q, want ALIASOK", s)
	}
}

// TestSendKeysDepth covers send-keys -l (literal text) and -H (hex bytes).
func TestSendKeysDepth(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Run("run", "default", "send-keys", "-l", "LITERAL")
	c.WaitForText("LITERAL")
	c.Run("run", "default", "send-keys", "-H", "20", "48", "49") // space, H, I
	c.WaitForText("HI")
}

// TestCustomKeyTable covers bind -T / switch-client -T: prefix+g switches into a
// custom table, and the next key resolves there (multi-key sequence).
func TestCustomKeyTable(t *testing.T) {
	lua := `
gtmux.bind("g", function() gtmux.key_table("demo") end)
gtmux.bind_table("demo", "n", function() gtmux.new_window() end)
`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:")
	c.Prefix("g") // enter the custom "demo" table
	c.Key('n')    // resolved from "demo" -> new-window
	c.WaitForStatus("2:")
	// The table is one-shot: a bare 'n' now types into the shell, not a bind
	// (if it re-fired the table we'd get window 3).
	promptReady(c)
	c.TypeLine("echo TABLEREVERTED")
	c.WaitForText("TABLEREVERTED")
	// Assert no third window exists (checking the window list, not the status
	// string — the clock can contain "3:" at 23:xx).
	if wins := strings.Fields(strings.TrimSpace(c.Run("run", "default", "list-windows", "-F", "#{window_index}"))); len(wins) != 2 {
		t.Errorf("custom table should be one-shot; want 2 windows, got %v", wins)
	}
}

// TestDestroyUnattached: with the option on, a session self-destroys once its
// last client detaches.
func TestDestroyUnattached(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-option", "-g", "destroy-unattached", "on")
	c.Run("run", "default", "detach")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !strings.Contains(c.Run("list"), "default") {
			return // gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("destroy-unattached: session survived its last client detaching")
}

// TestDetachOnDestroy: with the option off, killing a session hands its client to
// another session instead of detaching it.
func TestDetachOnDestroy(t *testing.T) {
	c := harness.Start(t) // session "default"
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")

	c.Run("run", "default", "set-option", "-g", "detach-on-destroy", "off")
	c.Run("kill-session", "default")
	c.WaitForStatus("work") // c switched to "work", not detached
}

// TestKillSessionCommand: kill-session as a session command (the keybind /
// command-prompt path, not the CLI kill) ends the session; with
// detach-on-destroy off the client lands in the surviving session.
func TestKillSessionCommand(t *testing.T) {
	c := harness.Start(t) // session "default"
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")

	c.Run("run", "default", "set-option", "-g", "detach-on-destroy", "off")
	c.Run("run", "default", "kill-session")
	c.WaitForStatus("work") // c switched to "work", not detached
}

// TestMonitorActivity: with monitor-activity on, output on a window the user
// isn't viewing sets that window's activity flag.
func TestMonitorActivity(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	// Deterministic: window 1 blocks until a trigger file exists, so its output
	// is caused only after we've confirmed the switch to window 2 — no timing
	// race against the (slow) new-window shell startup.
	trig := filepath.Join(t.TempDir(), "trigger")
	c.Run("run", "default", "set-option", "-g", "monitor-activity", "on")
	c.Run("run", "default", "set-option", "-g", "visual-activity", "on")
	c.TypeLine("while [ ! -f " + trig + " ]; do sleep 0.1; done; echo DONEWIN1")
	c.WaitForText("DONEWIN1") // command is in window 1 (echoed) before we switch away
	c.Prefix("c")             // create + switch to window 2
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
	c.Run("run", "default", "run-shell", "touch "+trig) // now unblock window 1

	// server-side flag via list-windows
	deadline := time.Now().Add(10 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		out := c.Run("run", "default", "list-windows", "-F", "#{window_index}:#{window_activity_flag}")
		found = strings.Contains(out, "1:1")
		if !found {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if !found {
		t.Fatal("monitor-activity: window 1 activity flag not set after background output")
	}
	// visual-activity flashes a message (which transiently replaces the window
	// list), then once it clears the flag shows as # on the non-current window.
	c.WaitForStatus("activity in window 1")
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool { return strings.Contains(s.Status().String(), "#") })
}

// TestMonitorSilence: a window with no output for the monitor-silence interval,
// while non-current, raises the silence flag (~) and a visual message.
func TestMonitorSilence(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	trig := filepath.Join(t.TempDir(), "trigger")
	c.Run("run", "default", "set-option", "-g", "monitor-silence", "2") // 2s of quiet
	c.Run("run", "default", "set-option", "-g", "visual-activity", "on")
	c.TypeLine("while [ ! -f " + trig + " ]; do sleep 0.1; done; echo SILENCEARM")
	c.WaitForText("SILENCEARM") // command is in window 1 before we leave
	c.Prefix("c")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
	// One output on the now-non-current window 1 arms its silence timer; it then
	// stays quiet, so after the interval the silence alert fires.
	c.Run("run", "default", "run-shell", "touch "+trig)
	// The silence timer needs the full 2s interval AFTER window 1's arming output
	// (its post-trigger prompt redraw), so the default 2s WaitFor can't win the
	// race — give the alert real headroom past the interval.
	c.WaitForUntil(6*time.Second, func(s *harness.Screen) bool { return s.Status().Has("silence in window 1") })
	c.WaitFor(func(s *harness.Screen) bool { return strings.Contains(s.Status().String(), "~") })
}

// TestCapturePaneDepth covers capture-pane -e (keep SGR escapes) and -J (join a
// line that wrapped at the terminal edge back into one).
func TestCapturePaneDepth(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)

	c.TypeLine("printf '\\033[31mREDTEXT\\033[0m\\n'")
	c.WaitForText("REDTEXT")
	esc := c.Run("run", "default", "capture-pane", "-e", "-p")
	if !strings.Contains(esc, "REDTEXT") || !strings.Contains(esc, "\x1b[") {
		t.Errorf("capture-pane -e must keep text + escapes; got %q", esc)
	}
	if plain := c.Run("run", "default", "capture-pane", "-p"); strings.Contains(plain, "\x1b[") {
		t.Errorf("capture-pane without -e must be plain text; got %q", plain)
	}

	long := strings.Repeat("W", 90) // wider than the 80-col terminal -> wraps
	c.TypeLine("printf '" + long + "\\n'")
	c.WaitForText("WWW")
	if joined := c.Run("run", "default", "capture-pane", "-J", "-p"); !strings.Contains(joined, long) {
		t.Errorf("capture-pane -J should rejoin the wrapped line; %q not in:\n%s", long, joined)
	}
}

// TestPaneBorderStatus covers pane-border-status/-format: turning it on reserves
// a row per pane and draws the expanded pane-border-format there.
func TestPaneBorderStatus(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Run("run", "default", "set-option", "pane-border-format", "PB#{pane_index}")
	c.Run("run", "default", "set-option", "pane-border-status", "top")
	c.WaitForText("PB1") // label row, pane_index expanded (base-index 1)
}

// TestFormatLoops covers the #{W:}/#{P:}/#{S:} format loops via display-message
// (server-side expansion, where the windows/panes/sessions live).
func TestFormatLoops(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "new-window")
	c.WaitForStatus("2:")
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{W:#{window_index}:#{window_name} }")); s != "1:zsh 2:zsh" {
		t.Errorf("#{W:} = %q, want '1:zsh 2:zsh'", s)
	}
	c.Run("run", "default", "split-window") // active window 2 now has panes 1 and 2
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{P:[#{pane_index}]}")); s != "[1][2]" {
		t.Errorf("#{P:} = %q, want '[1][2]'", s)
	}
	if s := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{S:#{session}}")); s != "default" {
		t.Errorf("#{S:} = %q, want 'default'", s)
	}
}

// TestWaitFor covers the wait-for sync primitive: signal/wait (-S wakes a bare
// waiter) and lock/unlock (-L blocks while held, -U releases it).
func TestWaitFor(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")

	// signal/wait: the waiter blocks until -S.
	woke := make(chan struct{})
	go func() { c.Run("run", "default", "wait-for", "sync1"); close(woke) }()
	time.Sleep(500 * time.Millisecond)
	select {
	case <-woke:
		t.Fatal("wait-for returned before it was signaled")
	default:
	}
	c.Run("run", "default", "wait-for", "-S", "sync1")
	select {
	case <-woke:
	case <-time.After(5 * time.Second):
		t.Fatal("wait-for did not wake on -S")
	}

	// lock/unlock: -L holds the channel; a second -L blocks until -U.
	c.Run("run", "default", "wait-for", "-L", "mutex1")
	got := make(chan struct{})
	go func() { c.Run("run", "default", "wait-for", "-L", "mutex1"); close(got) }()
	time.Sleep(500 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("second -L acquired the lock while it was held")
	default:
	}
	c.Run("run", "default", "wait-for", "-U", "mutex1")
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("second -L did not acquire after -U")
	}
}

// TestHookSessionCreated: a session-created hook (config-time) runs once the
// session is up.
func TestHookSessionCreated(t *testing.T) {
	c := harness.StartWithConfig(t, "", `gtmux.set_hook("session-created", "rename-window CREATED")`)
	c.WaitForStatus("CREATED")
}

// TestHookAfterKillWindow: the after-kill-window hook fires when a window is
// killed (the surviving window gets renamed by the hook).
func TestHookAfterKillWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.Run("run", "default", "set-hook", "after-kill-window", "rename-window", "KILLED")
	c.Run("run", "default", "kill-window") // kills window 2, back to window 1
	c.WaitForStatus("KILLED")
}

// TestHookClientCommand: a hook that fires a client-owned command (command-prompt)
// opens the overlay on the client, instead of no-opping server-side.
func TestHookClientCommand(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-hook", "after-new-window", "command-prompt")
	c.Prefix("c")         // new-window fires after-new-window -> command-prompt
	c.WaitForStatus("(:") // the command-prompt overlay opened locally
}

// TestPipePane tees a pane's output to a command; the data must reach it.
func TestPipePane(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	out := filepath.Join(t.TempDir(), "pipe.out")
	c.Run("run", "default", "pipe-pane", "cat >> "+out)
	c.TypeLine("echo PIPEDATA")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(out); err == nil && strings.Contains(string(b), "PIPEDATA") {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pipe-pane output never reached the pipe command")
}

// TestLock covers the lock overlay (and the shared clock/lock overlay
// mechanism): it shows a message and any key dismisses it.
func TestLock(t *testing.T) {
	lua := `gtmux.bind_root("L", function() gtmux.lock() end)`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:")
	c.Key('L')
	c.WaitForText("locked")
	c.Key(' ') // any key unlocks
	c.WaitFor(func(s *harness.Screen) bool { return !strings.Contains(s.String(), "locked") })
}

// TestClockMode enters clock-mode (shares the overlay mechanism with lock) and
// dismisses it; the session keeps working.
func TestClockMode(t *testing.T) {
	lua := `gtmux.bind_root("K", function() gtmux.clock_mode() end)`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:")
	c.Key('K')
	// The clock draws big block digits across the window's middle rows (mid row
	// 11 for 80x24, where digit columns show '█'). Wait for it before dismissing,
	// so the two keys are separate reads (a key that arrives in the same chunk
	// that turns the overlay on isn't gated).
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(11, '█') >= 0 })
	c.Key(' ') // dismiss
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(11, '█') < 0 })
	c.Prefix("c")
	c.WaitForStatus("2:") // still functional after the overlay
}

// TestBufferStack covers the paste-buffer stack: set/show/list/named/delete and
// paste round-tripping into the pane.
func TestBufferStack(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-buffer", "alpha")
	c.Run("run", "default", "set-buffer", "beta") // beta is now the newest
	if s := strings.TrimSpace(c.Run("run", "default", "show-buffer")); s != "beta" {
		t.Errorf("top buffer = %q, want beta", s)
	}
	lb := c.Run("run", "default", "list-buffers")
	if !strings.Contains(lb, "alpha") || !strings.Contains(lb, "beta") {
		t.Errorf("list-buffers = %q, want both alpha and beta", lb)
	}
	// Named buffer, then delete it.
	c.Run("run", "default", "set-buffer", "-b", "nb", "named")
	if s := strings.TrimSpace(c.Run("run", "default", "show-buffer", "-b", "nb")); s != "named" {
		t.Errorf("named buffer = %q, want named", s)
	}
	c.Run("run", "default", "delete-buffer", "-b", "nb")
	if s := strings.TrimSpace(c.Run("run", "default", "show-buffer", "-b", "nb")); s != "" {
		t.Errorf("deleted buffer still present: %q", s)
	}
	// paste-buffer writes the top buffer (beta) into the pane.
	c.Run("run", "default", "paste-buffer")
	c.WaitForText("beta")
}

// TestHasSession: has-session exits 0 for a live session, non-zero otherwise.
func TestHasSession(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	if err := c.RunErr("has-session", "default"); err != nil {
		t.Errorf("has-session default should succeed, got %v", err)
	}
	if err := c.RunErr("has-session", "ghost"); err == nil {
		t.Error("has-session ghost should fail")
	}
}

// TestFindWindow: find-window selects the first window matching a name pattern.
func TestFindWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:")
	c.Run("run", "default", "rename-window", "targetwin") // names window 2
	c.WaitForStatus("targetwin")
	c.Prefix("c")
	c.WaitForStatus("3:") // window 3 active now
	c.Run("run", "default", "find-window", "targetwin")
	c.WaitFor(func(s *harness.Screen) bool { return s.ActiveWindow() == 2 })
}

// TestListClients lists the clients attached to a session (one line each).
func TestListClients(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	peer := c.NewPeer(80, 24)
	peer.WaitForStatus("1:")
	out := c.Run("run", "default", "list-clients")
	if n := strings.Count(out, "client-"); n != 2 {
		t.Fatalf("list-clients = %q, want 2 client lines", out)
	}
}

// TestWindowSizeSmallest: with window_size smallest, two differently-sized
// clients share the grid at the smaller size, so the wider client dot-fills the
// slack columns.
func TestWindowSizeSmallest(t *testing.T) {
	srv := `gtmux.set_option("window_size", "smallest")`
	c := harness.StartWithConfig(t, "", srv) // this client is 80x24
	c.WaitForStatus("1:")
	peer := c.NewPeer(40, 24) // narrower peer -> grid shrinks to 40
	peer.WaitForStatus("1:")
	// On the 80-wide client, columns past 40 are now dot-fill.
	c.WaitFor(func(s *harness.Screen) bool { return s.Cell(0, 60).Char == '·' })
}

// TestDisplayPopup opens a popup (via a config bind) running a command whose
// output must render inside the floating box.
func TestDisplayPopup(t *testing.T) {
	lua := `gtmux.bind_root("P", function() gtmux.display_popup("echo POPUPOK; sleep 3") end)`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:")
	c.Key('P') // bind_root: bare key, no prefix
	c.WaitForText("POPUPOK")
}

// TestDisplayMenu opens a menu (via a config bind) and selects an item, whose
// command (new-window) must run.
func TestDisplayMenu(t *testing.T) {
	lua := `gtmux.bind_root("M", function() gtmux.display_menu("go", "new", "new-window") end)`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:")
	c.Key('M')
	c.WaitForText("go") // menu title visible
	c.Key('\r')         // select the only item -> new-window
	c.WaitForStatus("2:")
}

// TestHookFires registers an after-new-window hook, then triggers the event and
// asserts the hook's command actually ran (renames the new window).
func TestHookFires(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-hook", "after-new-window", "rename-window", "hooked")
	c.Run("run", "default", "new-window")
	c.WaitForStatus("hooked") // the hook renamed the freshly-created window
}

// TestHookAfterRenameWindow covers a newer fire point (and the per-hook
// re-entry guard: after-rename-window fires a rename that would re-trigger it,
// but the guard stops the loop and the second rename still lands).
func TestHookAfterRenameWindow(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Run("run", "default", "set-hook", "after-rename-window", "rename-window", "guarded")
	c.Run("run", "default", "rename-window", "first")
	c.WaitForStatus("guarded") // the hook renamed once; the guard stopped it looping
}

// TestIfShell covers if-shell's then/else branching. The shell runs async
// (never blocking the session goroutine) and the chosen command lands a moment
// later — WaitForStatus polls until it does.
func TestIfShell(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	// exit 0 -> then branch: new-window.
	c.Run("run", "default", "if-shell", "true", "new-window", "next-window")
	c.WaitForStatus("2:")
	// exit non-zero -> else branch: new-window (then branch is a harmless no-op).
	c.Run("run", "default", "if-shell", "false", "next-window", "new-window")
	c.WaitForStatus("3:")
}

// TestMouseWheelEntersCopyMode: a wheel-up over a non-mouse-tracking pane
// enters copy-mode (tmux scrollback behavior). The client owns the gesture now
// (recognized from WantsMouse in its Layout) and drives it via select-pane +
// copy-mode actions.
func TestMouseWheelEntersCopyMode(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.WheelUp(5, 5) // wheel up inside the pane grid
	c.WaitForText("hjkl")
}

// TestMouseBorderDrag: dragging a pane divider resizes the split. The client
// recognizes the border-drag from its own Layout.Borders and sends
// ResizeBorder to the server, which maps it to the split node's fraction.
func TestMouseBorderDrag(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.Prefix("%") // vertical split -> divider
	c.WaitFor(func(s *harness.Screen) bool { return s.Col(3, '│') >= 0 })
	before := c.Screen().Col(3, '│')

	// Grab the divider (SGR col = 0-based col + 1) and drag it ~10 cells left.
	c.Drag(before+1, 5, before-9, 5)
	c.WaitFor(func(s *harness.Screen) bool {
		d := s.Col(3, '│')
		return d >= 0 && before-d >= 8
	})
}

// TestMouseDragCopy: a left-drag across a non-tracking pane enters copy-mode
// with a selection anchored at the press cell (tmux drag-to-copy). The client
// recognizes the gesture and asks the server (which alone holds the scrollback)
// for the snapshot via CopyDrag.
func TestMouseDragCopy(t *testing.T) {
	c := harness.Start(t)
	promptReady(c)
	c.TypeLine("echo DRAGME")
	c.WaitForText("DRAGME")

	c.Drag(3, 3, 12, 3) // press, motion, release across the pane
	c.WaitForStatus("q quit")
}

// TestMultiLineStatus: `status 2` reserves a second status row, drawn from
// status-format[1] (status_format_2). Proves the full round-trip — the client
// sends its status-lines count in Attach, the server sizes the window grid
// (rows - 2) so the pane doesn't overwrite the extra line, and the client
// composites it. On an 80x24 client the main bar is row 23, the extra line 22.
func TestMultiLineStatus(t *testing.T) {
	lua := `
gtmux.options.status = "2"
gtmux.options.status_format_2 = "SECONDLINE"
`
	c := harness.StartWithConfig(t, lua, "")
	c.WaitForStatus("1:") // main bar (window list) still on the bottom row
	c.WaitFor(func(s *harness.Screen) bool {
		return strings.Contains(string(s.Row(22)), "SECONDLINE")
	})
	// The server must reserve BOTH status rows, or the pane is one row too tall
	// and its bottom line hides under the extra status line. On an 80x24 client
	// the pane gets 24 - 2 = 22 rows.
	h := strings.TrimSpace(c.Run("run", "default", "display-message", "-p", "#{pane_height}"))
	if h != "22" {
		t.Fatalf("pane_height = %q, want 22 (24 - 2 status rows)", h)
	}
}

// TestListWindows covers list-windows: one line per window, -F format, and the
// window_active flag.
func TestListWindows(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("1:")
	c.Prefix("c")
	c.WaitForStatus("2:") // window 2 active now

	out := strings.TrimSpace(c.Run("run", "default", "list-windows",
		"-F", "#{window_index}:#{window_active}:#{window_panes}"))
	got := strings.Split(out, "\n")
	want := []string{"1::1", "2:1:1"} // window 2 is active, 1 pane each
	if len(got) != len(want) {
		t.Fatalf("list-windows lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("list-windows[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestListSessions covers list-sessions across the registry, including a peer
// session queried via info() (not the caller's own goroutine).
func TestListSessions(t *testing.T) {
	c := harness.Start(t)
	c.WaitForStatus("default")
	peer := c.AttachSession("work")
	peer.WaitForStatus("work")

	out := strings.TrimSpace(c.Run("run", "default", "list-sessions",
		"-F", "#{session_name}:#{session_windows}:#{session_attached}"))
	lines := strings.Split(out, "\n")
	got := map[string]string{}
	for _, l := range lines {
		if name, _, ok := strings.Cut(l, ":"); ok {
			got[name] = l
		}
	}
	if got["default"] != "default:1:1" {
		t.Errorf("default line = %q, want default:1:1", got["default"])
	}
	if got["work"] != "work:1:1" {
		t.Errorf("work line = %q, want work:1:1", got["work"])
	}
}
