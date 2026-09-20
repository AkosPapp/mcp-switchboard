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

	HubVersion string
	Metrics    Metrics
	Logger     *slog.Logger
}

const (
	defaultToolsTimeout = 30 * time.Second
	defaultStopWait     = 5 * time.Second
	defaultSettleDelay  = 150 * time.Millisecond
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
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	presented := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return subtle.ConstantTimeCompare([]byte(presented), []byte(h.opts.Token)) == 1
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

	mu       sync.Mutex
	channels map[string]*channelOwner
}

// channelOwner is the handle the receive loop holds on one server's owner
// goroutine: a way to feed it, a way to stop it, and a way to know it is gone.
type channelOwner struct {
	incoming chan json.RawMessage
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

func (c *clientConn) run(ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.baseCtx = ctx

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
	c.registry.AddConnection(c.connection)

	defer c.teardown()
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
	if c.registry.LabelInUse(label) {
		// Two machines claiming one label would make tools ambiguous and
		// silently shadow each other in the aggregated catalog.
		return protocolErrorf("label %q is already connected", label)
	}

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
	return nil
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

	select {
	case owner.incoming <- frame.Payload:
	case <-owner.done:
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
		if session != nil {
			session.Close()
			session = nil
		}
		channel.SetSession(nil)
		channel.SetTools(nil)
		if c.opts.Metrics != nil {
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
	session, err := client.Connect(initCtx, transport, nil)
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
		})
	}
	channel.SetTools(tools)
	if c.opts.Metrics != nil {
		c.opts.Metrics.SetTools(channel.Label, channel.Name, len(tools))
	}
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
		c.registry.RemoveConnection(c.connection.ID)
		if c.opts.Metrics != nil {
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
