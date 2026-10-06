package agents

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

const (
	calcName     = "switchboard.calc"
	calcMaxLen   = 500
	calcMaxDepth = 50
)

var calcTool = &sbTool{
	name: calcName,
	desc: "Evaluate an arithmetic expression exactly instead of guessing. Supports + - * / % ^ **, parentheses, unary minus, the functions sqrt abs min max round floor ceil ln log10 exp sin cos tan (radians), the constants pi and e, decimals and scientific notation, and percentages like '15% of 80'. " +
		`Example: {"expression": "(3.5 * 12) + 15% of 80"}`,
	schema: obj([]string{"expression"}, map[string]any{
		"expression": typ("string", "the arithmetic expression to evaluate"),
	}),
	ann:     &mcp.ToolAnnotations{ReadOnlyHint: true},
	visible: func(store.Capabilities) bool { return true },
	run: func(_ *Manager, _ context.Context, _ *callCtx, args map[string]any) (any, error) {
		expr := strings.TrimSpace(argStr(args, "expression"))
		res, err := calcEval(expr)
		if err != nil {
			return nil, err
		}
		return map[string]any{"expression": expr, "result": res}, nil
	},
}

func init() { registerExtraTool(calcTool, nil) }

type calcParser struct {
	s     string
	pos   int
	depth int
}

// calcEval evaluates expr with a recursive-descent parser; no code is ever
// executed. Errors wrap ErrInvalid.
func calcEval(expr string) (float64, error) {
	if expr == "" {
		return 0, fmt.Errorf("%w: expression is required", ErrInvalid)
	}
	if len(expr) > calcMaxLen {
		return 0, fmt.Errorf("%w: expression longer than %d characters", ErrInvalid, calcMaxLen)
	}
	p := &calcParser{s: expr}
	v, err := p.expr()
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	p.skip()
	if p.pos < len(p.s) {
		return 0, fmt.Errorf("%w: unexpected %q at position %d", ErrInvalid, p.s[p.pos:p.pos+1], p.pos+1)
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("%w: result is not a finite number", ErrInvalid)
	}
	return v, nil
}

func (p *calcParser) skip() {
	for p.pos < len(p.s) && (p.s[p.pos] == ' ' || p.s[p.pos] == '\t' || p.s[p.pos] == '\n') {
		p.pos++
	}
}

func (p *calcParser) peek() byte {
	p.skip()
	if p.pos < len(p.s) {
		return p.s[p.pos]
	}
	return 0
}

func (p *calcParser) enter() error {
	p.depth++
	if p.depth > calcMaxDepth {
		return fmt.Errorf("expression nested deeper than %d levels", calcMaxDepth)
	}
	return nil
}

func (p *calcParser) expr() (float64, error) {
	if err := p.enter(); err != nil {
		return 0, err
	}
	defer func() { p.depth-- }()
	v, err := p.term()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		if c != '+' && c != '-' {
			return v, nil
		}
		p.pos++
		r, err := p.term()
		if err != nil {
			return 0, err
		}
		if c == '+' {
			v += r
		} else {
			v -= r
		}
	}
}

// percentOf reports whether the input at pos is "% of", consuming it if so.
func (p *calcParser) percentOf() bool {
	i := p.pos + 1
	for i < len(p.s) && p.s[i] == ' ' {
		i++
	}
	if i+2 <= len(p.s) && strings.EqualFold(p.s[i:i+2], "of") && (i+2 == len(p.s) || !isAlnum(p.s[i+2])) {
		p.pos = i + 2
		return true
	}
	return false
}

