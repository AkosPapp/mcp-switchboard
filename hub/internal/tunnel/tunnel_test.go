package tunnel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/AkosPapp/mcp-switchboard/hub/internal/events"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/protocol"
	"github.com/AkosPapp/mcp-switchboard/hub/internal/registry"
)

const testToken = "s3cr3t"

func newTestHub(t *testing.T) (*httptest.Server, *registry.Registry) {
	t.Helper()
	reg := registry.New(events.NewBus())
	handler := NewHandler(reg, Options{
		Token:        testToken,
		ToolsTimeout: 3 * time.Second,
		StopWait:     time.Second,
		HubVersion:   "test",
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, reg
}

// fakeClient is the other end of the tunnel: it speaks the frame protocol, and
// answers MCP requests for its servers the way a real stdio server would.
type fakeClient struct {
	t  *testing.T
	ws *websocket.Conn

	// A websocket allows one writer at a time, and this client writes from both
	// the test goroutine (state frames) and serve's goroutine (MCP replies).
	sendMu sync.Mutex

	mu sync.Mutex
	// tools answered by tools/list, per server name.
	tools map[string][]map[string]any
	// which servers are currently up. A real client answers nothing for a
	// server whose process has exited - there is no process left to answer -
	// so the fake must not either, or a restart test measures a situation that
	// cannot occur on the wire.
	up map[string]bool
	// how many initialize requests this fake has answered per server, which is
	// how the burst test observes session churn.
	initializes map[string]int
	// frames the hub sent that were not MCP traffic, for assertions.
	inbox chan *protocol.Frame
}

func dial(t *testing.T, srv *httptest.Server, token string) (*fakeClient, error) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http")
	ws, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		return nil, err
	}
	ws.SetReadLimit(maxFrameBytes)
	client := &fakeClient{
		t:           t,
		ws:          ws,
		tools:       map[string][]map[string]any{},
		up:          map[string]bool{},
		initializes: map[string]int{},
		inbox:       make(chan *protocol.Frame, 16),
	}
	t.Cleanup(func() { ws.Close(websocket.StatusNormalClosure, "") })
	return client, nil
}

func (c *fakeClient) send(frame any) {
	c.t.Helper()
	raw, err := json.Marshal(frame)
	if err != nil {
		c.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if err := c.ws.Write(ctx, websocket.MessageText, raw); err != nil {
		// Not Fatalf: this runs on serve's goroutine too, where the testing
		// package forbids it, and a closed socket at teardown is expected.
		c.t.Logf("write: %v", err)
	}
}

func (c *fakeClient) hello(servers ...protocol.ServerDecl) {
	c.send(map[string]any{
		"type":     protocol.TypeHello,
		"protocol": protocol.Version,
		"client": map[string]any{
			"name": "fake", "version": "0", "instance": "i", "label": "box",
		},
		"servers": servers,
	})
}

// serve answers the hub's MCP traffic until the socket closes. Anything that is
// not an mcp frame goes to inbox for the test to assert on.
func (c *fakeClient) serve() {
	go func() {
		for {
			_, raw, err := c.ws.Read(context.Background())
			if err != nil {
				close(c.inbox)
				return
			}
			frame, err := protocol.Decode(raw)
			if err != nil {
				continue
			}
			if frame.Type != protocol.TypeMCP {
				select {
				case c.inbox <- frame:
				default:
				}
				continue
			}
			c.answerMCP(frame)
		}
	}()
}

func (c *fakeClient) answerMCP(frame *protocol.Frame) {
	var msg struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(frame.Payload, &msg); err != nil {
		return
	}
	if msg.ID == nil {
		return // a notification: nothing to answer
	}

	c.mu.Lock()
	running := c.up[frame.Server]
	c.mu.Unlock()
	if !running {
		return
	}

	var result any
	switch msg.Method {
	case "initialize":
		c.mu.Lock()
		c.initializes[frame.Server]++
		c.mu.Unlock()
		result = map[string]any{
			"protocolVersion": mcp.SupportedProtocolVersions()[len(mcp.SupportedProtocolVersions())-1],
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": frame.Server, "version": "1"},
		}
	case "tools/list":
		c.mu.Lock()
		tools := c.tools[frame.Server]
		c.mu.Unlock()
		if tools == nil {
			tools = []map[string]any{}
		}
		result = map[string]any{"tools": tools}
	case "tools/call":
		result = map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ok"}},
		}
	default:
		result = map[string]any{}
	}

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": result,
	})
	if err != nil {
		return
	}
	c.send(map[string]any{
		"type": protocol.TypeMCP, "server": frame.Server, "payload": json.RawMessage(payload),
	})
}

func (c *fakeClient) setTools(server string, names ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tools := make([]map[string]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, map[string]any{
			"name":        name,
			"description": "a tool",
			"inputSchema": map[string]any{"type": "object"},
		})
	}
	c.tools[server] = tools
}

func (c *fakeClient) running(server string) {
	c.mu.Lock()
	c.up[server] = true
	c.mu.Unlock()
	c.send(map[string]any{
		"type": protocol.TypeServerState, "server": server, "state": protocol.StateRunning,
	})
}

