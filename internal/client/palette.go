package client

import "strings"

// paletteCmd is one command-palette entry: the command line it runs, a short
// description, and the kind of argument it completes in stage two ("" = none,
// "text" = free text, no candidate list). Kinds with candidates: session,
// window, pane, layout, option — see compositor.paletteCands.
type paletteCmd struct{ name, desc, arg string }

// paletteCmds is the curated catalog behind the command palette. Order is the
// unfiltered order and breaks ties among equal matches, so the common form of a
// command comes before its variants. Keep in sync with the server's runCommand
// (internal/server/session.go) when adding commands.
var paletteCmds = []paletteCmd{
	{"split-window", "split pane, stacked", ""},
	{"split-window -h", "split pane, side by side", ""},
	{"new-window", "open a new window", ""},
	{"new-window -n", "new window with a name", "text"},
	{"kill-pane", "close this pane", ""},
	{"kill-pane -t", "close a pane", "pane"},
	{"kill-window", "close this window", ""},
	{"kill-window -t", "close a window", "window"},
	{"kill-session -t", "kill a session", "session"},
	{"rename-window", "rename this window", "text"},
	{"rename-session", "rename this session", "text"},
	{"select-window -t", "go to a window", "window"},
	{"next-window", "next window", ""},
	{"previous-window", "previous window", ""},
	{"last-window", "last used window", ""},
	{"select-pane -t", "focus a pane", "pane"},
	{"select-pane -L", "focus pane to the left", ""},
	{"select-pane -R", "focus pane to the right", ""},
	{"select-pane -U", "focus pane above", ""},
	{"select-pane -D", "focus pane below", ""},
	{"resize-pane -Z", "zoom / unzoom pane", ""},
	{"swap-pane -U", "swap pane with previous", ""},
	{"swap-pane -D", "swap pane with next", ""},
	{"rotate-window", "rotate panes", ""},
	{"break-pane", "move pane to its own window", ""},
	{"join-pane -s", "pull a pane into this window", "pane"},
	{"mark-pane", "mark this pane", ""},
	{"join-marked", "join the marked pane here", ""},
	{"display-panes", "show pane numbers", ""},
	{"select-layout", "apply a layout", "layout"},
	{"next-layout", "cycle to next layout", ""},
	{"previous-layout", "cycle to previous layout", ""},
	{"switch-client -t", "switch to a session", "session"},
	{"switch-client -l", "last session", ""},
	{"switch-client -n", "next session", ""},
	{"switch-client -p", "previous session", ""},
	{"new-session -d -s", "create a detached session", "text"},
	{"find-session", "fuzzy session finder", ""},
	{"choose-tree", "session / window tree", ""},
	{"choose-window", "pick a window", ""},
	{"choose-buffer", "pick a paste buffer", ""},
	{"copy-mode", "scrollback / copy mode", ""},
	{"paste-buffer", "paste the latest buffer", ""},
	{"list-buffers", "list paste buffers", ""},
	{"clear-history", "clear pane scrollback", ""},
	{"respawn-pane -k", "restart the pane's program", ""},
	{"respawn-window -k", "restart the window", ""},
	{"find-window", "go to window matching text", "text"},
	{"set-option -g", "set a global option", "option"},
	{"show-options -g", "show global options", ""},
	{"list-keys", "show key bindings", ""},
	{"display-message", "show a message", "text"},
	{"run-shell", "run a shell command", "text"},
	{"display-popup", "floating terminal popup", ""},
	{"send-keys", "type keys into this pane", "text"},
	{"detach-client", "detach this client", ""},
}

// paletteLayouts and paletteOptions are the static stage-two candidates.
var (
	paletteLayouts = []string{"even-horizontal", "even-vertical", "main-horizontal", "main-vertical", "tiled"}
	// ponytail: hand-kept list of the server's options (set-option in
	// internal/server/session.go); generate from the server if it drifts.
	paletteOptions = []string{
		"activity-action", "allow-passthrough", "allow-rename", "automatic-rename",
		"automatic-rename-format", "base-index", "bell-action", "default-command",
		"default-shell", "destroy-unattached", "detach-on-destroy", "exit-empty",
		"focus-events", "history-limit", "main-pane-height", "main-pane-width",
		"monitor-activity", "monitor-bell", "monitor-silence", "pane-base-index",
		"pane-border-format", "pane-border-status", "remain-on-exit",
		"synchronize-panes", "visual-activity", "visual-bell", "window-size",
	}
)

