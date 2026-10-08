// Package tunnel serves /tunnel/v1: a client's WebSocket on one side, one MCP
// client session per tunnelled server on the other.
//
// The client is a dumb pipe carrying raw JSON-RPC for N local servers over one
// socket. For each of those servers the hub runs a real MCP ClientSession on top
// of that server's channel, which is what gives initialize/tools-list/call
// semantics, id correlation and concurrent in-flight calls without hand-rolling
// any JSON-RPC.
//
// Each server channel gets exactly one owner goroutine, which opens the session,
// registers it, parks until the server goes away, and unregisters on the way
// out. That single-owner rule is what makes the restart discipline of spec.md H4
// expressible at all.
package tunnel

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/library"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/mcpsession"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

// Metrics is the slice of the metrics surface the tunnel touches, taken as an
// interface so this package does not depend on the collector implementation.
type Metrics interface {
	CountFrame(direction string)
	ClearTools(label, server string)
	SetTools(label, server string, n int)
}

// Options configure a Handler. Zero values are replaced with the defaults.
type Options struct {
	// Token authenticates every tunnel connection. This listener is the one
	// meant to face the public internet, so it is never optional.
	Token string

	// ToolsTimeout bounds initialize and tools/list for one server. A server
	// that never answers must not hold a channel open forever.
	ToolsTimeout time.Duration

	// StopWait bounds how long shutdown waits for a channel's supervisor to
	// finish before giving up (spec.md H4).
	StopWait time.Duration

	// SettleDelay is how long a server must hold a state before the hub acts on
	// it. It is what turns a burst of restarts into one session rather than one
	// per frame; see superviseChannel.
	SettleDelay time.Duration

	// PingInterval and PingTimeout drive the server-side websocket keepalive.
	// The client never pings, and behind a proxy a dead connection can linger
	// indefinitely, so the hub probes it.
	PingInterval time.Duration
	PingTimeout  time.Duration

	HubVersion string
	Metrics    Metrics
	Logger     *slog.Logger

	// Lib, when set, receives every client's scanned skills as
	// skills/hosts/<label>/<name>/SKILL.md (hello + skills_update frames).
	// Nil leaves scanning to the console's view of the connection only.
	Lib *library.Library

	// Bus, when set, gets a TypeSkill event whenever a host's scanned set
	// changes, so the console refreshes without polling.
	Bus *events.Bus
}

// legacyProtocolVersion is the newest MCP revision that starts with initialize.
const legacyProtocolVersion = "2025-11-25"

const (
	defaultToolsTimeout = 30 * time.Second
	defaultStopWait     = 5 * time.Second
	defaultSettleDelay  = 150 * time.Millisecond
	defaultPingInterval = 20 * time.Second
	defaultPingTimeout  = 10 * time.Second
	hubName             = "mcp-switchboard-hub"
)

// Handler serves the tunnel endpoint for every connecting client.
type Handler struct {
	registry *registry.Registry
	opts     Options
	log      *slog.Logger
}