// starting is what a real client sends at the front of a restart: the process
// is gone, a new one is on its way, and no "exited" frame is sent at all.
func (c *fakeClient) starting(server string) {
	c.mu.Lock()
	c.up[server] = false
	c.mu.Unlock()
	c.send(map[string]any{
		"type": protocol.TypeServerState, "server": server, "state": protocol.StateStarting,
	})
}

func (c *fakeClient) exited(server string) {
	c.mu.Lock()
	c.up[server] = false
	c.mu.Unlock()
	c.send(map[string]any{
		"type": protocol.TypeServerState, "server": server, "state": protocol.StateExited,
	})
}

// waitFor polls until cond holds, so tests assert on an outcome rather than on
// a sleep long enough to be flaky on a loaded machine.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestRejectsABadToken(t *testing.T) {
	srv, _ := newTestHub(t)
	if _, err := dial(t, srv, "wrong"); err == nil {
		t.Fatal("a client with the wrong token must not be accepted")
	}
}

func TestHelloRegistersTheConnection(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git", Command: "git-mcp"})

	waitFor(t, "the connection to register", func() bool { return len(reg.Connections()) == 1 })
	conn := reg.Connections()[0]
	if conn.Label != "box" {
		t.Errorf("label = %q", conn.Label)
	}
	if len(conn.Servers()) != 1 {
		t.Fatalf("got %d servers, want 1", len(conn.Servers()))
	}
}

func TestSessionPublishesTools(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.setTools("git", "git_status", "git_log")
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"}, protocol.ServerDecl{Name: "fs", Project: "web"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })

	client.running("git")

	waitFor(t, "tools to appear", func() bool {
		channel := reg.FindServer(reg.Connections()[0].ID, "git")
		return channel != nil && len(channel.Tools()) == 2
	})

	channel := reg.FindServer(reg.Connections()[0].ID, "git")
	if !channel.Ready() {
		t.Error("a server with a live session must report ready")
	}
	if got := channel.Tools()[0].Name; got != "git_status" {
		t.Errorf("first tool = %q", got)
	}

	// A server the client never reported running has no session and no tools,
	// which is what keeps a declared-but-dead server out of the catalog.
	fs := reg.FindServer(reg.Connections()[0].ID, "fs")
	if fs.Ready() || len(fs.Tools()) != 0 {
		t.Error("a server that never started must expose nothing")
	}
}

func TestStopRetiresTheSessionAndItsTools(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.setTools("git", "git_status")
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })
	connID := reg.Connections()[0].ID

	client.running("git")
	waitFor(t, "tools", func() bool {
		channel := reg.FindServer(connID, "git")
		return channel != nil && len(channel.Tools()) == 1
	})

	client.exited("git")
	waitFor(t, "the session to retire", func() bool {
		channel := reg.FindServer(connID, "git")
		return channel != nil && !channel.Ready() && len(channel.Tools()) == 0
	})
}

// The reason stopChannel waits for the old owner before a new one starts: a
// restart that raced its predecessor used to come back with no tools at all.
func TestRapidRestartsNeverLoseTools(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.setTools("git", "git_status")
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })
	connID := reg.Connections()[0].ID

	for i := 0; i < 5; i++ {
		client.running("git")
		client.exited("git")
	}
	client.running("git")

	waitFor(t, "tools after the last restart", func() bool {
		channel := reg.FindServer(connID, "git")
		return channel != nil && channel.Ready() && len(channel.Tools()) == 1
	})
}

func TestDisconnectRemovesTheConnection(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })

	client.ws.Close(websocket.StatusNormalClosure, "")
	waitFor(t, "the connection to drop", func() bool { return len(reg.Connections()) == 0 })
}

