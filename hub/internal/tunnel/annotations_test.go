package tunnel

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestAnnotationsOfCarriesIrreversibleFromMeta(t *testing.T) {
	a := &mcp.ToolAnnotations{}
	ann := annotationsOf(a, mcp.Meta{"switchboard": map[string]any{
		"irreversible":       true,
		"irreversibleReason": "moves remote history",
	}})
	if ann == nil || !ann.Irreversible || ann.Reason != "moves remote history" {
		t.Fatalf("irreversible hint lost: %+v", ann)
	}
	// A tool with no annotations at all but the meta flag still gets one.
	ann = annotationsOf(nil, mcp.Meta{"switchboard": map[string]any{"irreversible": true}})
	if ann == nil || !ann.Irreversible {
		t.Fatalf("meta-only annotation dropped: %+v", ann)
	}
	// Junk meta is ignored, not fatal.
	if ann = annotationsOf(a, mcp.Meta{"switchboard": "nope"}); ann == nil || ann.Irreversible {
		t.Fatalf("junk meta mishandled: %+v", ann)
	}
	if annotationsOf(nil, nil) != nil {
		t.Error("nothing at all should stay nil")
	}
}

func TestAnnotationsOfIgnoresIrreversibleUnderWrongKey(t *testing.T) {
	// A foreign server's unrelated _meta must not trip the gate.
	ann := annotationsOf(&mcp.ToolAnnotations{}, mcp.Meta{"irreversible": true})
	if ann == nil || ann.Irreversible {
		t.Fatalf("non-switchboard meta honoured: %+v", ann)
	}
}
