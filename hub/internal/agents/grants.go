// Package agents is the agent orchestrator. This file holds the one piece of it
// that is pure: deciding whether a grant set permits a server (spec.md 5.4).
package agents

import "github.com/AkosPapp/mcp-switchboard/hub/internal/registry"

// Rule is one grant reduced to what resolution needs: a pattern and a verdict.
type Rule struct {
	Ref     registry.ServerRef // any field may be registry.Wildcard
	Allowed bool
}

// Resolve decides whether rules permit ref, in the two stages of 5.4 step 2:
//
//	(a) any matching rule with Allowed == false denies, whatever its
//	    specificity - a wildcard deny beats an exact allow, so a broad
//	    revocation cannot be re-opened by a narrow row left behind;
//	(b) otherwise the most specific matching allow applies, an exact field
//	    beating "*" field by field in the order label, project, server.
//
// The returned rule is the one that decided (the deny, or the winning allow),
// nil when nothing matched - which is a denial: absence of a grant is no grant.
func Resolve(rules []Rule, ref registry.ServerRef) (allowed bool, decisive *Rule) {
	var best *Rule
	for i := range rules {
		r := &rules[i]
		if !r.Ref.Matches(ref) {
			continue
		}
		if !r.Allowed {
			return false, r
		}
		if best == nil || moreSpecific(r.Ref, best.Ref) {
			best = r
		}
	}
	return best != nil, best
}

// moreSpecific orders two patterns lexicographically by (label, project,
// server), an exact field outranking a wildcard one.
func moreSpecific(a, b registry.ServerRef) bool {
	for _, pair := range [][2]string{{a.Label, b.Label}, {a.Project, b.Project}, {a.Server, b.Server}} {
		aExact, bExact := pair[0] != registry.Wildcard, pair[1] != registry.Wildcard
		if aExact != bExact {
			return aExact
		}
	}
	return false
}