func TestHelloValidation(t *testing.T) {
	cases := map[string]map[string]any{
		"wrong first frame": {"type": protocol.TypeMCP, "server": "git"},
		"bad version": {
			"type": protocol.TypeHello, "protocol": 99,
			"client":  map[string]any{"label": "box"},
			"servers": []any{map[string]any{"name": "git"}},
		},
		"label with the separator": {
			"type": protocol.TypeHello, "protocol": protocol.Version,
			"client":  map[string]any{"label": "a__b"},
			"servers": []any{map[string]any{"name": "git"}},
		},
		"server name with the separator": {
			"type": protocol.TypeHello, "protocol": protocol.Version,
			"client":  map[string]any{"label": "box"},
			"servers": []any{map[string]any{"name": "a__b"}},
		},
		"duplicate server names": {
			"type": protocol.TypeHello, "protocol": protocol.Version,
			"client": map[string]any{"label": "box"},
			"servers": []any{
				map[string]any{"name": "git"}, map[string]any{"name": "git"},
			},
		},
		"no servers": {
			"type": protocol.TypeHello, "protocol": protocol.Version,
			"client": map[string]any{"label": "box"}, "servers": []any{},
		},
	}

	for name, hello := range cases {
		t.Run(name, func(t *testing.T) {
			srv, reg := newTestHub(t)
			client, err := dial(t, srv, testToken)
			if err != nil {
				t.Fatal(err)
			}
			client.serve()
			client.send(hello)

			select {
			case frame, ok := <-client.inbox:
				if !ok {
					t.Fatal("socket closed without an error frame")
				}
				if frame.Type != protocol.TypeError {
					t.Fatalf("got a %q frame, want an error frame", frame.Type)
				}
				if frame.Message == "" {
					t.Error("an error frame must say what was wrong")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("no error frame arrived")
			}

			if len(reg.Connections()) != 0 {
				t.Error("a refused client must not be registered")
			}
		})
	}
}

// Two machines claiming one label would silently shadow each other's tools.
func TestDuplicateLabelIsRefused(t *testing.T) {
	srv, reg := newTestHub(t)

	first, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	first.serve()
	first.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "the first client", func() bool { return len(reg.Connections()) == 1 })

	second, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	second.serve()
	second.hello(protocol.ServerDecl{Name: "fs"})

	select {
	case frame, ok := <-second.inbox:
		if !ok || frame.Type != protocol.TypeError {
			t.Fatal("the second client should have been refused with an error frame")
		}
		if !strings.Contains(frame.Message, "already connected") {
			t.Errorf("error = %q, want it to explain the label clash", frame.Message)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no error frame arrived")
	}

	if len(reg.Connections()) != 1 {
		t.Errorf("registry holds %d connections, want the original only", len(reg.Connections()))
	}
}

// A server that goes quiet must not hold the channel open forever.
func TestInitializeTimeoutFailsTheChannel(t *testing.T) {
	reg := registry.New(events.NewBus())
	handler := NewHandler(reg, Options{
		Token:        testToken,
		ToolsTimeout: 150 * time.Millisecond,
		StopWait:     time.Second,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately not calling serve(): nothing answers initialize.
	client.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })
	connID := reg.Connections()[0].ID
	client.running("git")

	waitFor(t, "the channel to fail", func() bool {
		channel := reg.FindServer(connID, "git")
		if channel == nil {
			return false
		}
		state, errMsg, _ := channel.State()
		return state == protocol.StateFailed && errMsg != ""
	})
}

func TestUnknownFramesAreIgnored(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })

	client.send(map[string]any{"type": "from_the_future", "server": "git"})
	client.send(map[string]any{"type": protocol.TypeServerState, "server": "nosuch", "state": "running"})

	// Still alive and still serving after both.
	client.setTools("git", "git_status")
	client.running("git")
	waitFor(t, "the connection to keep working", func() bool {
		connections := reg.Connections()
		if len(connections) == 0 {
			return false
		}
		channel := reg.FindServer(connections[0].ID, "git")
		return channel != nil && len(channel.Tools()) == 1
	})
}

func (c *fakeClient) initializeCount(server string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.initializes[server]
}

// A burst of restarts must produce ONE session, not one per frame.
//
// This is the regression test for a bug the end-to-end suite found: the hub
// used to open a session on every "running" frame and cancel it on the next
// "starting". Cancelling does not un-send an initialize already on the wire, so
// the stale ones arrived at the process the client had just respawned, which
// rejected the second handshake and left the channel unable to list tools. The
// assertion that matters is the initialize count.
func TestARestartBurstOpensOneSession(t *testing.T) {
	reg := registry.New(events.NewBus())
	handler := NewHandler(reg, Options{
		Token:        testToken,
		ToolsTimeout: 3 * time.Second,
		StopWait:     time.Second,
		SettleDelay:  50 * time.Millisecond,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.setTools("git", "git_status")
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })
	connID := reg.Connections()[0].ID

	// The client reports "starting" then "running" for each restart, which is
	// what a real one does - it never sends "exited" for a restart.
	for i := 0; i < 6; i++ {
		client.starting("git")
		client.running("git")
	}

	waitFor(t, "the session to come up after the burst", func() bool {
		channel := reg.FindServer(connID, "git")
		return channel != nil && channel.Ready() && len(channel.Tools()) == 1
	})

	if got := client.initializeCount("git"); got != 1 {
		t.Errorf("the burst produced %d initializes, want exactly 1", got)
	}
}

// One server's session churn must not stall another server on the same tunnel:
// they share a receive loop, and MCP replies arrive through it.
func TestOneChannelDoesNotStallAnother(t *testing.T) {
	srv, reg := newTestHub(t)
	client, err := dial(t, srv, testToken)
	if err != nil {
		t.Fatal(err)
	}
	client.setTools("fs", "read")
	client.serve()
	client.hello(protocol.ServerDecl{Name: "git"}, protocol.ServerDecl{Name: "fs"})
	waitFor(t, "registration", func() bool { return len(reg.Connections()) == 1 })
	connID := reg.Connections()[0].ID

	// git is declared running but nothing ever answers for it, so its
	// handshake will sit there until it times out.
	client.send(map[string]any{
		"type": protocol.TypeServerState, "server": "git", "state": protocol.StateRunning,
	})
	client.running("fs")

	waitFor(t, "fs to come up while git hangs", func() bool {
		channel := reg.FindServer(connID, "fs")
		return channel != nil && channel.Ready() && len(channel.Tools()) == 1
	})
}