// NewHandler returns a handler bound to the live registry.
func NewHandler(reg *registry.Registry, opts Options) *Handler {
	if opts.ToolsTimeout <= 0 {
		opts.ToolsTimeout = defaultToolsTimeout
	}
	if opts.StopWait <= 0 {
		opts.StopWait = defaultStopWait
	}
	if opts.SettleDelay == 0 {
		opts.SettleDelay = defaultSettleDelay
	}
	if opts.PingInterval <= 0 {
		opts.PingInterval = defaultPingInterval
	}
	if opts.PingTimeout <= 0 {
		opts.PingTimeout = defaultPingTimeout
	}
	if opts.HubVersion == "" {
		opts.HubVersion = "0.0.0+dev"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{registry: reg, opts: opts, log: logger}
}

// authorized checks the bearer token in constant time: this endpoint is the one
// intended to be publicly reachable, so a timing oracle here would be a real
// one rather than a theoretical one.
func (h *Handler) authorized(r *http.Request) bool {
	header := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	presented := strings.TrimSpace(header[len(prefix):])
	// Compare fixed-length digests so the comparison does not leak the token's
	// length.
	a := sha256.Sum256([]byte(presented))
	b := sha256.Sum256([]byte(h.opts.Token))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(r) {
		h.log.Warn("rejecting tunnel connection: bad or missing token", "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// The client may carry a large tools/list for a server with many tools;
		// the default 32KiB read limit is too small for real catalogs.
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		h.log.Warn("websocket handshake failed", "error", err)
		return
	}
	ws.SetReadLimit(maxFrameBytes)

	conn := &clientConn{
		ws:       ws,
		registry: h.registry,
		opts:     h.opts,
		log:      h.log,
		channels: map[string]*channelOwner{},
	}
	conn.run(r.Context())
}

// maxFrameBytes bounds one tunnel frame. An MCP result can legitimately be
// large (a file read, a long diff), so this is generous; it exists to stop a
// broken or hostile client from making the hub allocate without limit.
const maxFrameBytes = 32 << 20

// clientConn is one connected client process and all of its server channels.
type clientConn struct {
	ws       *websocket.Conn
	registry *registry.Registry
	opts     Options
	log      *slog.Logger

	sendMu     sync.Mutex
	connection *registry.Connection

	// The connection's own lifetime, which every socket write is bounded by.
	// See send for why a caller's context is not used for the write itself.
	baseCtx context.Context
	// cancelRun ends run's context; used to evict this connection.
	cancelRun context.CancelFunc

	mu       sync.Mutex
	channels map[string]*channelOwner
}

// channelOwner is the handle the receive loop holds on one server's owner
// goroutine: a way to feed it, a way to stop it, and a way to know it is gone.
type channelOwner struct {
	incoming chan json.RawMessage

	// mu guards accepting, lastWarn and the drain of incoming, so a frame can
	// never slip into the buffer after a session is retired.
	mu        sync.Mutex
	accepting bool
	lastWarn  time.Time
	// desired holds the last state the client reported, latest-wins.
	desired chan string
	cancel  context.CancelFunc
	done    chan struct{}
}

// writeTimeout bounds one socket write. A peer that stops reading must not park
// a goroutine forever, but the value is generous: a large tool result is a
// legitimate frame and a slow link is not a fault.
const writeTimeout = 30 * time.Second

// send writes one frame to the client.
//
// The caller's context deliberately does NOT bound the socket write. coder/
// websocket closes the whole connection when a write's context is cancelled -
// it cannot leave a half-written frame on the wire - so passing a per-server
// context here means retiring one server channel tears down the entire tunnel
// and every other server on it. The write is instead bounded by the
// connection's own lifetime plus writeTimeout; the caller's context still
// governs everything around the write, which is where cancellation belongs.
func (c *clientConn) send(_ context.Context, frame any) error {
	raw, err := json.Marshal(frame)
	if err != nil {
		return err
	}

	base := c.baseCtx
	if base == nil {
		base = context.Background()
	}
	writeCtx, cancel := context.WithTimeout(base, writeTimeout)
	defer cancel()

	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.ws.Write(writeCtx, websocket.MessageText, raw); err != nil {
		return err
	}
	if c.opts.Metrics != nil {
		c.opts.Metrics.CountFrame("out")
	}
	return nil
}

// evict shuts this connection down because a newer one took its label. It must
// not block the evictor, so it runs on its own goroutine: first an explanatory
// error frame (a client that sees a bare socket teardown reconnects instantly
// and — with a second live client on the same label — evicts IT back, an
// eviction tennis match at the reconnect rate; the frame plus the client's
// stable-session rule turns that into a legible, backed-off retry), then the
// cancellation and the close, in that order so the reason actually reaches the
// peer. Every step is best-effort with a short bound: an evicted connection
// that cannot even say goodbye must still go away.
func (c *clientConn) evict() {
	go func() {
		dCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = c.send(dCtx, protocol.ErrorFrame("label "+fmt.Sprintf("%q", c.connection.Label)+
			" is already connected from a newer connection; this one is replaced — retry after a backoff, or stop the other client", ""))
		if c.cancelRun != nil {
			c.cancelRun()
		}
		_ = c.ws.Close(websocket.StatusPolicyViolation, "replaced by a newer connection")
	}()
}

// keepalive pings the client until the connection ends, closing it when a ping
// goes unanswered. The receive loop is what reads the pong.
func (c *clientConn) keepalive(ctx context.Context) {
	ticker := time.NewTicker(c.opts.PingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		pingCtx, cancel := context.WithTimeout(ctx, c.opts.PingTimeout)
		err := c.ws.Ping(pingCtx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("client did not answer ping; closing", "error", err)
				c.cancelRun()
				c.ws.Close(websocket.StatusGoingAway, "ping timeout")
			}
			return
		}
	}
}

// current reports whether this connection is still the registered one, i.e. it
// has not been evicted or removed. Only a current connection owns the metrics
// for its label.
func (c *clientConn) current() bool {
	return c.connection != nil && c.registry.Get(c.connection.ID) != nil
}

func (c *clientConn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.baseCtx = ctx
	c.cancelRun = cancel

	helloCtx, helloCancel := context.WithTimeout(ctx, c.opts.ToolsTimeout)
	defer helloCancel()

	_, raw, err := c.ws.Read(helloCtx)
	if err != nil {
		c.ws.Close(websocket.StatusNormalClosure, "")
		return
	}

	frame, err := protocol.Decode(raw)
	if err == nil {
		err = c.acceptHello(frame)
	}
	if err != nil {
		c.log.Warn("rejecting client", "error", err)
		_ = c.send(ctx, protocol.ErrorFrame(err.Error(), ""))
		c.ws.Close(websocket.StatusProtocolError, err.Error())
		return
	}

	if err := c.send(ctx, protocol.HelloAck(c.connection.ID, hubName, c.opts.HubVersion)); err != nil {
		return
	}
	c.connection.Evict = c.evict
	if old := c.registry.AddOrReplaceConnection(c.connection); old != nil {
		// A reconnecting client must not be locked out by its own stale
		// connection, so the newer claim on the label wins. The old teardown
		// removes by connection ID, so it cannot touch this registration; its
		// metrics are cleared here, once, before this connection opens sessions.
		if c.opts.Metrics != nil {
			for _, channel := range old.Servers() {
				c.opts.Metrics.ClearTools(old.Label, channel.Name)
			}
		}
		if old.Evict != nil {
			old.Evict()
		}
	}

	defer c.teardown()
	go c.keepalive(ctx)
	c.receiveLoop(ctx)
}

// acceptHello validates the first frame and builds the connection it describes.
// Everything here is a reason to refuse the whole client: none of it is
// recoverable, and a half-accepted client would expose a catalog nobody can
// reason about.
func (c *clientConn) acceptHello(frame *protocol.Frame) error {
	if frame.Type != protocol.TypeHello {
		return protocolErrorf("expected a %q frame first, got %q", protocol.TypeHello, frame.Type)
	}
	if frame.Protocol != protocol.Version {
		return protocolErrorf(
			"unsupported protocol version %d; this hub speaks %d", frame.Protocol, protocol.Version,
		)
	}
	if frame.Client == nil {
		return protocolErrorf("hello carried no client information")
	}

	label := strings.TrimSpace(frame.Client.Label)
	if err := protocol.ValidateName(label, "label"); err != nil {
		return err
	}
	// A label already in use is not an error here: registration evicts the older
	// connection atomically (see AddOrReplaceConnection).

	connection := registry.NewConnection(
		uuid.NewString(), label, *frame.Client, time.Now().UTC(), c.send,
	)

	seen := map[string]bool{}
	for _, decl := range frame.Servers {
		name := strings.TrimSpace(decl.Name)
		if err := protocol.ValidateName(name, "server name"); err != nil {
			return err
		}
		if seen[name] {
			// Servers are keyed by name within a connection regardless of
			// project, so accepting a duplicate would drop one entirely rather
			// than expose it under another name. Two projects that both want an
			// "lsp" need distinct server names or separate connections.
			return protocolErrorf("duplicate server name %q in hello", name)
		}
		seen[name] = true

		project := strings.TrimSpace(decl.Project)
		if project != "" {
			if err := protocol.ValidateName(project, "project name"); err != nil {
				return err
			}
		}
		decl.Name, decl.Project = name, project
		connection.AddServer(registry.NewServerChannel(connection.ID, label, decl))
	}

	if len(connection.Servers()) == 0 {
		return protocolErrorf("hello listed no servers")
	}

	c.connection = connection
	c.log = c.log.With("label", label)
	if len(frame.Client.Skills) > 0 {
		c.storeScannedSkills(frame.Client.Skills)
	}
	return nil
}

// storeScannedSkills mirrors one connection's scanned skill set into the
// library's skills/hosts/<label>/ tree (wholesale, like the frame contract)
// and tells the console it changed. Best-effort: a disk hiccup never drops a
// tunnel connection, and the live copy on the Connection object still serves
// the session that is up.
func (c *clientConn) storeScannedSkills(skills []protocol.SkillFile) {
	if c.opts.Lib == nil || c.connection == nil {
		return
	}
	items := make([]library.RawSkill, 0, len(skills))
	for _, s := range skills {
		items = append(items, library.RawSkill{Name: s.Name, Description: s.Description, Source: s.Source, Content: s.Content})
	}
	if err := c.opts.Lib.PutHostSkills(c.connection.Label, items); err != nil {
		c.log.Warn("could not store scanned skills", "error", err)
		return
	}
	if c.opts.Bus != nil {
		c.opts.Bus.Publish(events.Event{Type: events.TypeSkill})
	}
}

func (c *clientConn) receiveLoop(ctx context.Context) {
	for {
		_, raw, err := c.ws.Read(ctx)
		if err != nil {
			return
		}
		if c.opts.Metrics != nil {
			c.opts.Metrics.CountFrame("in")
		}

		frame, err := protocol.Decode(raw)
		if err != nil {
			c.log.Warn("dropping unreadable frame from client", "error", err)
			continue
		}

		switch frame.Type {
		case protocol.TypeMCP:
			c.onMCP(frame)
		case protocol.TypeServerState:
			c.onServerState(ctx, frame)
		case protocol.TypeContextUpdate:
			// The client re-read its instruction files and/or environment brief;
			// replace whichever fields the frame carries so the next turn's prompt
			// shows current state. An absent field is left alone; an empty list
			// or string clears its copy. Never a reason to drop the connection.
			if c.connection != nil {
				if frame.Instructions != nil {
					c.connection.SetInstructions(protocol.SanitizeInstructions(frame.Instructions))
				}
				if frame.EnvironmentBrief != nil {
					c.connection.SetBrief(protocol.SanitizeEnvironmentBrief(*frame.EnvironmentBrief), time.Now())
				}
			}
		case protocol.TypeSkillsUpdate:
			// The client re-scanned its SKILL.md directories. The list is
			// wholesale (an empty one clears the copies); never a reason to
			// drop the connection.
			if c.connection != nil {
				skills := protocol.SanitizeSkills(frame.Skills)
				c.connection.SetSkills(skills, time.Now())
				c.storeScannedSkills(skills)
			}
		default:
			c.log.Warn("ignoring unknown frame type", "type", frame.Type)
		}
	}
}

// onMCP hands one payload to the owner goroutine for that server.
//
// A frame for a server with no open session is normal during startup and
// shutdown races - the session either has not opened yet or has already
// retired - so it is dropped quietly rather than logged as a fault.
func (c *clientConn) onMCP(frame *protocol.Frame) {
	c.mu.Lock()
	owner := c.channels[frame.Server]
	c.mu.Unlock()
	if owner == nil {
		return
	}

	owner.deliver(frame.Payload, c.log, frame.Server)
}

// deliver hands a payload to the session without ever blocking: the receive
// loop carries every server on the tunnel, so one stalled reader must not stall
// them all. Frames arriving while no session is open belong to a dead process
// and are dropped; frames that do not fit are dropped with a rate-limited
// warning.
func (o *channelOwner) deliver(payload json.RawMessage, log *slog.Logger, server string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.accepting {
		return
	}
	select {
	case o.incoming <- payload:
	default:
		if now := time.Now(); now.Sub(o.lastWarn) >= 5*time.Second {
			o.lastWarn = now
			log.Warn("dropping mcp frame: session buffer full", "server", server)
		}
	}
}

// startAccepting flushes stale frames and opens the channel to new ones.
func (o *channelOwner) startAccepting() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainLocked()
	o.accepting = true
}

