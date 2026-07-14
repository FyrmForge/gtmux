package format

import (
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// splitOp detects a leading multi-arg operator on a #{...} body — the tmux
// comparison/logical/match/arithmetic/char operators, whose args (unlike the
// b:/d:/t:/n: modifiers) are expanded as format fragments, not looked up as
// variables. Returns the operator, the remaining body, and whether one matched.
func splitOp(body string) (op, rest string, ok bool) {
	c := strings.IndexByte(body, ':')
	if c <= 0 {
		return "", "", false
	}
	op = body[:c]
	switch op {
	case "==", "!=", "<", ">", "<=", ">=", "||", "&&", "m", "m/r", "e", "a":
		return op, body[c+1:], true
	}
	if strings.HasPrefix(op, "e|") { // e|N: arithmetic with N decimals
		return op, body[c+1:], true
	}
	return "", "", false
}

// applyOp evaluates one operator over its (top-level-comma-split, each
// expanded) arguments and returns a string result ("1"/"0" for the predicates).
func applyOp(op, rest string, vars map[string]string, loop LoopFunc) string {
	// e:/e|N: is a single arithmetic expression, expanded then evaluated.
	if op == "e" || strings.HasPrefix(op, "e|") {
		prec := -1
		if strings.HasPrefix(op, "e|") {
			if p, err := strconv.Atoi(op[2:]); err == nil {
				prec = p
			}
		}
		v, ok := evalArith(ExpandLoop(rest, vars, loop))
		if !ok {
			return ""
		}
		return formatNum(v, prec)
	}

	args := SplitTopLevel(rest)
	exp := make([]string, len(args))
	for i, a := range args {
		exp[i] = ExpandLoop(a, vars, loop)
	}
	get := func(i int) string {
		if i < len(exp) {
			return exp[i]
		}
		return ""
	}

	switch op {
	case "a":
		n, err := strconv.Atoi(strings.TrimSpace(get(0)))
		if err != nil {
			return ""
		}
		return string(rune(n))
	case "==":
		return boolStr(get(0) == get(1))
	case "!=":
		return boolStr(get(0) != get(1))
	case "<", ">", "<=", ">=":
		return boolStr(numCompare(op, get(0), get(1)))
	case "||":
		return boolStr(truthy(get(0)) || truthy(get(1)))
	case "&&":
		return boolStr(truthy(get(0)) && truthy(get(1)))
	case "m":
		ok, err := path.Match(get(0), get(1))
		return boolStr(err == nil && ok)
	case "m/r":
		re, err := regexp.Compile(get(0))
		return boolStr(err == nil && re.MatchString(get(1)))
	}
	return ""
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// truthy mirrors tmux's notion for the logical operators: non-empty and not "0".
func truthy(s string) bool { return s != "" && s != "0" }

// numCompare compares two operands numerically when both parse as numbers, else
// lexically — tmux's behavior for < > <= >=.
func numCompare(op, a, b string) bool {
	x, ex := strconv.ParseFloat(strings.TrimSpace(a), 64)
	y, ey := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if ex != nil || ey != nil {
		switch op {
		case "<":
			return a < b
		case ">":
			return a > b
		case "<=":
			return a <= b
		case ">=":
			return a >= b
		}
		return false
	}
	switch op {
	case "<":
		return x < y
	case ">":
		return x > y
	case "<=":
		return x <= y
	case ">=":
		return x >= y
	}
	return false
}

// formatNum renders an arithmetic result: N decimals if a precision was given,
// else an integer when the value is integral, else the shortest round-trip.
func formatNum(v float64, prec int) string {
	if prec >= 0 {
		return strconv.FormatFloat(v, 'f', prec, 64)
	}
	if v == math.Trunc(v) && !math.IsInf(v, 0) {
		return strconv.FormatInt(int64(v), 10)
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// evalArith evaluates a + - * / % expression with parentheses and unary minus
// over float64. Returns false on a syntax/eval error (e.g. divide by zero).
func evalArith(s string) (float64, bool) {
	p := &arith{s: s}
	v, ok := p.expr()
	if !ok {
		return 0, false
	}
	p.skip()
	if p.i != len(p.s) {
		return 0, false
	}
	return v, true
}

type arith struct {
	s string
	i int
}

func (p *arith) skip() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *arith) expr() (float64, bool) {
	v, ok := p.term()
	if !ok {
		return 0, false
	}
	for {
		p.skip()
		if p.i < len(p.s) && (p.s[p.i] == '+' || p.s[p.i] == '-') {
			op := p.s[p.i]
			p.i++
			r, ok := p.term()
			if !ok {
				return 0, false
			}
			if op == '+' {
				v += r
			} else {
				v -= r
			}
		} else {
			return v, true
		}
	}
}

func (p *arith) term() (float64, bool) {
	v, ok := p.factor()
	if !ok {
		return 0, false
	}
	for {
		p.skip()
		if p.i < len(p.s) && (p.s[p.i] == '*' || p.s[p.i] == '/' || p.s[p.i] == '%') {
			op := p.s[p.i]
			p.i++
			r, ok := p.factor()
			if !ok {
				return 0, false
			}
			switch op {
			case '*':
				v *= r
			case '/':
				if r == 0 {
					return 0, false
				}
				v /= r
			case '%':
				if r == 0 {
					return 0, false
				}
				v = math.Mod(v, r)
			}
		} else {
			return v, true
		}
	}
}

func (p *arith) factor() (float64, bool) {
	p.skip()
	if p.i < len(p.s) && p.s[p.i] == '(' {
		p.i++
		v, ok := p.expr()
		if !ok {
			return 0, false
		}
		p.skip()
		if p.i >= len(p.s) || p.s[p.i] != ')' {
			return 0, false
		}
		p.i++
		return v, true
	}
	if p.i < len(p.s) && p.s[p.i] == '-' {
		p.i++
		v, ok := p.factor()
		if !ok {
			return 0, false
		}
		return -v, true
	}
	start := p.i
	for p.i < len(p.s) && ((p.s[p.i] >= '0' && p.s[p.i] <= '9') || p.s[p.i] == '.') {
		p.i++
	}
	if p.i == start {
		return 0, false
	}
	v, err := strconv.ParseFloat(p.s[start:p.i], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
