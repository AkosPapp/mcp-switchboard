package protocol

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifest mirrors docs/protocol.json. The Python client asserts against the
// same file, which is what replaced byte-identity between the two copies once
// one of them stopped being Python (spec.md P1/P2).
type manifest struct {
	ProtocolVersion int      `json:"protocolVersion"`
	TunnelPath      string   `json:"tunnelPath"`
	NameSeparator   string   `json:"nameSeparator"`
	ServerStates    []string `json:"serverStates"`
	Frames          map[string]struct {
		Direction string   `json:"direction"`
		Required  []string `json:"required"`
		Optional  []string `json:"optional"`
		// ClientOptional lists optional fields of hello.client.
		ClientOptional []string `json:"clientOptional"`
	} `json:"frames"`
}

func load(t *testing.T) manifest {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		path := filepath.Join(dir, "docs", "protocol.json")
		if data, err := os.ReadFile(path); err == nil {
			var m manifest
			if err := json.Unmarshal(data, &m); err != nil {
				t.Fatalf("docs/protocol.json is not valid JSON: %v", err)
			}
			return m
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("not running from a source checkout")
		}
		dir = parent
	}
}

func TestConstantsMatchManifest(t *testing.T) {
	m := load(t)
	if Version != m.ProtocolVersion {
		t.Errorf("Version = %d, manifest says %d", Version, m.ProtocolVersion)
	}
	if Path != m.TunnelPath {
		t.Errorf("Path = %q, manifest says %q", Path, m.TunnelPath)
	}
	if NameSeparator != m.NameSeparator {
		t.Errorf("NameSeparator = %q, manifest says %q", NameSeparator, m.NameSeparator)
	}
}

func TestServerStatesMatchManifest(t *testing.T) {
	m := load(t)
	declared := map[string]bool{
		StateStarting: true, StateRunning: true, StateExited: true, StateFailed: true,
	}
	if len(declared) != len(m.ServerStates) {
		t.Fatalf("declared %d states, manifest has %d", len(declared), len(m.ServerStates))
	}
	for _, state := range m.ServerStates {
		if !declared[state] {
			t.Errorf("manifest state %q has no constant", state)
		}
	}
}

func TestFrameTypesMatchManifest(t *testing.T) {
	m := load(t)
	declared := map[string]bool{
		TypeHello: true, TypeHelloAck: true, TypeMCP: true,
		TypeServerState: true, TypeRestart: true, TypeError: true,
	}
	if len(declared) != len(m.Frames) {
		t.Fatalf("declared %d frame types, manifest has %d", len(declared), len(m.Frames))
	}
	for name := range m.Frames {
		if !declared[name] {
			t.Errorf("manifest frame %q has no constant", name)
		}
	}
}

// Every builder is called with all optional fields populated, so a builder that
// silently dropped one would fail here rather than in production.
func TestBuildersEmitDeclaredFieldsOnly(t *testing.T) {
	m := load(t)
	built := map[string]map[string]any{
		TypeHelloAck: HelloAck("cid", "hub", "0.0.0"),
		TypeMCP:      MCP("git", json.RawMessage(`{"jsonrpc":"2.0"}`)),
		TypeRestart:  Restart("git"),
		TypeError:    ErrorFrame("bad", "git"),
	}

	for frameType, frame := range built {
		declared, ok := m.Frames[frameType]
		if !ok {
			t.Fatalf("built a %q frame the manifest does not declare", frameType)
		}
		allowed := map[string]bool{}
		for _, f := range declared.Required {
			allowed[f] = true
		}
		for _, f := range declared.Optional {
			allowed[f] = true
		}
		for field := range frame {
			if !allowed[field] {
				t.Errorf("%s emits undeclared field %q", frameType, field)
			}
		}
		for _, field := range declared.Required {
			if value, ok := frame[field]; !ok || value == nil {
				t.Errorf("%s is missing required field %q", frameType, field)
			}
		}
	}

	// hello and server_state are client->hub: the hub decodes them rather than
	// building them, so they are covered by the decode test below instead.
	for frameType, declared := range m.Frames {
		if _, built := built[frameType]; built {
			continue
		}
		if declared.Direction == "hub->client" || declared.Direction == "both" {
			t.Errorf("%s can be sent by the hub but has no builder", frameType)
		}
	}
}

func TestDecodeReadsEveryClientFrame(t *testing.T) {
	cases := map[string]string{
		TypeHello: `{"type":"hello","protocol":1,"client":{"name":"c","version":"1",
			"instance":"i","label":"lab"},"servers":[{"name":"git","project":"p"}]}`,
		TypeServerState: `{"type":"server_state","server":"git","state":"exited","exitCode":1}`,
		TypeMCP:         `{"type":"mcp","server":"git","payload":{"jsonrpc":"2.0"}}`,
		TypeError:       `{"type":"error","message":"boom","server":"git"}`,
	}
	for want, raw := range cases {
		frame, err := Decode([]byte(raw))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if frame.Type != want {
			t.Errorf("decoded type = %q, want %q", frame.Type, want)
		}
	}
}