// stopAccepting closes the channel to new frames and discards buffered ones.
func (o *channelOwner) stopAccepting() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.accepting = false
	o.drainLocked()
}

func (o *channelOwner) drainLocked() {
	for {
		select {
		case <-o.incoming:
		default:
			return
		}
	}
}

func (c *clientConn) onServerState(ctx context.Context, frame *protocol.Frame) {
	channel := c.connection.Server(frame.Server)
	if channel == nil {
		c.log.Warn("state for unknown server", "server", frame.Server)
		return
	}

	state := frame.State
	if state == "" {
		state = protocol.StateFailed
	}
	channel.SetState(state, frame.ErrorMsg, frame.ExitCode)
	c.log.Info("server state", "server", frame.Server, "state", state, "error", frame.ErrorMsg)

	// Hand the state to that channel's supervisor and move on. Nothing about a
	// session is done on this goroutine: the receive loop carries every server
	// on this tunnel, so blocking it to open or close one server's session
	// stalls all the others - including the MCP replies the session being
	// opened is itself waiting for.
	c.supervisor(ctx, channel).setDesired(state)
	c.registry.PublishChange()
}

// supervisor returns the goroutine that owns this channel's session, starting
// it on first use.
func (c *clientConn) supervisor(ctx context.Context, channel *registry.ServerChannel) *channelOwner {
	c.mu.Lock()
	defer c.mu.Unlock()

	if owner, ok := c.channels[channel.Name]; ok {
		return owner
	}

	ownerCtx, cancel := context.WithCancel(ctx)
	owner := &channelOwner{
		incoming: make(chan json.RawMessage, 64),
		desired:  make(chan string, 1),
		cancel:   cancel,
		done:     make(chan struct{}),
	}
	c.channels[channel.Name] = owner
	go c.superviseChannel(ownerCtx, channel, owner)
	return owner
}

