// Package mcpsession runs one MCP client session per tunnelled server.
//
// The hub is the only party that speaks MCP: the client is a dumb pipe that
// shuttles a local server's stdio across the tunnel tagged with a server name.
// So a "connection" to an upstream server is not a socket or a subprocess here,
// it is a pair of channels through the tunnel frame pump, which is what
// ChannelTransport adapts to the SDK's Transport interface.
package mcpsession

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrChannelClosed is returned once the underlying server channel is gone,
// which happens when the server stops or its tunnel drops.
var ErrChannelClosed = errors.New("server channel closed")

// ChannelTransport is an mcp.Transport over one tunnelled server channel.
//
// Incoming carries the payloads of `mcp` frames the client sent for this
// server, already separated from other servers on the same tunnel. Send writes
// one back. Neither side is a stream: the tunnel is message-oriented, so there
// is no framing to do here beyond JSON-RPC encode/decode.
type ChannelTransport struct {
	Incoming <-chan json.RawMessage
	Send     func(context.Context, json.RawMessage) error

	// Called once when the connection closes, so the owner can retire the
	// channel without polling the session's state.
	OnClose func()
}

// Connect implements mcp.Transport. It is called exactly once per session.
func (t *ChannelTransport) Connect(context.Context) (mcp.Connection, error) {
	return &channelConn{
		incoming: t.Incoming,
		send:     t.Send,
		onClose:  t.OnClose,
		done:     make(chan struct{}),
	}, nil
}

type channelConn struct {
	incoming <-chan json.RawMessage
	send     func(context.Context, json.RawMessage) error
	onClose  func()

	closeOnce sync.Once
	done      chan struct{}
}

// Read blocks for the next message from the upstream server.
//
// The SDK requires Read to be unblocked by a concurrent Close, which is what
// the done channel is for: without it, retiring a session for a restart would
// leave a goroutine parked on a channel that nobody will ever send to.
func (c *channelConn) Read(ctx context.Context) (jsonrpc.Message, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.done:
			return nil, ErrChannelClosed
		case raw, ok := <-c.incoming:
			if !ok {
				return nil, ErrChannelClosed
			}
			msg, err := jsonrpc.DecodeMessage(raw)
			if err != nil {
				// One malformed line from a misbehaving upstream server must not
				// tear down the session: the next message may well be fine, and
				// killing the channel here would lose every tool it exposes.
				continue
			}
			return msg, nil
		}
	}
}

func (c *channelConn) Write(ctx context.Context, msg jsonrpc.Message) error {
	select {
	case <-c.done:
		return ErrChannelClosed
	default:
	}

	raw, err := jsonrpc.EncodeMessage(msg)
	if err != nil {
		return err
	}
	return c.send(ctx, raw)
}

// Close may be called several times, concurrently, and from either direction -
// the SDK calls it implicitly whenever a Read or Write fails.
func (c *channelConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.done)
		if c.onClose != nil {
			c.onClose()
		}
	})
	return nil
}

// SessionID is empty: sessions here are identified by the (connection, server)
// pair the hub already tracks, not by anything on the wire.
func (c *channelConn) SessionID() string { return "" }
