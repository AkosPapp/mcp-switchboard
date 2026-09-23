package agents

import (
	"errors"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/store"
)

// Sentinel errors the HTTP layer maps to status codes. Returned errors wrap
// these (errors.Is), with a human-readable reason appended.
var (
	// ErrNotFound: the agent, chat, message or run does not exist (404).
	ErrNotFound = store.ErrNotFound
	// ErrInvalid: the request is structurally wrong (400).
	ErrInvalid = store.ErrInvalid
	// ErrNameTaken: a live sibling already has this name (409).
	ErrNameTaken = store.ErrNameTaken
	// ErrNotPending: no approval is waiting for that run/call (404 or 409).
	ErrNotPending = errors.New("agents: no such pending approval")
	// ErrLimit: AGENT_MAX_DEPTH or AGENT_MAX_CHILDREN would be exceeded (409).
	ErrLimit = errors.New("agents: structural limit reached")
	// ErrForbidden: the acting agent may not do this (403).
	ErrForbidden = errors.New("agents: not permitted")
	// ErrConflict: well-formed but clashes with current state, e.g. deleting
	// the default or the only profile (409).
	ErrConflict = errors.New("agents: conflict")
)