// setDesired records the state the client last reported, replacing any state
// the supervisor has not picked up yet.
//
// Latest-wins, and never blocking: during a burst of restarts the intermediate
// states are not worth acting on, only the one the server settles in.
func (o *channelOwner) setDesired(state string) {
	for {
		select {
		case o.desired <- state:
			return
		default:
		}
		select {
		case <-o.desired:
		default:
			// Drained by the supervisor between the two selects; try again.
		}
	}
}

// superviseChannel reconciles one server's session with the state the client
// reports, for the life of the connection.
//
// Reconciling rather than reacting is what makes a burst of restarts safe. The
// obvious implementation - open a session on every "running" frame, close it on
// every other - sends an `initialize` for each one, and cancelling that
// goroutine does not un-send the initialize already on the wire. The client
// meanwhile has respawned the server, so those stale initializes arrive at the
// *new* process, which rejects the second one ("the initialize handshake is not
// accepted") and leaves the channel with a session that cannot list tools.
// Waiting for the state to settle collapses a burst into the single session the
// final state calls for.
func (c *clientConn) superviseChannel(ctx context.Context, channel *registry.ServerChannel, owner *channelOwner) {
	defer close(owner.done)

	var (
		session *mcp.ClientSession
		open    bool
	)
	closeSession := func() {
		if !open {
			return
		}
		open = false
		owner.stopAccepting()
		if session != nil {
			session.Close()
			session = nil
		}
		channel.SetSession(nil)
		channel.SetTools(nil)
		if c.opts.Metrics != nil && c.current() {
			c.opts.Metrics.ClearTools(channel.Label, channel.Name)
		}
		c.registry.PublishChange()
	}
	defer closeSession()

	state, ok := owner.next(ctx)
	for ok {
		// A session belonging to the previous incarnation is retired the moment
		// any new state arrives, whatever it is: the process behind it is gone.
		closeSession()

		if state != protocol.StateRunning {
			state, ok = owner.next(ctx)
			continue
		}

		// Let the state settle before opening a session. A server about to be
		// restarted again reports "starting" within milliseconds, and there is
		// nothing to gain from initializing a process already being replaced.
		//
		// A state that arrives during the wait is carried straight into the next
		// iteration rather than put back on the channel: re-queuing it could
		// displace a newer state the receive loop had just posted, and the
		// supervisor would then settle on a state the server has already left.
		newState, settled := owner.settle(ctx, c.opts.SettleDelay)
		if !settled {
			state, ok = newState, newState != ""
			continue
		}

		opened, err := c.openSession(ctx, channel, owner)
		if err != nil {
			owner.stopAccepting()
			if ctx.Err() == nil {
				channel.SetState(protocol.StateFailed, "initialize failed: "+err.Error(), nil)
				c.log.Warn("session did not initialize", "server", channel.Name, "error", err)
				c.registry.PublishChange()
			}
		} else {
			session, open = opened, true
		}

		state, ok = owner.next(ctx)
	}
}

