package server

// rect is a pane's position and size within its window's composite screen.
type rect struct {
	Row, Col, Rows, Cols int
}

func (r rect) rowsOverlap(o rect) bool {
	return max(r.Row, o.Row) < min(r.Row+r.Rows, o.Row+o.Rows)
}

func (r rect) colsOverlap(o rect) bool {
	return max(r.Col, o.Col) < min(r.Col+r.Cols, o.Col+o.Cols)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

type splitDir int

const (
	splitVertical   splitDir = iota // side by side, dividing columns, vertical divider line
	splitHorizontal                 // stacked, dividing rows, horizontal divider line
)

// borderSeg is one divider line between panes, drawn by the compositor.
// node is the internal layoutNode this divider belongs to, so a mouse drag
// on the border knows whose split fraction to adjust.
type borderSeg struct {
	vertical   bool // fixed column spanning rows, vs fixed row spanning columns
	fixed      int
	start, end int // [start, end) along the other axis
	node       *layoutNode
}

// layoutNode is a binary tree: a leaf holds a pane, an internal node splits
// its rect between two children, giving frac of the divisible space to a.
type layoutNode struct {
	pane *pane
	dir  splitDir
	a, b *layoutNode
	frac float64
	r    rect // the rect this node last covered, set by apply
}

// splitSizes divides total cells (minus 1 for the divider) between the two
// children by frac, keeping both sides at least 2 cells when there's room.
func (n *layoutNode) splitSizes(total int) (a, b int) {
	usable := total - 1
	a = int(float64(usable)*n.frac + 0.5)
	const minSide = 2
	if usable < 2*minSide {
		a = usable / 2 // too cramped for min-size clamps: just halve
	} else {
		if a < minSide {
			a = minSide
		}
		if a > usable-minSide {
			a = usable - minSide
		}
	}
	return a, usable - a
}

// apply computes rects for all leaf panes (resizing their PTY/emu state) and
// appends the divider segments needed to draw borders between them.
func (n *layoutNode) apply(r rect, borders *[]borderSeg, borderStatus string) {
	n.r = r
	if n.pane != nil {
		// pane-border-status reserves one row of the pane for its label; the pane
		// content gets the rest. borderRow is that reserved row (window-space).
		n.pane.borderRow = -1
		pr := r
		if r.Rows > 1 {
			switch borderStatus {
			case "top":
				n.pane.borderRow = r.Row
				pr = rect{Row: r.Row + 1, Col: r.Col, Rows: r.Rows - 1, Cols: r.Cols}
			case "bottom":
				n.pane.borderRow = r.Row + r.Rows - 1
				pr = rect{Row: r.Row, Col: r.Col, Rows: r.Rows - 1, Cols: r.Cols}
			}
		}
		n.pane.resize(pr)
		return
	}
	switch n.dir {
	case splitVertical:
		aCols, bCols := n.splitSizes(r.Cols)
		n.a.apply(rect{Row: r.Row, Col: r.Col, Rows: r.Rows, Cols: aCols}, borders, borderStatus)
		n.b.apply(rect{Row: r.Row, Col: r.Col + aCols + 1, Rows: r.Rows, Cols: bCols}, borders, borderStatus)
		*borders = append(*borders, borderSeg{vertical: true, fixed: r.Col + aCols, start: r.Row, end: r.Row + r.Rows, node: n})
	case splitHorizontal:
		aRows, bRows := n.splitSizes(r.Rows)
		n.a.apply(rect{Row: r.Row, Col: r.Col, Rows: aRows, Cols: r.Cols}, borders, borderStatus)
		n.b.apply(rect{Row: r.Row + aRows + 1, Col: r.Col, Rows: bRows, Cols: r.Cols}, borders, borderStatus)
		*borders = append(*borders, borderSeg{vertical: false, fixed: r.Row + aRows, start: r.Col, end: r.Col + r.Cols, node: n})
	}
}

// findParent returns the internal node whose child leaf holds p, and whether
// p is that node's a (vs b) child. Returns nil if p is the tree's only leaf.
func (n *layoutNode) findParent(p *pane) (parent *layoutNode, isA bool) {
	if n.pane != nil {
		return nil, false
	}
	if n.a.pane == p {
		return n, true
	}
	if n.b.pane == p {
		return n, false
	}
	if pr, ia := n.a.findParent(p); pr != nil {
		return pr, ia
	}
	return n.b.findParent(p)
}
