package agents

import "github.com/AkosPapp/mcp-switchboard/hub/internal/store"

// Grant patterns: each field is an exact value or "*" (A11).

type pattern struct{ Label, Project, Server string }

func patOf(g store.Grant) pattern { return pattern{g.Label, g.Project, g.Server} }

func fieldIntersect(a, b string) (string, bool) {
	switch {
	case a == store.Wildcard:
		return b, true
	case b == store.Wildcard || a == b:
		return a, true
	}
	return "", false
}

// intersect is the pattern matching exactly what both a and b match.
func intersect(a, b pattern) (pattern, bool) {
	l, ok1 := fieldIntersect(a.Label, b.Label)
	p, ok2 := fieldIntersect(a.Project, b.Project)
	s, ok3 := fieldIntersect(a.Server, b.Server)
	return pattern{l, p, s}, ok1 && ok2 && ok3
}

func fieldCovers(p, c string) bool { return p == store.Wildcard || p == c }

// covers reports whether every ref matched by c is matched by p.
func covers(p, c pattern) bool {
	return fieldCovers(p.Label, c.Label) && fieldCovers(p.Project, c.Project) && fieldCovers(p.Server, c.Server)
}

// deriveChildGrants applies A12: the child holds the parent's set intersected
// with what was requested (nil requested = ask for everything the parent has).
// Parent denies that overlap a granted intersection are carried along as
// denies, so a wildcard request cannot re-open what the parent may not do.
func deriveChildGrants(parent []store.Grant, requested []store.Grant) []store.Grant {
	var pAllow, pDeny []pattern
	for _, g := range parent {
		if g.Allowed {
			pAllow = append(pAllow, patOf(g))
		} else {
			pDeny = append(pDeny, patOf(g))
		}
	}
	type key = pattern
	out := map[key]store.Grant{}
	put := func(p pattern, allowed bool, source string) {
		if cur, ok := out[p]; ok && (!cur.Allowed || allowed) {
			return // an existing deny stays; an existing allow yields only to a deny
		}
		out[p] = store.Grant{Label: p.Label, Project: p.Project, Server: p.Server, Allowed: allowed, Source: source}
	}
	reqs := requested
	if requested == nil {
		for _, p := range pAllow {
			reqs = append(reqs, store.Grant{Label: p.Label, Project: p.Project, Server: p.Server, Allowed: true})
		}
	}
	for _, r := range reqs {
		if !r.Allowed {
			put(patOf(r), false, store.GrantInherited)
			continue
		}
		for _, p := range pAllow {
			i, ok := intersect(patOf(r), p)
			if !ok {
				continue
			}
			put(i, true, store.GrantInherited)
			for _, d := range pDeny {
				if di, ok := intersect(i, d); ok {
					put(di, false, store.GrantInherited)
				}
			}
		}
	}
	res := make([]store.Grant, 0, len(out))
	for _, g := range out {
		res = append(res, g)
	}
	return res
}

// holdsHarness is W1: does the grant set (at creation) include a `harness`
// server (directly or through a wildcard)?
func holdsHarness(grants []store.Grant) bool {
	for _, g := range grants {
		if g.Allowed && (g.Server == "harness" || g.Server == store.Wildcard) {
			return true
		}
	}
	return false
}