// next blocks for the state the client last reported. ok is false once the
// connection is going away.
func (o *channelOwner) next(ctx context.Context) (string, bool) {
	select {
	case <-ctx.Done():
		return "", false
	case state := <-o.desired:
		return state, true
	}
}

// settle waits out the settle delay. It returns settled=true if nothing new
// arrived, or the state that interrupted it.
func (o *channelOwner) settle(ctx context.Context, delay time.Duration) (string, bool) {
	if delay <= 0 {
		return "", true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return "", false
	case state := <-o.desired:
		return state, false
	case <-timer.C:
		return "", true
	}
}

// openSession runs initialize and the first tools/list for one channel.
func (c *clientConn) openSession(
	ctx context.Context, channel *registry.ServerChannel, owner *channelOwner,
) (*mcp.ClientSession, error) {
	owner.startAccepting()
	transport := &mcpsession.ChannelTransport{
		Incoming: owner.incoming,
		Send: func(sendCtx context.Context, payload json.RawMessage) error {
			return c.send(sendCtx, protocol.MCP(channel.Name, payload))
		},
	}

	client := mcp.NewClient(&mcp.Implementation{Name: hubName, Version: c.opts.HubVersion}, nil)

	// Connect performs initialize, so this timeout covers it; cancelling ctx
	// aborts it at any point, which is what stops a server that went away
	// mid-handshake from holding the channel for the full timeout.
	initCtx, cancelInit := context.WithTimeout(ctx, c.opts.ToolsTimeout)
	// Pinned below 2026-07-28 on purpose: from that version the SDK first sends
	// a stateless server/discover probe, which every current stdio server
	// rejects (each rejection is a validation-error stack in that server's
	// stderr, forwarded to the client's log) before the SDK falls back to
	// initialize anyway. These are long-lived stateful stdio sessions, so the
	// probe buys nothing.
	session, err := client.Connect(initCtx, transport, &mcp.ClientSessionOptions{
		ProtocolVersion: legacyProtocolVersion,
	})
	cancelInit()
	if err != nil {
		return nil, err
	}

	channel.SetSession(session)
	c.refreshTools(ctx, channel, session)
	c.log.Info("session up", "server", channel.Name, "tools", len(channel.Tools()))
	c.registry.PublishChange()
	return session, nil
}

