package indexer

import (
	"context"
	"net/http"

	"github.com/coder/websocket"
	"github.com/jcalabro/atmos/streaming"
)

// dialWebsocket is this package's websocket dialer, used when a Source has
// no injected Dial. It mirrors atmos's internal dialer (same websocket
// library, subprotocol offer, no compression) so behavior matches the
// atmos-managed path; atmos does not export its dialer, and the Source
// needs first-connect signaling for test synchronization.
func dialWebsocket(ctx context.Context, rawURL string, cfg streaming.DialConfig) (streaming.Conn, *http.Response, error) {
	subs := make([]string, 0, len(cfg.Subprotocols))
	for _, s := range cfg.Subprotocols {
		subs = append(subs, string(s))
	}
	conn, resp, err := websocket.Dial(ctx, rawURL, &websocket.DialOptions{
		Subprotocols:    subs,
		CompressionMode: cfg.Compression,
	})
	if err != nil {
		return nil, resp, err
	}
	return conn, resp, nil
}
