package server

import (
	"sync/atomic"

	"github.com/FyrmForge/gtmux/internal/proto"
)

// activePointSeq stamps panes as they become active (pane.activePoint), for the
// MRU tiebreak in directional nav. Server-global (like tmux's next_active_point)
// so a pane keeps a comparable stamp after moving between windows (join-pane).
// Concurrent across window-actor goroutines, hence atomic.
var activePointSeq atomic.Int64

// setActive makes p the window's active pane and stamps it, so directional nav
// can prefer the most-recently-active neighbor. THE one place w.active is set —
// mirrors tmux's window_set_active_pane. Callers that also track lastActive set
// it themselves before calling this (see session.go).
func (w *window) setActive(p *pane) {
	w.active = p
	if p != nil {
		p.activePoint = activePointSeq.Add(1)
	}
}

// window is a tiled arrangement of panes (a tmux "window"). Only one window
// per session is displayed at a time; its panes are all visible together.
type window struct {
	sessionName  string            // for GTMUX env var on spawned panes; not display state
	env          map[string]string // session environment (set-environment), injected into new panes; shared reference across the session's windows
	globalEnv    map[string]string // set-environment -g snapshot at window creation; applied under env (session overrides global)
	root         *layoutNode
	panes        []*pane
	active       *pane
	lastActive   *pane // previously-active pane, for select-pane -l
	cols         int
	rows         int
	borders      []borderSeg
	showNumbers  bool              // display-panes overlay, toggled by prefix+q
	paneBase     int               // pane-base-index snapshot for the display-panes overlay
	manualName   *string           // set by prefix+, ; overrides automatic-rename until cleared
	autoName     string            // last automatic-rename value; frozen when automatic-rename is off
	opts         map[string]string // per-window option overrides (setw), resolved over the session default
	borderStatus string            // pane-border-status: "off" | "top" | "bottom" (reserves a row per pane)
	borderFormat string            // pane-border-format template for the reserved label row
	actor        *windowActor      // the actor wrapping this window (set by newWindowActor); reached via pane.win.actor
	zoomed       bool              // prefix+z: active pane fills the window, others hidden
	layoutName   string            // last preset applied via select-layout, for next-layout cycling
	// main-pane-width/height in effect when layoutName was applied, so a window
	// resize can re-apply the named layout (keeping the main pane absolute)
	// instead of scaling frozen fractions. See resize().
	mainW, mainH int
}

func newWindow(cols, rows int, dir, command, sessionName string, env, globalEnv map[string]string) (*window, error) {
	w := &window{sessionName: sessionName, env: env, globalEnv: globalEnv, cols: cols, rows: rows}
	p, err := spawnPane(w, w.sessionName, rect{0, 0, rows, cols}, dir, command)
	if err != nil {
		return nil, err
	}
	w.root = &layoutNode{pane: p}
	w.panes = []*pane{p}
	w.setActive(p)
	return w, nil
}

// rename sets a manual name, overriding automatic-rename for this window.
func (w *window) rename(name string) {
	w.manualName = &name
}

// setOpt records a per-window option override (tmux's set-window-option on a
// specific window), lazily creating the store.
func (w *window) setOpt(k, v string) {
	if w.opts == nil {
		w.opts = map[string]string{}
	}
	w.opts[k] = v
}

// bitStr renders a tmux boolean flag: "1" for true, "" for false.
func bitStr(b bool) string {
	if b {
		return "1"
	}
	return ""
}

// clientSideCmd reports whether a command opens a client-owned overlay, so a
// hook firing it must route to the client rather than run server-side.
func clientSideCmd(cmd string) bool {
	switch cmd {
	case "command-prompt", "confirm-before", "display-menu":
		return true
	}
	return false
}

// onOff parses a tmux boolean option value; offOn renders one for show-options.
func onOff(s string) bool { return s == "on" || s == "1" || s == "true" }

// alertFires reports whether a bell/activity alert with the given tmux action
// (bell-action/activity-action: any/none/current/other) fires for a window that
// is / isn't the session's current window.
func alertFires(action string, isCurrent bool) bool {
	switch action {
	case "none":
		return false
	case "current":
		return isCurrent
	case "any":
		return true
	default: // "other" (and any unrecognized value)
		return !isCurrent
	}
}