func (c *clientConn) refreshTools(ctx context.Context, channel *registry.ServerChannel, session *mcp.ClientSession) {
	listCtx, cancel := context.WithTimeout(ctx, c.opts.ToolsTimeout)
	defer cancel()

	result, err := session.ListTools(listCtx, nil)
	if err != nil {
		// A server that cannot list its tools is still a live session: it may
		// answer a later tools/list after a restart. Better an empty catalog
		// for that one server than a dead channel.
		c.log.Warn("tools/list failed", "server", channel.Name, "error", err)
		channel.SetTools(nil)
		return
	}

	tools := make([]registry.ToolInfo, 0, len(result.Tools))
	for _, tool := range result.Tools {
		tools = append(tools, registry.ToolInfo{
			Name:        tool.Name,
			Title:       tool.Title,
			Description: tool.Description,
			InputSchema: asSchemaMap(tool.InputSchema),
			Annotations: annotationsOf(tool.Annotations, tool.Meta),
		})
	}
	channel.SetTools(tools)
	if c.opts.Metrics != nil && c.current() {
		c.opts.Metrics.SetTools(channel.Label, channel.Name, len(tools))
	}
}

func annotationsOf(a *mcp.ToolAnnotations, meta mcp.Meta) *registry.ToolAnnotations {
	if a == nil && !metaIrreversible(meta) {
		return nil
	}
	out := &registry.ToolAnnotations{}
	if a != nil {
		out.ReadOnly, out.Destructive, out.OpenWorld = a.ReadOnlyHint, a.DestructiveHint, a.OpenWorldHint
	}
	if sw, ok := meta["switchboard"].(map[string]any); ok {
		if irr, _ := sw["irreversible"].(bool); irr {
			out.Irreversible = true
			out.Reason, _ = sw["irreversibleReason"].(string)
		}
	}
	return out
}

