package remotecontrol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// testRelay is a minimal stand-in for the remote relay server: it answers
// /api/login and upgrades /ws/cli, handing the raw server-side connection to
// the test so it can misbehave on purpose.
type testRelay struct {
	url      string
	conns    chan *websocket.Conn
	upgrader websocket.Upgrader
}

func newTestRelay(t *testing.T, onConn func(*websocket.Conn)) *testRelay {
	t.Helper()
	r := &testRelay{conns: make(chan *websocket.Conn, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", func(w http.ResponseWriter, req *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "token": "test-token"})
	})
	mux.HandleFunc("/ws/cli", func(w http.ResponseWriter, req *http.Request) {
		conn, err := r.upgrader.Upgrade(w, req, nil)
		if err != nil {
			return
		}
		select {
		case r.conns <- conn:
		default:
		}
		if onConn != nil {
			onConn(conn)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	r.url = "ws://" + strings.TrimPrefix(srv.URL, "http://")
	return r
}

// drain reads and discards everything the client sends, so the server side
// never applies backpressure.
func drain(conn *websocket.Conn) {
	for {
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (r *testRelay) send(t *testing.T, conn *websocket.Conn, evtType string, payload any) {
	t.Helper()
	p, err := json.Marshal(payload)
	require.NoError(t, err)
	raw, err := json.Marshal(EventMessage{Type: evtType, Payload: p})
	require.NoError(t, err)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, raw))
}

func connectClient(t *testing.T, r *testRelay, ctx context.Context) *Client {
	t.Helper()
	c := NewClient(Config{RelayURL: r.url, Username: "u", Password: "p"})
	require.NoError(t, c.Connect(ctx, "test-session"))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Regression: gorilla/websocket allows exactly one concurrent writer and
// panics on a second. SendEvent previously guarded the write with an RWMutex
// read lock, which permits unlimited simultaneous holders.
func TestConcurrentSendsDoNotPanic(t *testing.T) {
	r := newTestRelay(t, func(conn *websocket.Conn) { go drain(conn) })
	c := connectClient(t, r, context.Background())

	payload := strings.Repeat("x", 8192)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = c.SendStreamChunk("assistant", payload)
			}
		}()
	}
	wg.Wait()
}

// Regression: a duplicate tool_response for the same request id used to block
// readLoop forever on a full cap-1 channel, silently killing the session.
func TestDuplicateToolResponseDoesNotWedgeReadLoop(t *testing.T) {
	r := newTestRelay(t, func(conn *websocket.Conn) { go drain(conn) })
	c := connectClient(t, r, context.Background())
	conn := <-r.conns

	// Register a waiter and abandon it, exactly as RequestToolApproval does
	// when its context expires.
	c.mu.Lock()
	c.pendingTools["req-1"] = make(chan bool, 1)
	c.mu.Unlock()

	r.send(t, conn, "tool_response", ToolResponsePayload{RequestID: "req-1", Approved: true})
	r.send(t, conn, "tool_response", ToolResponsePayload{RequestID: "req-1", Approved: true})

	// The read loop must still be processing messages afterwards.
	got := make(chan string, 1)
	c.SetPromptHandler(func(p string) { got <- p })
	r.send(t, conn, "send_prompt", map[string]string{"prompt": "still alive"})

	select {
	case p := <-got:
		require.Equal(t, "still alive", p)
	case <-time.After(5 * time.Second):
		t.Fatal("read loop wedged: send_prompt was never delivered after a duplicate tool_response")
	}
}

// Regression: without SetReadLimit the relay could force the client to
// allocate an arbitrarily large buffer.
func TestOversizedFrameIsRejected(t *testing.T) {
	oversized := strings.Repeat("A", int(maxMessageSize)+4096)
	r := newTestRelay(t, func(conn *websocket.Conn) {
		go drain(conn)
		raw := []byte(fmt.Sprintf(`{"type":"send_prompt","payload":{"prompt":%q}}`, oversized))
		_ = conn.WriteMessage(websocket.TextMessage, raw)
	})
	c := connectClient(t, r, context.Background())

	delivered := make(chan string, 1)
	c.SetPromptHandler(func(p string) { delivered <- p })

	select {
	case p := <-delivered:
		t.Fatalf("oversized frame was accepted: delivered %d bytes", len(p))
	case <-c.closeCh:
		// read loop tore the connection down, which is the desired behaviour
	case <-time.After(10 * time.Second):
		t.Fatal("client neither rejected the oversized frame nor closed")
	}
}

// Regression: gorilla reads ignore context cancellation, so cancelling the
// context has to close the connection or the read blocks forever.
func TestContextCancelUnblocksReadLoop(t *testing.T) {
	// Server accepts the connection then goes permanently silent.
	r := newTestRelay(t, func(conn *websocket.Conn) { go drain(conn) })
	ctx, cancel := context.WithCancel(context.Background())
	c := connectClient(t, r, ctx)

	cancel()

	select {
	case <-c.closeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("context cancellation did not unblock the read loop")
	}
}

// Regression: a peer that vanishes without sending FIN must be detected via
// the read deadline rather than hanging the read loop forever.
func TestSilentPeerIsDetectedByReadDeadline(t *testing.T) {
	origPong, origPing := pongWait, pingPeriod
	pongWait, pingPeriod = 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pongWait, pingPeriod = origPong, origPing })

	// Read the client's frames but never reply to pings: gorilla answers
	// pings automatically, so suppress that by installing a no-op handler.
	r := newTestRelay(t, func(conn *websocket.Conn) {
		conn.SetPingHandler(func(string) error { return nil })
		go drain(conn)
	})
	c := connectClient(t, r, context.Background())

	select {
	case <-c.closeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("silent peer was never detected: read deadline did not fire")
	}
}

// A healthy peer that answers pings must keep the session open past pongWait.
func TestPongRefreshesReadDeadline(t *testing.T) {
	origPong, origPing := pongWait, pingPeriod
	pongWait, pingPeriod = 300*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pongWait, pingPeriod = origPong, origPing })

	// Default gorilla ping handler replies with a pong.
	r := newTestRelay(t, func(conn *websocket.Conn) { go drain(conn) })
	c := connectClient(t, r, context.Background())

	select {
	case <-c.closeCh:
		t.Fatal("connection to a responsive peer was torn down")
	case <-time.After(2 * time.Second):
		// survived ~6 pongWait periods
	}
}