// palette is the command-palette mode of a picker (prefix+:). Stage one lists
// paletteCmds; once the typed text is a command that takes an argument plus a
// space ("switch-client -t "), stage two lists that argument's candidates and
// the text after the space is the query. Enter runs the selection, or the typed
// line verbatim when nothing matches (so it still works as a plain command line).
type palette struct {
	cmd   *paletteCmd // stage two: the command whose argument is being picked; nil = stage one
	off   int         // where the query starts in picker.filter (0 in stage one)
	cands func(kind string) (items, targets []string)
}

// newPalettePicker opens the palette; cands supplies stage-two candidates from
// the compositor's mirrored state.
func newPalettePicker(cands func(kind string) (items, targets []string)) *picker {
	p := &picker{title: "command", verb: "run", filterable: true, palette: &palette{cands: cands}}
	p.palette.refresh(p)
	return p
}

// refresh re-derives the stage and item list from the typed text.
func (pl *palette) refresh(p *picker) {
	pl.cmd, pl.off = nil, 0
	for i := range paletteCmds {
		c := &paletteCmds[i]
		if c.arg != "" && strings.HasPrefix(p.filter, c.name+" ") && (pl.cmd == nil || len(c.name) > len(pl.cmd.name)) {
			pl.cmd = c
		}
	}
	if pl.cmd == nil {
		p.items, p.targets, p.descs = nil, nil, nil
		for _, c := range paletteCmds {
			p.items = append(p.items, c.name)
			p.targets = append(p.targets, c.name)
			p.descs = append(p.descs, c.desc)
		}
		return
	}
	pl.off = len(pl.cmd.name) + 1
	p.items, p.targets, p.descs = nil, nil, nil
	if pl.cmd.arg != "text" && pl.cands != nil {
		p.items, p.targets = pl.cands(pl.cmd.arg)
	}
}

// listTitle labels the list box: "commands", or the argument being picked.
func (pl *palette) listTitle() string {
	if pl.cmd == nil {
		return "commands"
	}
	if pl.cmd.arg == "text" {
		return pl.cmd.name
	}
	return pl.cmd.arg
}

// byName returns the catalog entry with this exact name.
func paletteByName(name string) *paletteCmd {
	for i := range paletteCmds {
		if paletteCmds[i].name == name {
			return &paletteCmds[i]
		}
	}
	return nil
}

// setFilter replaces the typed text (Tab / Enter completion) and re-derives.
func (pl *palette) setFilter(p *picker, s string) {
	p.filter, p.sel = s, 0
	pl.refresh(p)
}

// complete is Tab: copy the selection into the input — a command (plus a space
// when it takes an argument, which opens stage two), or a stage-two candidate.
func (pl *palette) complete(p *picker) {
	v := p.view()
	if p.sel < 0 || p.sel >= len(v) {
		return
	}
	t := p.targets[v[p.sel]]
	if pl.cmd != nil {
		pl.setFilter(p, pl.cmd.name+" "+t)
		return
	}
	if c := paletteByName(t); c != nil && c.arg != "" {
		t += " "
	}
	pl.setFilter(p, t)
}

// commit is Enter.
func (pl *palette) commit(p *picker) pickerResult {
	v := p.view()
	have := p.sel >= 0 && p.sel < len(v)
	if pl.cmd == nil {
		if !have {
			return pickerResult{done: true, action: tokenize(p.filter)} // run as typed (nil = cancel)
		}
		c := paletteByName(p.targets[v[p.sel]])
		if c.arg != "" {
			pl.setFilter(p, c.name+" ") // needs an argument: move to stage two
			return pickerResult{}
		}
		return pickerResult{done: true, action: tokenize(c.name)}
	}
	if have {
		t := p.targets[v[p.sel]]
		if pl.cmd.arg == "option" {
			pl.setFilter(p, pl.cmd.name+" "+t+" ") // an option needs a value: type it, Enter again
			return pickerResult{}
		}
		return pickerResult{done: true, action: append(tokenize(pl.cmd.name), t)}
	}
	if strings.TrimSpace(p.filter[pl.off:]) == "" {
		return pickerResult{} // nothing typed for the argument yet: stay open
	}
	return pickerResult{done: true, action: tokenize(p.filter)}
}

// confirmKill routes a palette kill-window / kill-session through the
// confirm-before dialog, like prefix+& / prefix+q. The label goes through -p,
// which splits on commas, so it must not contain one.
func confirmKill(act []string) []string {
	if len(act) == 0 {
		return act
	}
	what := map[string]string{"kill-window": "window", "kill-session": "session"}[act[0]]
	if what == "" {
		return act
	}
	label := "kill this " + what + "?"
	for i, a := range act {
		if a == "-t" && i+1 < len(act) {
			label = "kill " + what + " " + act[i+1] + "?"
		}
	}
	return append([]string{"confirm-before", "-p", strings.ReplaceAll(label, ",", " "), "--"}, act...)
}
