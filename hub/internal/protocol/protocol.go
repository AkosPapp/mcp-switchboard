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
	"regexp"
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
	TypeHello         = "hello"
	TypeHelloAck      = "hello_ack"
	TypeMCP           = "mcp"
	TypeServerState   = "server_state"
	TypeRestart       = "restart"
	TypeError         = "error"
	TypeContextUpdate = "context_update"
	TypeSkillsUpdate  = "skills_update"
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

// Limits on client instruction files (hello.client.instructions and the
// context_update frame). The client is untrusted input that lands verbatim in
// model context, so the counts and sizes are capped hub-side no matter what
// the client sent.
const (
	MaxInstructionFiles      = 8
	MaxInstructionFileRunes  = 32 * 1024
	MaxInstructionTotalRunes = 64 * 1024
	MaxInstructionPathRunes  = 256
)

// MaxEnvironmentBriefRunes bounds the client host brief (hello's
// client.environment_brief and the context_update frame): free text the agent
// sees verbatim, capped so a broken client cannot flood the system prompt.
const MaxEnvironmentBriefRunes = 8 * 1024

// SanitizeEnvironmentBrief normalises line endings and rune-caps an untrusted
// environment brief. It never fails; junk just shrinks. Newlines are kept on
// purpose — the brief IS a multi-line text block (unlike instruction paths,
// which are single-line labels).
func SanitizeEnvironmentBrief(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return capRunes(strings.TrimSpace(s), MaxEnvironmentBriefRunes)
}

// InstructionFile is one repository instruction file (AGENTS.md, CLAUDE.md, ...)
// read from the client host. Path is host-relative-ish display text: the hub
// never opens it. Content is injected into model context verbatim (W1 merged is
// what makes multi-line injection survive).
type InstructionFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// SanitizeInstructions bounds an untrusted instruction list: empty paths and
// bodies are dropped, paths are collapsed to a single printable line (they are
// injected as labels, so a newline inside one would forge section headers),
// bodies are rune-capped, and the set is capped at MaxInstructionFiles entries
// and MaxInstructionTotalRunes total. It never fails: junk just shrinks.
func SanitizeInstructions(in []InstructionFile) []InstructionFile {
	out := make([]InstructionFile, 0, len(in))
	total := 0
	for _, f := range in {
		if len(out) >= MaxInstructionFiles {
			break
		}
		path := capRunes(strings.Join(strings.Fields(f.Path), " "), MaxInstructionPathRunes)
		body := capRunes(f.Content, MaxInstructionFileRunes)
		if path == "" || body == "" {
			continue
		}
		n := len([]rune(body))
		if total+n > MaxInstructionTotalRunes {
			break
		}
		total += n
		out = append(out, InstructionFile{Path: path, Content: body})
	}
	return out
}

// Limits on skills scanned on the client host (hello.client.skills and the
// skills_update frame). Same discipline as instructions: untrusted input,
// counts and sizes capped hub-side, junk dropped rather than refused.
const (
	MaxSkillFiles      = 200
	MaxSkillFileRunes  = 64 * 1024
	MaxSkillTotalRunes = 256 * 1024
	MaxSkillNameRunes  = 64
	MaxSkillPathRunes  = 256
)

