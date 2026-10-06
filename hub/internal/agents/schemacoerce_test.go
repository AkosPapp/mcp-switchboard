package agents

import (
	"context"
	"reflect"
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

var coerceSchema = map[string]any{"type": "object",
	"properties": map[string]any{
		"path":      map[string]any{"type": "string"},
		"recursive": map[string]any{"type": "boolean"},
		"limit":     map[string]any{"type": "integer"},
		"ratio":     map[string]any{"type": "number"},
		"mode":      map[string]any{"type": "string", "enum": []any{"never", "destructive", "always"}},
		"tags":      map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
		"opts":      map[string]any{"type": "object", "properties": map[string]any{"deep": map[string]any{"type": "boolean"}}},
	},
	"required": []any{"path"},
}

func TestNormalizeArgsCoercesEquivalentValues(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want map[string]any
	}{
		{"the reported case", map[string]any{"path": "client", "recursive": "True"},
			map[string]any{"path": "client", "recursive": true}},
		{"lower and upper bool", map[string]any{"path": "a", "recursive": " FALSE "},
			map[string]any{"path": "a", "recursive": false}},
		{"numeric strings", map[string]any{"path": "a", "limit": "50", "ratio": "3.5"},
			map[string]any{"path": "a", "limit": float64(50), "ratio": 3.5}},
		{"integer from 50.0", map[string]any{"path": "a", "limit": "50.0"},
			map[string]any{"path": "a", "limit": float64(50)}},
		{"number and bool into a string field", map[string]any{"path": float64(123)},
			map[string]any{"path": "123"}},
		{"bool into a string field", map[string]any{"path": true},
			map[string]any{"path": "true"}},
		{"json array in a string", map[string]any{"path": "a", "tags": "[1, 2]"},
			map[string]any{"path": "a", "tags": []any{float64(1), float64(2)}}},
		{"array items coerced", map[string]any{"path": "a", "tags": []any{"1", float64(2)}},
			map[string]any{"path": "a", "tags": []any{float64(1), float64(2)}}},
		{"json object in a string, nested bool", map[string]any{"path": "a", "opts": `{"deep": "true"}`},
			map[string]any{"path": "a", "opts": map[string]any{"deep": true}}},
		{"enum differing only by case", map[string]any{"path": "a", "mode": "Destructive"},
			map[string]any{"path": "a", "mode": "destructive"}},
		{"already valid is untouched", map[string]any{"path": "a", "recursive": true, "limit": float64(3)},
			map[string]any{"path": "a", "recursive": true, "limit": float64(3)}},
	}
	for _, c := range cases {
		got := normalizeArgs(coerceSchema, c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v, want %#v", c.name, got, c.want)
		}
		if msg := validateArgs(coerceSchema, got); msg != "" {
			t.Errorf("%s: still invalid after coercion: %s", c.name, msg)
		}
	}
}

func TestNormalizeArgsLeavesUnrepairableValuesAlone(t *testing.T) {
	for name, in := range map[string]map[string]any{
		"word for a bool":     {"path": "a", "recursive": "yes"},
		"non-numeric integer": {"path": "a", "limit": "many"},
		"fractional integer":  {"path": "a", "limit": "2.5"},
		"unknown enum":        {"path": "a", "mode": "sometimes"},
		"not json array":      {"path": "a", "tags": "one,two"},
		"object for a string": {"path": map[string]any{"x": 1}},
	} {
		got := normalizeArgs(coerceSchema, in)
		if !reflect.DeepEqual(got, in) {
			t.Errorf("%s: changed to %#v", name, got)
		}
		if validateArgs(coerceSchema, got) == "" {
			t.Errorf("%s: unrepairable value passed validation", name)
		}
	}
}

func TestNormalizeArgsDoesNotMutateItsInput(t *testing.T) {
	in := map[string]any{"path": "a", "recursive": "True", "tags": []any{"1"}}
	normalizeArgs(coerceSchema, in)
	if in["recursive"] != "True" || in["tags"].([]any)[0] != "1" {
		t.Errorf("input was mutated: %#v", in)
	}
}

// The reported failure end to end: the tool must run, and it must receive a
// real boolean, not the string the model wrote.
func TestExecToolCoercesBooleanStringAndRuns(t *testing.T) {
	e := newEnv(t)
	e.addServer("box", "", "demo", registry.ToolInfo{Name: "dir_list"})
	e.setSchema("demo", "dir_list", map[string]any{"type": "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}, "recursive": map[string]any{"type": "boolean"}},
		"required":   []any{"path"}})
	a := e.agent("a", func(in *CreateAgentInput) {
		in.Grants = []store.Grant{{Label: "*", Project: "*", Server: "*", Allowed: true}}
	})
	out := e.m.execTool(context.Background(), a, nil, "box__demo__dir_list",
		map[string]any{"path": "client", "recursive": "True"}, "")
	if out.IsError {
		t.Fatalf("call was rejected: %s", out.Text())
	}
	rows := e.callRows(store.CallFilter{})
	if len(rows) != 1 || rows[0].Status != store.StatusOK || rows[0].Arguments["recursive"] != true {
		t.Fatalf("logged call: %+v", rows)
	}
}
