package indexer

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/jcalabro/atmos/streaming"
)

// fakeConn is an in-memory streaming.Conn: Read yields queued frames in
// order, then blocks until Close or ctx cancellation. It lets tests drive
// a Source without a live transport (mirrors atmos's own dial-injection
// test pattern).
type fakeConn struct {
	frames chan fakeFrame
	closed chan struct{}
	once   sync.Once
}

type fakeFrame struct {
	msgType websocket.MessageType
	data    []byte
}

func newFakeConn(frames ...[]byte) *fakeConn {
	c := &fakeConn{frames: make(chan fakeFrame, len(frames)), closed: make(chan struct{})}
	for _, f := range frames {
		c.frames <- fakeFrame{websocket.MessageText, f}
	}
	return c
}

func (c *fakeConn) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case f := <-c.frames:
		return f.msgType, f.data, nil
	case <-c.closed:
		return 0, nil, io.EOF
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (c *fakeConn) Close(websocket.StatusCode, string) error { c.shutdown(); return nil }
func (c *fakeConn) CloseNow() error                          { c.shutdown(); return nil }
func (c *fakeConn) SetReadLimit(int64)                       {}
func (c *fakeConn) Subprotocol() string                      { return "" }
func (c *fakeConn) shutdown()                                { c.once.Do(func() { close(c.closed) }) }

// jetstreamFrame renders one Jetstream #commit JSON frame per the
// Jetstream event schema: {did, time_us, kind: "commit", commit: {rev,
// operation, collection, rkey, record?, cid?}}.
func jetstreamFrame(t *testing.T, e RepoEvent, timeUS int64) []byte {
	t.Helper()
	commit := map[string]any{
		"rev":        e.CommitRev,
		"operation":  string(e.Kind),
		"collection": e.Collection,
		"rkey":       e.Rkey,
	}
	if len(e.Record) > 0 {
		commit["record"] = json.RawMessage(e.Record)
	}
	if e.CID != "" {
		commit["cid"] = e.CID
	}
	raw, err := json.Marshal(map[string]any{
		"did":     e.DID,
		"time_us": timeUS,
		"kind":    "commit",
		"commit":  commit,
	})
	if err != nil {
		t.Fatalf("marshal jetstream frame: %v", err)
	}
	return raw
}

// recorderHandler collects Handler deliveries for assertions.
type recorderHandler struct {
	events   chan RepoEvent
	identity chan [2]string
}

func newRecorderHandler() *recorderHandler {
	return &recorderHandler{
		events:   make(chan RepoEvent, 64),
		identity: make(chan [2]string, 64),
	}
}

func (r *recorderHandler) RepoEvent(ev RepoEvent) { r.events <- ev }
func (r *recorderHandler) Identity(did, handle string) {
	r.identity <- [2]string{did, handle}
}

// collect waits for n events (create, update, delete each arrive as one).
func (r *recorderHandler) collect(t *testing.T, n int, within time.Duration) []RepoEvent {
	t.Helper()
	var out []RepoEvent
	deadline := time.After(within)
	for len(out) < n {
		select {
		case ev := <-r.events:
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("timed out collecting %d events; got %d", n, len(out))
		}
	}
	return out
}

// dialTo returns a streaming.DialFunc that hands back the given conns in
// sequence and records every dialed URL (cursor-resume assertions).
func dialTo(t *testing.T, dialed *[]string, conns ...*fakeConn) *streaming.DialFunc {
	i := 0
	return dialFuncPtr(streaming.DialFunc(func(_ context.Context, url string, _ streaming.DialConfig) (streaming.Conn, *http.Response, error) {
		if i >= len(conns) {
			t.Errorf("unexpected dial #%d to %s", i+1, url)
			// Park until the run is torn down.
			c := newFakeConn()
			return c, nil, nil
		}
		if dialed != nil {
			*dialed = append(*dialed, url)
		}
		conn := conns[i]
		i++
		return conn, nil, nil
	}))
}

func dialFuncPtr(f streaming.DialFunc) *streaming.DialFunc { return &f }
