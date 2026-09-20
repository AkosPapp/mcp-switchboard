package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture is shared with the Python client (spec.md V4). Both suites run
// every case, so "does an empty file substitute to an empty string?" has one
// answer across the two languages rather than two that drift.
type fixture struct {
	Prefix string            `json:"prefix"`
	Files  map[string]string `json:"files"`
	Dirs   []string          `json:"dirs"`
	Cases  []struct {
		Name string            `json:"name"`
		Env  map[string]string `json:"env"`
		Get  struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Default any    `json:"default"`
			Secret  bool   `json:"secret"`
		} `json:"get"`
		Expect any `json:"expect"`
	} `json:"cases"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		path := filepath.Join(dir, "docs", "envconf-cases.json")
		if data, err := os.ReadFile(path); err == nil {
			var f fixture
			if err := json.Unmarshal(data, &f); err != nil {
				t.Fatalf("docs/envconf-cases.json is not valid JSON: %v", err)
			}
			return f
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("not running from a source checkout")
		}
		dir = parent
	}
}

func TestPrefixMatchesFixture(t *testing.T) {
	if f := loadFixture(t); Prefix != f.Prefix {
		t.Errorf("Prefix = %q, fixture says %q", Prefix, f.Prefix)
	}
}

func TestSharedCases(t *testing.T) {
	f := loadFixture(t)
	root := t.TempDir()
	for name, content := range f.Files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range f.Dirs {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	expand := func(s string) string { return strings.ReplaceAll(s, "{{dir}}", root) }

	for _, tc := range f.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			// t.Setenv un-sets on cleanup, but a variable a previous case set is
			// already gone by then; clearing the namespace keeps cases independent
			// whatever order they run in.
			for _, entry := range os.Environ() {
				if key, _, _ := strings.Cut(entry, "="); strings.HasPrefix(key, Prefix) {
					t.Setenv(key, "")
					os.Unsetenv(key)
				}
			}
			for key, value := range tc.Env {
				t.Setenv(key, expand(value))
			}

			var got any
			var err error
			switch tc.Get.Kind {
			case "str":
				def, _ := tc.Get.Default.(string)
				got, err = Get(tc.Get.Name, def, tc.Get.Secret)
			case "int":
				def, _ := tc.Get.Default.(float64)
				got, err = GetInt(tc.Get.Name, int(def))
			case "float":
				def, _ := tc.Get.Default.(float64)
				got, err = GetFloat(tc.Get.Name, def)
			case "bool":
				def, _ := tc.Get.Default.(bool)
				got, err = GetBool(tc.Get.Name, def)
			default:
				t.Fatalf("fixture asks for an unknown kind %q", tc.Get.Kind)
			}

			if want, isMap := tc.Expect.(map[string]any); isMap {
				if wantErr, _ := want["error"].(bool); wantErr {
					if err == nil {
						t.Fatalf("expected an error, got %#v", got)
					}
					return
				}
				t.Fatalf("fixture case %q has an object expectation that is not an error", tc.Name)
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			want := tc.Expect
			if s, ok := want.(string); ok {
				want = expand(s)
			}
			if number, ok := want.(float64); ok {
				switch tc.Get.Kind {
				case "int":
					want = int(number)
				case "float":
					want = number
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("got %#v, want %#v", got, want)
			}
		})
	}
}

func TestLoadEnvFileDoesNotOverrideRealEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	body := "# a comment\n\nMCP_SWITCHBOARD_LABEL=from-file\nMCP_SWITCHBOARD_TAKEN=from-file\nnonsense\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_SWITCHBOARD_TAKEN", "from-env")
	os.Unsetenv("MCP_SWITCHBOARD_LABEL")
	t.Cleanup(func() { os.Unsetenv("MCP_SWITCHBOARD_LABEL") })

	if err := LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("MCP_SWITCHBOARD_LABEL"); got != "from-file" {
		t.Errorf("LABEL = %q, want the file's value", got)
	}
	if got := os.Getenv("MCP_SWITCHBOARD_TAKEN"); got != "from-env" {
		t.Errorf("TAKEN = %q, want the environment to win", got)
	}
}

func TestLoadEnvFileIgnoresAMissingFile(t *testing.T) {
	if err := LoadEnvFile(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("a missing .env is not an error, got %v", err)
	}
}
