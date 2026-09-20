package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
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