func metaIrreversible(meta mcp.Meta) bool {
	sw, ok := meta["switchboard"].(map[string]any)
	if !ok {
		return false
	}
	irr, _ := sw["irreversible"].(bool)
	return irr
}

// asSchemaMap normalises a tool's input schema to the generic map the registry
// stores and the API serves. The SDK hands back whatever the upstream server
// declared, so a schema that will not round-trip through JSON becomes an empty
// object rather than breaking the whole catalog.
func asSchemaMap(schema any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}
	if m, ok := schema.(map[string]any); ok {
		return m
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func (c *clientConn) teardown() {
	c.mu.Lock()
	owners := make([]*channelOwner, 0, len(c.channels))
	for _, owner := range c.channels {
		owners = append(owners, owner)
	}
	c.channels = map[string]*channelOwner{}
	c.mu.Unlock()

	for _, owner := range owners {
		owner.cancel()
	}
	for _, owner := range owners {
		select {
		case <-owner.done:
		case <-time.After(c.opts.StopWait):
		}
	}

	if c.connection != nil {
		removed := c.registry.RemoveConnection(c.connection.ID)
		// If this connection was evicted, the newer one owns the metrics for
		// the label; the evictor already cleared the old ones.
		if removed != nil && c.opts.Metrics != nil {
			for _, channel := range c.connection.Servers() {
				c.opts.Metrics.ClearTools(c.connection.Label, channel.Name)
			}
		}
	}
	c.ws.Close(websocket.StatusNormalClosure, "")
}

type protocolError struct{ msg string }

func (e *protocolError) Error() string { return e.msg }

// protocolErrorf builds the message that is both logged and sent to the client
// in an error frame, so it has to read as an explanation rather than a code.
func protocolErrorf(format string, args ...any) error {
	return &protocolError{msg: fmt.Sprintf(format, args...)}
}
