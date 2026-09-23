package agents

import (
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

func ref(l, p, s string) registry.ServerRef {
	return registry.ServerRef{Label: l, Project: p, Server: s}
}

func allow(l, p, s string) Rule { return Rule{Ref: ref(l, p, s), Allowed: true} }
func deny(l, p, s string) Rule  { return Rule{Ref: ref(l, p, s), Allowed: false} }

func TestResolve(t *testing.T) {
	target := ref("laptop", "web", "fs")
	cases := []struct {
		name    string
		rules   []Rule
		want    bool
		decided *Rule // expected decisive rule, nil when none
	}{
		{"no rules denies", nil, false, nil},
		{"exact allow", []Rule{allow("laptop", "web", "fs")}, true, ptr(allow("laptop", "web", "fs"))},
		{"non-matching allow", []Rule{allow("laptop", "web", "git")}, false, nil},
		{"non-matching label", []Rule{allow("desktop", "web", "fs")}, false, nil},
		{"project must match, '' is not a wildcard", []Rule{allow("laptop", "", "fs")}, false, nil},
		{"full wildcard allow", []Rule{allow("*", "*", "*")}, true, ptr(allow("*", "*", "*"))},
		{"wildcard server only", []Rule{allow("laptop", "web", "*")}, true, ptr(allow("laptop", "web", "*"))},
		{"exact deny", []Rule{deny("laptop", "web", "fs")}, false, ptr(deny("laptop", "web", "fs"))},
		{"wildcard deny beats exact allow",
			[]Rule{allow("laptop", "web", "fs"), deny("*", "*", "*")}, false, ptr(deny("*", "*", "*"))},
		{"exact deny beats wildcard allow",
			[]Rule{allow("*", "*", "*"), deny("laptop", "web", "fs")}, false, ptr(deny("laptop", "web", "fs"))},
		{"deny wins regardless of order",
			[]Rule{deny("laptop", "*", "*"), allow("laptop", "web", "fs")}, false, ptr(deny("laptop", "*", "*"))},
		{"deny elsewhere does not matter",
			[]Rule{allow("*", "*", "*"), deny("laptop", "web", "git")}, true, ptr(allow("*", "*", "*"))},
		{"most specific allow decides: exact label beats wildcard label",
			[]Rule{allow("*", "web", "fs"), allow("laptop", "*", "*")}, true, ptr(allow("laptop", "*", "*"))},
		{"project outranks server when labels tie",
			[]Rule{allow("laptop", "*", "fs"), allow("laptop", "web", "*")}, true, ptr(allow("laptop", "web", "*"))},
		{"more exact fields win",
			[]Rule{allow("*", "*", "*"), allow("laptop", "web", "*"), allow("laptop", "*", "*")}, true,
			ptr(allow("laptop", "web", "*"))},
		{"deny for a different ref does not block", []Rule{deny("other", "x", "y"), allow("laptop", "web", "fs")}, true,
			ptr(allow("laptop", "web", "fs"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, decisive := Resolve(tc.rules, target)
			if got != tc.want {
				t.Fatalf("Resolve = %v, want %v", got, tc.want)
			}
			switch {
			case tc.decided == nil && decisive != nil:
				t.Fatalf("decisive = %+v, want nil", *decisive)
			case tc.decided != nil && (decisive == nil || *decisive != *tc.decided):
				t.Fatalf("decisive = %v, want %+v", decisive, *tc.decided)
			}
		})
	}
}

func TestResolveProjectless(t *testing.T) {
	target := ref("laptop", "", "fs")
	if ok, _ := Resolve([]Rule{allow("laptop", "", "fs")}, target); !ok {
		t.Error("exact projectless grant should allow")
	}
	if ok, _ := Resolve([]Rule{allow("laptop", "*", "fs")}, target); !ok {
		t.Error("wildcard project should cover a projectless server")
	}
	if ok, _ := Resolve([]Rule{allow("laptop", "web", "fs")}, target); ok {
		t.Error("a project-scoped grant must not cover a projectless server")
	}
}

// A '*' on the concrete side is a server literally named '*', not a licence.
func TestResolveConcreteWildcardIsLiteral(t *testing.T) {
	if ok, _ := Resolve([]Rule{allow("laptop", "web", "fs")}, ref("laptop", "web", "*")); ok {
		t.Error("exact grant matched a concrete ref containing '*'")
	}
}

func ptr(r Rule) *Rule { return &r }