func TestDecodeRejectsJunk(t *testing.T) {
	if _, err := Decode([]byte("not json")); err == nil {
		t.Error("expected an error for a non-JSON frame")
	}
	if _, err := Decode([]byte(`{"server":"git"}`)); err == nil {
		t.Error("expected an error for a frame with no type")
	}
}

func TestValidateName(t *testing.T) {
	for _, name := range []string{"git", "my-server", "a.b", "projekt"} {
		if err := ValidateName(name, "server"); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "a__b", "__", "x__"} {
		if err := ValidateName(name, "server"); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}

func TestManifestDeclaresOptionalClientEnvironment(t *testing.T) {
	m := load(t)
	found := false
	for _, f := range m.Frames[TypeHello].ClientOptional {
		if f == "environment" {
			found = true
		}
	}
	if !found {
		t.Fatal("manifest hello.clientOptional does not declare environment")
	}
	// ClientInfo must carry every field the manifest declares.
	raw, err := json.Marshal(ClientInfo{Environment: &ClientEnvironment{Kinds: []string{}}})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	for _, f := range m.Frames[TypeHello].ClientOptional {
		if _, ok := out[f]; !ok {
			t.Errorf("ClientInfo does not carry declared optional field %q", f)
		}
	}
}

func helloWith(t *testing.T, client string) *ClientInfo {
	t.Helper()
	frame, err := Decode([]byte(`{"type":"hello","protocol":1,"client":` + client + `,"servers":[]}`))
	if err != nil {
		t.Fatalf("hello must decode: %v", err)
	}
	return frame.Client
}

func TestHelloWithoutEnvironment(t *testing.T) {
	c := helloWith(t, `{"name":"c","version":"1","instance":"i","label":"lab"}`)
	if c.Environment != nil || c.Label != "lab" {
		t.Errorf("client = %+v", c)
	}
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "environment") {
		t.Errorf("absent environment must stay absent: %s", raw)
	}
}

func TestEnvironmentDecodesUnknownKindsAndRoundTrips(t *testing.T) {
	c := helloWith(t, `{"label":"lab","environment":{"kinds":["devcontainer","future-thing"],
		"project":"proj","workspace":"/w","details":{"nixShell":"pure"},"extra":1}}`)
	env := c.Environment
	if env == nil || len(env.Kinds) != 2 || env.Kinds[1] != "future-thing" ||
		env.Project != "proj" || env.Workspace != "/w" || env.Details["nixShell"] != "pure" {
		t.Fatalf("environment = %+v", env)
	}
	raw, _ := json.Marshal(c)
	var out struct {
		Environment map[string]any `json:"environment"`
	}
	_ = json.Unmarshal(raw, &out)
	for _, k := range []string{"kinds", "project", "workspace", "details"} {
		if _, ok := out.Environment[k]; !ok {
			t.Errorf("missing %s in %s", k, raw)
		}
	}
}

func TestMalformedEnvironmentIsDroppedNotRejected(t *testing.T) {
	for _, env := range []string{`"nope"`, `[1]`, `{"kinds":"x"}`, `{"kinds":[1,2]}`, `{"details":{"a":1}}`, `null`, `{"project":5}`} {
		c := helloWith(t, `{"label":"lab","environment":`+env+`}`)
		if c.Environment != nil {
			t.Errorf("environment %s should be dropped, got %+v", env, c.Environment)
		}
		if c.Label != "lab" {
			t.Errorf("label lost for %s", env)
		}
	}
}

func TestEnvironmentSizeCaps(t *testing.T) {
	long := strings.Repeat("x", 1000)
	details := map[string]string{}
	for i := 0; i < 40; i++ {
		details[fmt.Sprintf("k%02d", i)] = long
	}
	kinds := []string{}
	for i := 0; i < 20; i++ {
		kinds = append(kinds, fmt.Sprintf("kind%d", i))
	}
	body, _ := json.Marshal(map[string]any{"label": "l", "environment": map[string]any{
		"kinds": kinds, "project": long, "workspace": long, "details": details}})
	env := helloWith(t, string(body)).Environment
	if env == nil {
		t.Fatal("environment dropped")
	}
	if len(env.Kinds) != MaxEnvKinds || len(env.Details) != MaxEnvDetails {
		t.Errorf("kinds=%d details=%d", len(env.Kinds), len(env.Details))
	}
	if len(env.Project) != MaxEnvStringSize || len(env.Workspace) != MaxEnvStringSize {
		t.Errorf("project/workspace not capped")
	}
	for k, v := range env.Details {
		if len(k) > MaxEnvStringSize || len(v) != MaxEnvStringSize {
			t.Errorf("detail %q not capped", k)
		}
	}
}
