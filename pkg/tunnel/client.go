package tunnel

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// HandlerFunc executes one tool call on the agent side.
type HandlerFunc func(ctx context.Context, tool string, args json.RawMessage) (string, error)

type ClientConfig struct {
	URL string // wss://hub:8443/v1/tunnel
	TLS *tls.Config
	// Token returns the bearer agent token, read on every connection (rotation without a
	// restart); nil for mutual TLS.
	Token func() (string, error)
	Hello Hello
	Log   *slog.Logger
	// Called when the connection state changes (for readiness).
	OnState func(connected bool)
}

// Run keeps a connection to the hub open until ctx is done, reconnecting
// with backoff, and serves requests with handle.
func Run(ctx context.Context, cfg ClientConfig, handle HandlerFunc) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := runOnce(ctx, cfg, handle)
		if cfg.OnState != nil {
			cfg.OnState(false)
		}
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		cfg.Log.Warn("tunnel disconnected, reconnecting", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func runOnce(ctx context.Context, cfg ClientConfig, handle HandlerFunc) error {
	opts := &websocket.DialOptions{HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.TLS}}}
	if cfg.Token != nil {
		token, err := cfg.Token()
		if err != nil {
			return fmt.Errorf("agent token: %w", err)
		}
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + token}}
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, resp, err := websocket.Dial(dialCtx, cfg.URL, opts)
	cancel()
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			return fmt.Errorf("dial: hub refused the credentials (HTTP %d): %w", resp.StatusCode, err)
		}
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)

	var writeMu sync.Mutex
	write := func(f Frame) error {
		data, _ := json.Marshal(f)
		writeMu.Lock()
		defer writeMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return conn.Write(wctx, websocket.MessageText, data)
	}
	hello := cfg.Hello
	if err := write(Frame{Type: FrameHello, Hello: &hello}); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	cfg.Log.Info("connected to hub", "url", cfg.URL, "tools", len(hello.Tools))
	if cfg.OnState != nil {
		cfg.OnState(true)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var f Frame
		if json.Unmarshal(data, &f) != nil || f.Type != FrameRequest || f.Request == nil {
			continue
		}
		req := *f.Request
		go func() {
			timeout := time.Duration(req.TimeoutMS) * time.Millisecond
			if timeout <= 0 || timeout > 5*time.Minute {
				timeout = time.Minute
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			started := time.Now()
			result, err := handle(cctx, req.Tool, req.Args)
			resp := Response{ID: req.ID, OK: err == nil, Result: result}
			if err != nil {
				resp.Error = err.Error()
			}
			cfg.Log.Info("tool call", "tool", req.Tool, "ok", err == nil, "ms", time.Since(started).Milliseconds(), "bytes", len(result))
			if werr := write(Frame{Type: FrameResponse, Response: &resp}); werr != nil {
				cfg.Log.Warn("response not sent", "tool", req.Tool, "err", werr)
			}
		}()
	}
}
