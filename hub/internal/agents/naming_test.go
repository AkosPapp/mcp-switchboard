package agents

import (
	"testing"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

func TestSoleClient(t *testing.T) {
	g := func(label string, allowed bool) store.Grant {
		return store.Grant{Label: label, Project: "*", Server: "*", Allowed: allowed}
	}
	for name, tc := range map[string]struct {
		grants []store.Grant
		want   string
		pinned bool
	}{
		"none":              {nil, "", false},
		"one client":        {[]store.Grant{g("box", true)}, "box", true},
		"same client twice": {[]store.Grant{g("box", true), g("box", true)}, "box", true},
		"wildcard label":    {[]store.Grant{g("*", true)}, "", false},
		"two clients":       {[]store.Grant{g("a", true), g("b", true)}, "", false},
		"deny is ignored":   {[]store.Grant{g("box", true), g("other", false)}, "box", true},
		"only denies":       {[]store.Grant{g("box", false)}, "", false},
	} {
		got, pinned := soleClient(tc.grants)
		if got != tc.want || pinned != tc.pinned {
			t.Errorf("%s: soleClient = %q, %v; want %q, %v", name, got, pinned, tc.want, tc.pinned)
		}
	}
}

func TestAgentToolName(t *testing.T) {
	for name, tc := range map[string]struct {
		pinned                       bool
		label, project, server, tool string
		want                         string
	}{
		"pinned harness is bare":               {true, "legion5", "", "harness", "run_command", "run_command"},
		"pinned other server keeps its prefix": {true, "legion5", "", "filesystem", "list_directory", "filesystem__list_directory"},
		"pinned project server":                {true, "legion5", "proj", "git", "git_status", "proj__git__git_status"},
		"pinned harness in a project keeps it": {true, "legion5", "proj", "harness", "run_command", "proj__harness__run_command"},
		"unpinned is fully qualified":          {false, "legion5", "", "harness", "run_command", "legion5__harness__run_command"},
	} {
		got, ok := agentToolName(tc.pinned, tc.label, tc.project, tc.server, tc.tool)
		if !ok || got != tc.want {
			t.Errorf("%s: got %q (%v), want %q", name, got, ok, tc.want)
		}
	}
}