func isAlnum(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (p *calcParser) term() (float64, error) {
	v, err := p.unary()
	if err != nil {
		return 0, err
	}
	for {
		c := p.peek()
		if c != '*' && c != '/' && c != '%' {
			return v, nil
		}
		if c == '*' && p.pos+1 < len(p.s) && p.s[p.pos+1] == '*' {
			return v, nil // '**' is handled by power
		}
		if c == '%' && p.percentOf() {
			r, err := p.unary()
			if err != nil {
				return 0, err
			}
			v = v / 100 * r
			continue
		}
		p.pos++
		r, err := p.unary()
		if err != nil {
			return 0, err
		}
		switch c {
		case '*':
			v *= r
		case '/':
			if r == 0 {
				return 0, fmt.Errorf("division by zero")
			}
			v /= r
		case '%':
			if r == 0 {
				return 0, fmt.Errorf("modulo by zero")
			}
			v = math.Mod(v, r)
		}
	}
}

// unary binds looser than ^, so -2^2 is -4.
func (p *calcParser) unary() (float64, error) {
	if err := p.enter(); err != nil {
		return 0, err
	}
	defer func() { p.depth-- }()
	switch p.peek() {
	case '-':
		p.pos++
		v, err := p.unary()
		return -v, err
	case '+':
		p.pos++
		return p.unary()
	}
	return p.power()
}

func (p *calcParser) power() (float64, error) {
	base, err := p.primary()
	if err != nil {
		return 0, err
	}
	c := p.peek()
	if c == '^' || (c == '*' && p.pos+1 < len(p.s) && p.s[p.pos+1] == '*') {
		if c == '^' {
			p.pos++
		} else {
			p.pos += 2
		}
		exp, err := p.unary() // right associative
		if err != nil {
			return 0, err
		}
		if base == 0 && exp < 0 {
			return 0, fmt.Errorf("division by zero (0 to a negative power)")
		}
		r := math.Pow(base, exp)
		if math.IsNaN(r) {
			return 0, fmt.Errorf("%v ^ %v is not a real number", base, exp)
		}
		return r, nil
	}
	return base, nil
}

func (p *calcParser) primary() (float64, error) {
	c := p.peek()
	switch {
	case c == 0:
		return 0, fmt.Errorf("unexpected end of expression")
	case c == '(':
		p.pos++
		v, err := p.expr()
		if err != nil {
			return 0, err
		}
		if p.peek() != ')' {
			return 0, fmt.Errorf("missing closing parenthesis")
		}
		p.pos++
		return v, nil
	case c >= '0' && c <= '9' || c == '.':
		return p.number()
	case c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		start := p.pos
		for p.pos < len(p.s) && isAlnum(p.s[p.pos]) {
			p.pos++
		}
		name := strings.ToLower(p.s[start:p.pos])
		if p.peek() != '(' {
			switch name {
			case "pi":
				return math.Pi, nil
			case "e":
				return math.E, nil
			}
			return 0, fmt.Errorf("unknown name %q", name)
		}
		p.pos++
		var argv []float64
		if p.peek() == ')' {
			p.pos++
		} else {
			for {
				v, err := p.expr()
				if err != nil {
					return 0, err
				}
				argv = append(argv, v)
				if p.peek() == ',' {
					p.pos++
					continue
				}
				if p.peek() != ')' {
					return 0, fmt.Errorf("missing closing parenthesis after %s(", name)
				}
				p.pos++
				break
			}
		}
		return calcFunc(name, argv)
	}
	return 0, fmt.Errorf("unexpected %q at position %d", string(c), p.pos+1)
}

func (p *calcParser) number() (float64, error) {
	start := p.pos
	for p.pos < len(p.s) && (p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '.') {
		p.pos++
	}
	if p.pos < len(p.s) && (p.s[p.pos] == 'e' || p.s[p.pos] == 'E') {
		j := p.pos + 1
		if j < len(p.s) && (p.s[j] == '+' || p.s[j] == '-') {
			j++
		}
		if j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
			for j < len(p.s) && p.s[j] >= '0' && p.s[j] <= '9' {
				j++
			}
			p.pos = j
		}
	}
	v, err := strconv.ParseFloat(p.s[start:p.pos], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid number %q", p.s[start:p.pos])
	}
	return v, nil
}

func calcFunc(name string, a []float64) (float64, error) {
	one := func(f func(float64) float64) (float64, error) {
		if len(a) != 1 {
			return 0, fmt.Errorf("%s takes exactly 1 argument", name)
		}
		return f(a[0]), nil
	}
	switch name {
	case "sqrt":
		if len(a) == 1 && a[0] < 0 {
			return 0, fmt.Errorf("sqrt of a negative number")
		}
		return one(math.Sqrt)
	case "abs":
		return one(math.Abs)
	case "round":
		return one(math.Round)
	case "floor":
		return one(math.Floor)
	case "ceil":
		return one(math.Ceil)
	case "exp":
		return one(math.Exp)
	case "sin":
		return one(math.Sin)
	case "cos":
		return one(math.Cos)
	case "tan":
		return one(math.Tan)
	case "ln", "log10":
		if len(a) == 1 && a[0] <= 0 {
			return 0, fmt.Errorf("%s of a non-positive number", name)
		}
		if name == "ln" {
			return one(math.Log)
		}
		return one(math.Log10)
	case "min", "max":
		if len(a) == 0 {
			return 0, fmt.Errorf("%s needs at least 1 argument", name)
		}
		r := a[0]
		for _, v := range a[1:] {
			if name == "min" {
				r = math.Min(r, v)
			} else {
				r = math.Max(r, v)
			}
		}
		return r, nil
	}
	return 0, fmt.Errorf("unknown function %q", name)
}
