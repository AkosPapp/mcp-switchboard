// Package protocol carries tunnel protocol v1: the frames exchanged with
// mcp-switchboard-client over one outbound WebSocket.
//
// docs/PROTOCOL.md is the prose source of truth and docs/protocol.json its
// machine-readable projection. This file is checked against that manifest by
// protocol_test.go, and the Python client is checked against the same manifest
// by client/tests/test_protocol_conformance.py. Neither language holds the
// definition; both answer to the file (spec.md P1/P2).
package protocol

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

const (
	// Version is the integer sent in hello.protocol. A hub rejects a version it
	// does not implement rather than guessing at the frames.
	Version = 1

	// Path is the only route the tunnel listener serves besides /health.
	Path = "/tunnel/v1"

	// NameSeparator joins label, project, server and tool into the name a
	// consumer sees, which is why none of those may contain it.
	NameSeparator = "__"
)

// Frame types.
const (
	TypeHello       = "hello"
	TypeHelloAck    = "hello_ack"
	TypeMCP         = "mcp"
	TypeServerState = "server_state"
	TypeRestart     = "restart"
	TypeError       = "error"
)

// Server lifecycle states, as reported by the client.
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateExited   = "exited"
	StateFailed   = "failed"
)

// Error is a frame that cannot be acted on.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

// ValidateName rejects a label, project or server name the hub cannot compose a
// tool name from. Enforced on the hub side too, never only on the client's:
// a name containing the separator would make the composed name ambiguous, and
// the client is not a trusted input.
func ValidateName(name, kind string) error {
	if name == "" {
		return errf("%s must not be empty", kind)
	}
	if strings.Contains(name, NameSeparator) {
		return errf("%s %q must not contain %q", kind, name, NameSeparator)
	}
	return nil
}

// Limits on hello.client.environment. The client is not a trusted input and the
// environment is shown to people, so its size is bounded rather than trusted.
const (
	MaxEnvKinds      = 8
	MaxEnvDetails    = 16
	MaxEnvStringSize = 256
)

// ClientEnvironment describes where a client runs (dev container, direnv, nix
// shell, ...). Kinds are open strings: an unknown kind from a newer client is
// preserved, not rejected.
type ClientEnvironment struct {
	Kinds     []string          `json:"kinds"`
	Project   string            `json:"project,omitempty"`
	Workspace string            `json:"workspace,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

// ClientInfo identifies the process at the other end of a tunnel.
type ClientInfo struct {
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Instance    string             `json:"instance"`
	Label       string             `json:"label"`
	Environment *ClientEnvironment `json:"environment,omitempty"`
}

// UnmarshalJSON decodes the client object, treating environment as strictly
// optional: a malformed one is logged and dropped, never a reason to refuse the
// hello.
func (c *ClientInfo) UnmarshalJSON(data []byte) error {
	type plain struct {
		Name        string          `json:"name"`
		Version     string          `json:"version"`
		Instance    string          `json:"instance"`
		Label       string          `json:"label"`
		Environment json.RawMessage `json:"environment"`
	}
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*c = ClientInfo{Name: p.Name, Version: p.Version, Instance: p.Instance, Label: p.Label}
	if len(p.Environment) > 0 && string(p.Environment) != "null" {
		env, err := parseEnvironment(p.Environment)
		if err != nil {
			slog.Warn("dropping malformed hello.client.environment", "error", err)
		} else {
			c.Environment = env
		}
	}
	return nil
}

func capString(s string) string {
	if r := []rune(s); len(r) > MaxEnvStringSize {
		return string(r[:MaxEnvStringSize])
	}
	return s
}

func parseEnvironment(raw json.RawMessage) (*ClientEnvironment, error) {
	var in struct {
		Kinds     []string          `json:"kinds"`
		Project   string            `json:"project"`
		Workspace string            `json:"workspace"`
		Details   map[string]string `json:"details"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	env := &ClientEnvironment{
		Kinds:     []string{},
		Project:   capString(in.Project),
		Workspace: capString(in.Workspace),
	}
	for _, kind := range in.Kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" || len(env.Kinds) >= MaxEnvKinds {
			continue
		}
		env.Kinds = append(env.Kinds, capString(kind))
	}
	if len(in.Details) > 0 {
		keys := make([]string, 0, len(in.Details))
		for k := range in.Details {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		env.Details = map[string]string{}
		for _, k := range keys {
			if len(env.Details) >= MaxEnvDetails {
				break
			}
			env.Details[capString(k)] = capString(in.Details[k])
		}
	}
	return env, nil
}

// HubInfo identifies this hub to the client.
type HubInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerDecl is one entry of hello.servers: a server the client is running.
// Command is descriptive only; the hub never executes it.
type ServerDecl struct {
	Name    string `json:"name"`
	Command string `json:"command,omitempty"`
	Project string `json:"project,omitempty"`
}

// Frame is any tunnel frame, decoded loosely enough that an unknown type can be
// logged and skipped instead of killing the connection.
type Frame struct {
	Type string `json:"type"`

	// hello
	Protocol int          `json:"protocol,omitempty"`
	Client   *ClientInfo  `json:"client,omitempty"`
	Servers  []ServerDecl `json:"servers,omitempty"`

	// hello_ack
	ConnectionID string   `json:"connectionId,omitempty"`
	Hub          *HubInfo `json:"hub,omitempty"`

	// mcp, server_state, restart, error
	Server   string          `json:"server,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	State    string          `json:"state,omitempty"`
	ExitCode *int            `json:"exitCode,omitempty"`
	Message  string          `json:"message,omitempty"`
	ErrorMsg string          `json:"error,omitempty"`
}

// Decode parses one text frame off the wire.
func Decode(data []byte) (*Frame, error) {
	var f Frame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, errf("frame is not valid JSON: %v", err)
	}
	if f.Type == "" {
		return nil, errf("frame has no type")
	}
	return &f, nil
}

// HelloAck acknowledges a hello and hands the client its connection id.
func HelloAck(connectionID, hubName, hubVersion string) map[string]any {
	return map[string]any{
		"type":         TypeHelloAck,
		"connectionId": connectionID,
		"hub":          map[string]any{"name": hubName, "version": hubVersion},
	}
}

// MCP wraps one MCP message for a named server channel. payload is passed
// through verbatim: the hub speaks MCP, but this frame is only an envelope.
func MCP(server string, payload json.RawMessage) map[string]any {
	return map[string]any{"type": TypeMCP, "server": server, "payload": payload}
}

// Restart asks the client to stop and respawn one server.
func Restart(server string) map[string]any {
	return map[string]any{"type": TypeRestart, "server": server}
}

// ErrorFrame is sent before closing a connection the hub cannot serve. server is
// optional and omitted when the fault is not specific to one.
func ErrorFrame(message, server string) map[string]any {
	frame := map[string]any{"type": TypeError, "message": message}
	if server != "" {
		frame["server"] = server
	}
	return frame
}
