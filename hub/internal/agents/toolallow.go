package agents

import (
	"fmt"
	"path"
	"strings"
)

// Per-agent tool allowlists: glob patterns on the UPSTREAM tool name that
// narrow what an agent's per-server grants allow. Empty means no narrowing.
// switchboard.* hub tools are never subject to it.

// toolAllowed reports whether upstream passes the allowlist.
func toolAllowed(allow []string, upstream string) bool {
	if len(allow) == 0 {
		return true
	}
	for _, p := range allow {
		if ok, err := path.Match(p, upstream); err == nil && ok {
			return true
		}
	}
	return false
}

// normalizeToolAllow trims, drops blanks and duplicates, and rejects patterns
// that are not valid globs.
func normalizeToolAllow(in []string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, p := range in {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if _, err := path.Match(p, ""); err != nil {
			return nil, fmt.Errorf("%w: allowed_tools pattern %q is not a valid glob: %v", ErrInvalid, p, err)
		}
		seen[p] = true
		out = append(out, p)
	}
	return out, nil
}

// deriveToolAllow computes a child's effective allowlist. A child can only
// narrow: with no request it inherits the parent's; when the parent has an
// allowlist, every requested pattern must be within it (equal to, or matched
// as a literal by, one of the parent's patterns).
func deriveToolAllow(parent, requested []string) ([]string, error) {
	req, err := normalizeToolAllow(requested)
	if err != nil {
		return nil, err
	}
	if len(req) == 0 {
		return append([]string(nil), parent...), nil
	}
	if len(parent) == 0 {
		return req, nil
	}
	for _, p := range req {
		if !toolAllowed(parent, p) {
			return nil, denied("allowed_tools pattern %q is outside your own tool allowlist %v; a child can only narrow it", p, parent)
		}
	}
	return req, nil
}