var skillNameRX = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// SkillFile is one SKILL.md found on a client host. Name is validated against
// the skill-name grammar (and the client guarantees it equals the directory
// name); Path and Source are display labels, collapsed to single lines;
// Content is the raw SKILL.md text the hub can store and show.
type SkillFile struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Source      string `json:"source"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

// SanitizeSkills bounds an untrusted skill list: invalid names and empty
// bodies are dropped, fields are rune-capped, and the set is capped at
// MaxSkillFiles entries and MaxSkillTotalRunes of body content. It never
// fails: junk just shrinks. Earlier entries win a name clash.
func SanitizeSkills(in []SkillFile) []SkillFile {
	out := make([]SkillFile, 0, len(in))
	seen := map[string]bool{}
	total := 0
	for _, s := range in {
		if len(out) >= MaxSkillFiles {
			break
		}
		name := capRunes(strings.TrimSpace(s.Name), MaxSkillNameRunes)
		if !skillNameRX.MatchString(name) || seen[name] {
			continue
		}
		body := capRunes(s.Content, MaxSkillFileRunes)
		if body == "" {
			continue
		}
		n := len([]rune(body))
		if total+n > MaxSkillTotalRunes {
			break
		}
		total += n
		seen[name] = true
		out = append(out, SkillFile{
			Name:        name,
			Path:        capRunes(strings.Join(strings.Fields(s.Path), " "), MaxSkillPathRunes),
			Source:      capRunes(strings.Join(strings.Fields(s.Source), " "), MaxSkillPathRunes),
			Description: capRunes(strings.Join(strings.Fields(s.Description), " "), 1024),
			Content:     body,
		})
	}
	return out
}

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
//
// Instructions is the hello-time snapshot of the client host's instruction
// files; the live set lives on registry.Connection, because a later
// context_update frame replaces it without a reconnect.
type ClientInfo struct {
	Name             string             `json:"name"`
	Version          string             `json:"version"`
	Instance         string             `json:"instance"`
	Label            string             `json:"label"`
	Environment      *ClientEnvironment `json:"environment,omitempty"`
	Instructions     []InstructionFile  `json:"instructions,omitempty"`
	EnvironmentBrief string             `json:"environment_brief,omitempty"`
	Skills           []SkillFile        `json:"skills,omitempty"`
}

// UnmarshalJSON decodes the client object, treating environment, instructions
// and environment_brief as strictly optional: a malformed one of them is
// logged and dropped, never a reason to refuse the hello.
func (c *ClientInfo) UnmarshalJSON(data []byte) error {
	type plain struct {
		Name             string          `json:"name"`
		Version          string          `json:"version"`
		Instance         string          `json:"instance"`
		Label            string          `json:"label"`
		Environment      json.RawMessage `json:"environment"`
		Instructions     json.RawMessage `json:"instructions"`
		EnvironmentBrief json.RawMessage `json:"environment_brief"`
		Skills           json.RawMessage `json:"skills"`
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
	if len(p.Instructions) > 0 && string(p.Instructions) != "null" {
		files, err := parseInstructions(p.Instructions)
		if err != nil {
			slog.Warn("dropping malformed hello.client.instructions", "error", err)
		} else {
			c.Instructions = files
		}
	}
	if len(p.EnvironmentBrief) > 0 && string(p.EnvironmentBrief) != "null" {
		var brief string
		if err := json.Unmarshal(p.EnvironmentBrief, &brief); err != nil {
			slog.Warn("dropping malformed hello.client.environment_brief", "error", err)
		} else {
			c.EnvironmentBrief = SanitizeEnvironmentBrief(brief)
		}
	}
	if len(p.Skills) > 0 && string(p.Skills) != "null" {
		var in []SkillFile
		if err := json.Unmarshal(p.Skills, &in); err != nil {
			slog.Warn("dropping malformed hello.client.skills", "error", err)
		} else {
			c.Skills = SanitizeSkills(in)
		}
	}
	return nil
}

func parseInstructions(raw json.RawMessage) ([]InstructionFile, error) {
	var in []InstructionFile
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	return SanitizeInstructions(in), nil
}

func capString(s string) string { return capRunes(s, MaxEnvStringSize) }

func capRunes(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max])
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

	// context_update (client->hub): the live replacement for hello's
	// client.instructions and/or client.environment_brief. A nil EnvironmentBrief
	// means the field was absent (leave the hub-side copy alone); a non-nil one
	// replaces it, empty string clears it.
	Instructions     []InstructionFile `json:"instructions,omitempty"`
	EnvironmentBrief *string           `json:"environment_brief,omitempty"`

	// skills_update (client->hub): the live replacement for hello's
	// client.skills. The list is always wholesale; absent means the same frame
	// type was not sent, and an empty list clears the hub-side copy.
	Skills []SkillFile `json:"skills,omitempty"`

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
