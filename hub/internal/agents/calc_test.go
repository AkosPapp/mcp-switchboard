package agents

import (
	"errors"
	"math"
	"strings"
	"testing"
)

func TestCalcEval(t *testing.T) {
	ok := []struct {
		in   string
		want float64
	}{
		{"1+2*3", 7}, {"(1+2)*3", 9}, {"-2^2", -4}, {"2^3^2", 512}, {"2**10", 1024},
		{"10 % 3", 1}, {"15% of 80", 12}, {"15 % of 80 + 1", 13}, {"7/2", 3.5},
		{"sqrt(16)+abs(-2)", 6}, {"min(3,1,2)", 1}, {"max(3, 9)", 9}, {"round(2.5)", 3},
		{"floor(2.9)+ceil(2.1)", 5}, {"ln(e)", 1}, {"log10(1000)", 3}, {"exp(0)", 1},
		{"sin(0)+cos(0)", 1}, {"pi", math.Pi}, {"1.5e3", 1500}, {"2e-1", 0.2}, {"--3", 3},
		{"2*-3", -6}, {".5+.5", 1},
	}
	for _, c := range ok {
		got, err := calcEval(c.in)
		if err != nil || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%q: got %v, %v want %v", c.in, got, err, c.want)
		}
	}
}

func TestCalcErrors(t *testing.T) {
	for _, in := range []string{
		"", "1/0", "5 % 0", "0^-1", "sqrt(-1)", "ln(0)", "1+", "(1", "1)", "foo", "foo(1)", "sqrt(1,2)",
		"1 2", "system('x')", "2^0.5^", "(-8)^(1/3)", "1e999*10", strings.Repeat("1+", 300) + "1",
		strings.Repeat("(", 100) + "1" + strings.Repeat(")", 100), "min()",
	} {
		if v, err := calcEval(in); err == nil || !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: got %v, %v; want ErrInvalid", in, v, err)
		}
	}
}

func TestCalcTool(t *testing.T) {
	out, err := calcTool.run(nil, nil, nil, map[string]any{"expression": " 2+2 "})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["result"] != 4.0 || m["expression"] != "2+2" {
		t.Fatalf("%v", m)
	}
	if !calcTool.ann.ReadOnlyHint {
		t.Fatal("not read only")
	}
}