func offOn(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (w *window) reflow() {
	w.borders = w.borders[:0]
	if w.zoomed {
		w.active.borderRow = -1
		w.active.resize(rect{0, 0, w.rows, w.cols})
		return
	}
	w.root.apply(rect{0, 0, w.rows, w.cols}, &w.borders, w.borderStatus)
}

// toggleZoom zooms the active pane to fill the window (hiding the others),
// or restores the tiled layout. No-op on a lone pane, which is already
// effectively zoomed.
func (w *window) toggleZoom() {
	if !w.zoomed && len(w.panes) < 2 {
		return
	}
	w.zoomed = !w.zoomed
	w.reflow()
}

// unzoom restores the tiled layout if zoomed — layout-changing operations
// (split, close, navigate, resize) all drop out of zoom first, like tmux.
func (w *window) unzoom() {
	if w.zoomed {
		w.zoomed = false
		w.reflow()
	}
}

// pathToActive returns the layout nodes from root down to the active leaf,
// or nil if the active pane isn't in the tree.
func (w *window) pathToActive() []*layoutNode {
	var path []*layoutNode
	var walk func(n *layoutNode) bool
	walk = func(n *layoutNode) bool {
		path = append(path, n)
		if n.pane == w.active {
			return true
		}
		if n.pane == nil && (walk(n.a) || walk(n.b)) {
			return true
		}
		path = path[:len(path)-1]
		return false
	}
	if !walk(w.root) {
		return nil
	}
	return path
}

// resizePane moves the divider nearest the active pane one step in dir:
// the closest ancestor split of matching orientation gets its fraction
// nudged by delta cells. This matches tmux's resize-pane in both cases —
// growing when the active pane is on the near side of the divider,
// shrinking when it's on the far side.
func (w *window) resizePane(dir string, delta int) {
	w.unzoom()
	wantVertical := dir == "left" || dir == "right"

	path := w.pathToActive()
	if path == nil {
		return
	}

	for i := len(path) - 2; i >= 0; i-- {
		node := path[i]
		if (node.dir == splitVertical) != wantVertical {
			continue
		}
		usable := node.r.Cols - 1
		if !wantVertical {
			usable = node.r.Rows - 1
		}
		if usable <= 0 {
			return
		}
		step := float64(delta) / float64(usable)
		if dir == "left" || dir == "up" {
			step = -step
		}
		node.frac += step
		if node.frac < 0 {
			node.frac = 0
		}
		if node.frac > 1 {
			node.frac = 1
		}
		w.layoutName = "" // hand-tuned now — a window resize should scale, not snap back
		w.reflow()
		return
	}
}

// resizePaneTo sets the active pane's width (-x) or height (-y) to an absolute
// size (tmux's resize-pane -x/-y). spec is cells ("80") or a percent of the
// window ("50%"), parsed by popupDim. It sets the nearest ancestor split of the
// matching orientation so the active pane's side gets that size — exact for a
// simple split (the common case), approximate when that side nests more panes.
func (w *window) resizePaneTo(width bool, spec string) {
	w.unzoom()

	path := w.pathToActive()
	if path == nil {
		return
	}

	for i := len(path) - 2; i >= 0; i-- {
		node := path[i]
		if (node.dir == splitVertical) != width {
			continue
		}
		usable, span := node.r.Rows-1, w.rows
		if width {
			usable, span = node.r.Cols-1, w.cols
		}
		if usable <= 0 {
			return
		}
		target := popupDim(spec, span)
		if target <= 0 {
			return
		}
		frac := float64(target) / float64(usable)
		if node.b == path[i+1] { // active is on the far side: size side b to target
			frac = 1 - frac
		}
		if frac < 0 {
			frac = 0
		} else if frac > 1 {
			frac = 1
		}
		node.frac = frac
		w.layoutName = "" // hand-tuned now — a window resize should scale, not snap back
		w.reflow()
		return
	}
}

func (w *window) resize(cols, rows int) {
	w.cols, w.rows = cols, rows
	// A named preset (main-vertical, tiled, …) defines pane sizes relative to
	// the window, so re-apply it at the new size rather than scaling the frozen
	// fractions. Without this, a layout built while the session was detached —
	// window width == main-pane-width, so the side column clamps to its 2-col
	// minimum — stays a permanent sliver once the client attaches wider (the
	// main-vertical-from-workspacer bug). A manual pane resize clears layoutName
	// (resizePane), so a hand-tuned layout still scales instead of snapping back.
	if w.layoutName != "" {
		w.setLayout(w.layoutName, w.mainW, w.mainH)
		return
	}
	w.reflow()
}

// split divides the active pane in two along dir, spawning a new pane in the
// new half. No-op if the active pane is too small to split.
func (w *window) split(dir splitDir, command, dirPath string) error {
	w.unzoom()
	active := w.active
	if dir == splitVertical && active.rect.Cols < 3 {
		return nil
	}
	if dir == splitHorizontal && active.rect.Rows < 3 {
		return nil
	}
	if dirPath == "" {
		dirPath = active.cwd()
	}

	node := w.findLeaf(w.root, active)
	newPane, err := spawnPane(w, w.sessionName, active.rect, dirPath, command) // rect is provisional, reflow fixes it
	if err != nil {
		return err
	}
	node.pane = nil
	node.dir = dir
	node.frac = 0.5
	node.a = &layoutNode{pane: active}
	node.b = &layoutNode{pane: newPane}

	w.panes = append(w.panes, newPane)
	w.setActive(newPane)
	w.reflow()
	return nil
}

func (w *window) findLeaf(n *layoutNode, p *pane) *layoutNode {
	if n.pane == p {
		return n
	}
	if n.pane != nil {
		return nil
	}
	if f := w.findLeaf(n.a, p); f != nil {
		return f
	}
	return w.findLeaf(n.b, p)
}

// closePane removes a pane, collapsing its side of the layout tree onto its
// sibling. Returns false if that was the window's last pane (caller should
// remove the whole window).
func (w *window) closePane(p *pane) bool {
	w.zoomed = false // reflow below restores the tiled layout
	idx := -1
	for i, pp := range w.panes {
		if pp == p {
			idx = i
			break
		}
	}
	if idx == -1 {
		return true
	}
	w.panes = append(w.panes[:idx], w.panes[idx+1:]...)
	if len(w.panes) == 0 {
		return false
	}

	parent, isA := w.root.findParent(p)
	if parent != nil {
		sibling := parent.a
		if isA {
			sibling = parent.b
		}
		*parent = *sibling
	}

	if w.active == p {
		w.setActive(w.panes[0])
	}
	w.reflow()
	return true
}

// swapPane exchanges the active pane's position with the previous or next
// pane (prefix+{ / prefix+}). The active pointer follows the pane to its new
// spot, like tmux.
func (w *window) swapPane(dir string) {
	if len(w.panes) < 2 {
		return
	}
	w.unzoom()
	idx := -1
	for i, p := range w.panes {
		if p == w.active {
			idx = i
			break
		}
	}
	step := 1
	if dir == "prev" {
		step = len(w.panes) - 1
	}
	other := w.panes[(idx+step)%len(w.panes)]

	la := w.findLeaf(w.root, w.active)
	lo := w.findLeaf(w.root, other)
	la.pane, lo.pane = lo.pane, la.pane
	w.panes[idx], w.panes[(idx+step)%len(w.panes)] = other, w.active
	w.reflow()
}

// adoptWindow wraps an existing pane (broken out of another window) in a new
// single-pane window. It does NOT reflow: the pane's origin actor is still the
// sole writer of p.term until break-pane hands the relay over, so the caller
// reflows on the new actor's goroutine afterwards (avoids a cross-goroutine
// p.term race). See breakPane.
func adoptWindow(p *pane, cols, rows int, sessionName string) *window {
	w := &window{sessionName: sessionName, cols: cols, rows: rows}
	w.root = &layoutNode{pane: p}
	w.panes = []*pane{p}
	w.setActive(p)
	p.win = w
	return w
}

// joinPaneAt inserts an existing pane (removed from another window) by splitting
// pane `at` along dir — tmux's join-pane/move-pane. at nil (or the default
// caller) splits the window's active pane; dir splitHorizontal stacks them
// (tmux's default), splitVertical puts them side by side (-h).
func (w *window) joinPaneAt(p, at *pane, dir splitDir) {
	w.unzoom()
	if at == nil {
		at = w.active
	}
	node := w.findLeaf(w.root, at)
	node.pane = nil
	node.dir = dir
	node.frac = 0.5
	node.a = &layoutNode{pane: at}
	node.b = &layoutNode{pane: p}
	w.panes = append(w.panes, p)
	w.setActive(p)
	p.win = w
	w.reflow()
}

// adjacent finds the pane bordering the active one in the given direction,
// picking the candidate with the most overlap along the cross axis.
func (w *window) adjacent(dir string) *pane {
	a := w.active.rect
	var best *pane
	for _, p := range w.panes {
		if p == w.active {
			continue
		}
		r := p.rect
		var match bool
		switch dir {
		case "right":
			match = r.Col == a.Col+a.Cols+1 && r.rowsOverlap(a)
		case "left":
			match = a.Col == r.Col+r.Cols+1 && r.rowsOverlap(a)
		case "down":
			match = r.Row == a.Row+a.Rows+1 && r.colsOverlap(a)
		case "up":
			match = a.Row == r.Row+r.Rows+1 && r.colsOverlap(a)
		}
		// Among the panes touching the moving edge (and overlapping the active
		// pane's span), pick the most-recently-active — highest activePoint. This
		// is tmux's window_pane_choose_best: navigate left then right returns to
		// the pane you came from, not whichever neighbor happens to be biggest.
		if match && (best == nil || p.activePoint > best.activePoint) {
			best = p
		}
	}
	return best
}

// layoutOrder is the cycle next-layout walks — tmux's five preset layouts.
var layoutOrder = []string{"even-horizontal", "even-vertical", "main-horizontal", "main-vertical", "tiled"}

// leaves wraps each pane in a fresh leaf node, preserving order.
func leaves(panes []*pane) []*layoutNode {
	ns := make([]*layoutNode, len(panes))
	for i, p := range panes {
		ns[i] = &layoutNode{pane: p}
	}
	return ns
}

// evenTree builds a right-leaning equal split of nodes along dir: each level
// gives 1/len of its space to the first node and recurses on the rest, so all
// leaves end up the same size.
func evenTree(nodes []*layoutNode, dir splitDir) *layoutNode {
	if len(nodes) == 1 {
		return nodes[0]
	}
	return &layoutNode{dir: dir, frac: 1.0 / float64(len(nodes)), a: nodes[0], b: evenTree(nodes[1:], dir)}
}

// mainTree: the first pane takes mainSize cells along dir (tmux's
// main-pane-width/height), the rest share the leftover space stacked on the
// cross axis. dir=splitVertical => main on the left (main-vertical) with total
// = window cols; splitHorizontal => main on top (main-horizontal) with total =
// window rows. splitSizes clamps if mainSize crowds the others out.
func mainTree(ls []*layoutNode, dir splitDir, mainSize, total int) *layoutNode {
	if len(ls) == 1 {
		return ls[0]
	}
	restDir := splitHorizontal
	if dir == splitHorizontal {
		restDir = splitVertical
	}
	frac := 0.5
	if total > 0 {
		frac = float64(mainSize) / float64(total)
	}
	return &layoutNode{dir: dir, frac: frac, a: ls[0], b: evenTree(ls[1:], restDir)}
}

// tiledTree: a grid filled row by row, cols = ceil(sqrt(n)). Rows stack
// (horizontal splits); panes within a row sit side by side (vertical splits).
func tiledTree(ls []*layoutNode) *layoutNode {
	cols := 1
	for cols*cols < len(ls) {
		cols++
	}
	var rows []*layoutNode
	for i := 0; i < len(ls); i += cols {
		end := i + cols
		if end > len(ls) {
			end = len(ls)
		}
		rows = append(rows, evenTree(ls[i:end], splitVertical))
	}
	return evenTree(rows, splitHorizontal)
}

// setLayout rebuilds the pane tree into one of tmux's preset arrangements over
// the window's current panes (w.panes order), then reflows. Unknown name is a
// no-op.
func (w *window) setLayout(name string, mainW, mainH int) {
	w.unzoom()
	ls := leaves(w.panes)
	switch name {
	case "even-horizontal":
		w.root = evenTree(ls, splitVertical)
	case "even-vertical":
		w.root = evenTree(ls, splitHorizontal)
	case "main-vertical":
		w.root = mainTree(ls, splitVertical, mainW, w.cols)
	case "main-horizontal":
		w.root = mainTree(ls, splitHorizontal, mainH, w.rows)
	case "tiled":
		w.root = tiledTree(ls)
	default:
		return
	}
	w.layoutName = name
	w.mainW, w.mainH = mainW, mainH // remembered so resize() can re-apply this preset
	w.reflow()
}

// cycleLayout steps through layoutOrder from the current preset (wrapping):
// next-layout is step +1 (tmux prefix+Space), previous-layout is -1. A window
// with no preset yet (manual splits) starts at the first.
func (w *window) cycleLayout(step, mainW, mainH int) {
	next := 0
	for i, n := range layoutOrder {
		if n == w.layoutName {
			next = ((i+step)%len(layoutOrder) + len(layoutOrder)) % len(layoutOrder)
			break
		}
	}
	w.setLayout(layoutOrder[next], mainW, mainH)
}

// orderedLeaves collects leaf nodes in visual (tree) order, left/top first.
func (w *window) orderedLeaves() []*layoutNode {
	var ls []*layoutNode
	var walk func(n *layoutNode)
	walk = func(n *layoutNode) {
		if n.pane != nil {
			ls = append(ls, n)
			return
		}
		walk(n.a)
		walk(n.b)
	}
	walk(w.root)
	return ls
}

// rotateWindow shifts every pane one slot through the layout, wrapping: -U
// (tmux prefix+C-o) moves each pane toward the front, -D the other way. The
// tree structure stays put; only the pane contents move between slots.
func (w *window) rotateWindow(dir string) {
	if len(w.panes) < 2 {
		return
	}
	w.unzoom()
	ls := w.orderedLeaves()
	ps := make([]*pane, len(ls))
	for i, n := range ls {
		ps[i] = n.pane
	}
	if dir == "-D" {
		ps = append(ps[len(ps)-1:], ps[:len(ps)-1]...)
	} else {
		ps = append(ps[1:], ps[:1]...)
	}
	for i, n := range ls {
		n.pane = ps[i]
	}
	w.panes = ps
	w.reflow()
}

// layout reports the window's current pane arrangement for the client to
// compose its own chrome from (dividers, active-pane highlight, the
// display-panes number overlay) instead of receiving pre-drawn glyphs mixed
// into pane content.
func (w *window) layout() *proto.Layout {
	if w.zoomed {
		a := w.active
		num := w.paneBase
		for i, p := range w.panes {
			if p == a {
				num = i + w.paneBase
			}
		}
		return &proto.Layout{Cols: w.cols, Rows: w.rows, Panes: []proto.PaneRect{
			{ID: a.id, Number: num, Row: a.rect.Row, Col: a.rect.Col, Rows: a.rect.Rows, Cols: a.rect.Cols, Active: true, WantsMouse: a.wantsMouse(), KeyFlags: a.keyFlags(), Marked: a.marked},
		}, ShowNumbers: w.showNumbers}
	}
	panes := make([]proto.PaneRect, len(w.panes))
	for i, p := range w.panes {
		panes[i] = proto.PaneRect{
			ID: p.id, Number: i + w.paneBase, Row: p.rect.Row, Col: p.rect.Col, Rows: p.rect.Rows, Cols: p.rect.Cols,
			Active: p == w.active, WantsMouse: p.wantsMouse(), KeyFlags: p.keyFlags(), Marked: p.marked, BorderRow: p.borderRow, BorderLabel: p.borderLabel,
		}
	}
	borders := make([]proto.BorderSeg, len(w.borders))
	for i, b := range w.borders {
		borders[i] = proto.BorderSeg{
			Vertical: b.vertical, Fixed: b.fixed, Start: b.start, End: b.end,
		}
	}
	return &proto.Layout{Cols: w.cols, Rows: w.rows, Panes: panes, Borders: borders, ShowNumbers: w.showNumbers}
}

// content returns every pane's full content, for messages that also carry
// a fresh Layout (attach, split, close, resize, window switch) — the
// client has no prior state to diff against in pane-local coordinates once
// the arrangement itself has changed.
func (w *window) content() []proto.PaneContent {
	if w.zoomed {
		return []proto.PaneContent{w.active.fullContent()}
	}
	content := make([]proto.PaneContent, len(w.panes))
	for i, p := range w.panes {
		content[i] = p.fullContent()
	}
	return content
}
