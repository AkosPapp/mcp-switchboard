package mcpsession

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

func testConn(t *testing.T, incoming chan json.RawMessage) (*channelConn, *[]json.RawMessage, *bool) {
	t.Helper()
	sent := &[]json.RawMessage{}
	closed := new(bool)
	transport := &ChannelTransport{
		Incoming: incoming,
		Send: func(_ context.Context, raw json.RawMessage) error {
			*sent = append(*sent, raw)
			return nil
		},
		OnClose: func() { *closed = true },
	}
	conn, err := transport.Connect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return conn.(*channelConn), sent, closed
}

func TestReadDecodesAMessage(t *testing.T) {
	incoming := make(chan json.RawMessage, 1)
	conn, _, _ := testConn(t, incoming)
	incoming <- json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	msg, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	req, ok := msg.(*jsonrpc.Request)
	if !ok {
		t.Fatalf("decoded %T, want a request", msg)
	}
	if req.Method != "tools/list" {
		t.Errorf("method = %q", req.Method)
	}
}

// A server that writes junk to stdout would otherwise take down every tool it
// exposes, which is a bad trade for one bad line.
func TestReadSkipsMalformedMessages(t *testing.T) {
	incoming := make(chan json.RawMessage, 2)
	conn, _, _ := testConn(t, incoming)
	incoming <- json.RawMessage(`not json at all`)
	incoming <- json.RawMessage(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)

	msg, err := conn.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if req := msg.(*jsonrpc.Request); req.Method != "ping" {
		t.Errorf("method = %q, want the message after the junk", req.Method)
	}
}

// The SDK requires this: a session retired for a restart must not leave a
// goroutine parked on a channel nobody will send to.
func TestCloseUnblocksAConcurrentRead(t *testing.T) {
	conn, _, closed := testConn(t, make(chan json.RawMessage))

	errc := make(chan error, 1)
	go func() {
		_, err := conn.Read(context.Background())
		errc <- err
	}()

	time.Sleep(10 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errc:
		if !errors.Is(err, ErrChannelClosed) {
			t.Errorf("Read returned %v, want ErrChannelClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not unblock Read")
	}
	if !*closed {
		t.Error("OnClose was not called")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	conn, _, _ := testConn(t, make(chan json.RawMessage))
	for i := 0; i < 3; i++ {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWriteEncodesAndRefusesAfterClose(t *testing.T) {
	conn, sent, _ := testConn(t, make(chan json.RawMessage))
	id, err := jsonrpc.MakeID(float64(1))
	if err != nil {
		t.Fatal(err)
	}
	req := &jsonrpc.Request{ID: id, Method: "tools/list"}

	if err := conn.Write(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(*sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(*sent))
	}

	conn.Close()
	if err := conn.Write(context.Background(), req); !errors.Is(err, ErrChannelClosed) {
		t.Errorf("write after close = %v, want ErrChannelClosed", err)
	}
}

func TestReadHonoursContextCancellation(t *testing.T) {
	conn, _, _ := testConn(t, make(chan json.RawMessage))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := conn.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Read = %v, want context.Canceled", err)
	}
}

// A closed incoming channel means the server channel is gone for good.
func TestReadReportsAClosedChannel(t *testing.T) {
	incoming := make(chan json.RawMessage)
	conn, _, _ := testConn(t, incoming)
	close(incoming)
	if _, err := conn.Read(context.Background()); !errors.Is(err, ErrChannelClosed) {
		t.Errorf("Read = %v, want ErrChannelClosed", err)
	}
}
