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

// ClientInfo identifies the process at the other end of a tunnel.
type ClientInfo struct {
	Name     string `json:"name"`
	Version  string `json:"version"`
	Instance string `json:"instance"`
	Label    string `json:"label"`
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
