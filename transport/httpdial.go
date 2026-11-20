package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// This file implements the client side of the Streamable HTTP binding from
// contracts/transport.md Section 4 (FR-010, FR-021, R-005, R-016), the mirror of the
// server side in http.go, with the correlation, header, and endpoint-validation choices
// recorded in docs/decisions/0009-streamable-http-dialer.md:
//
//   - One Send, one POST. Send issues exactly one HTTP POST to the endpoint, carrying the
//     frame as the request body with Content-Type: application/json and
//     Accept: application/json, text/event-stream. It carries no session-identifier
//     header: a modern request puts all protocol context in per-request _meta
//     (wire-protocol.md Section 9, FR-003, FR-055).
//   - The response body's frames are read through Receive: a Content-Type:
//     application/json body is exactly one frame; a Content-Type: text/event-stream body
//     is one frame per event, delivered as the stream produces them (streamed, not
//     buffered; R-016); and an empty body — the 202 a notification receives — contributes
//     no frame.
//   - The endpoint is validated up front: a string that is not an absolute URL is
//     rejected by the constructor with an error matchable with
//     errors.Is(err, core.ErrTransport), rather than deferring the failure to the first
//     Send.
//
// Send returns once the response header has arrived and hands the body to a reader
// goroutine, so a long-lived SSE stream yields its first frame without Send blocking on
// the whole body. The reader goroutines are owned by the transport and released on Close
// or on context cancellation (R-017, V20): Close cancels the transport context, which
// aborts the in-flight requests. Nothing is ever written to standard output (FR-057,
// NFR-011).

// dialAcceptHeader is the Accept value the binding requires of a client: it negotiates a
// JSON body or an SSE body (transport.md Section 4). It is a private constant because the
// package's test files already declare the same literal under their own name.
const dialAcceptHeader = "application/json, text/event-stream"

// dialConfig holds the resolved Streamable HTTP dialer options. It is unexported because
// HTTPDialOption is the only public knob (contracts/transport.md Section 2).
type dialConfig struct {
	client *http.Client
}

// HTTPDialOption configures the transport StreamableHTTPDial returns.
type HTTPDialOption func(*dialConfig)

// WithHTTPClient sets the *http.Client the dialer issues its requests through, for
// example one whose transport attaches a bearer token obtained from a core.TokenSource
// (transport.md Section 2). A nil client leaves http.DefaultClient in place.
func WithHTTPClient(c *http.Client) HTTPDialOption {
	return func(cfg *dialConfig) {
		cfg.client = c
	}
}

// StreamableHTTPDial returns a client-side Transport over the Streamable HTTP binding
// against one endpoint (FR-010, FR-021). The endpoint must be an absolute URL; a string
// it cannot use is rejected here with an error matchable with
// errors.Is(err, core.ErrTransport) and a nil transport (decision 0009).
func StreamableHTTPDial(endpoint string, opts ...HTTPDialOption) (core.Transport, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid endpoint %q: %w", core.ErrTransport, endpoint, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%w: endpoint %q is not an absolute URL", core.ErrTransport, endpoint)
	}

	cfg := dialConfig{client: http.DefaultClient}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.client == nil {
		cfg.client = http.DefaultClient
	}

	// ctx is the transport's lifetime: Close cancels it, which aborts every in-flight
	// request and unblocks the reader goroutines. It is deliberately not derived from a
	// caller's Send context, whose lifetime ends when Send returns while the response
	// body may still be streaming.
	ctx, cancel := context.WithCancel(context.Background())
	return &httpDialer{
		endpoint: u,
		client:   cfg.client,
		ctx:      ctx,
		cancel:   cancel,
		frames:   make(chan []byte),
	}, nil
}

// httpDialer is the client-side Streamable HTTP core.Transport. Its reader goroutines
// funnel every response body's frames into one channel, from which Receive reads them.
type httpDialer struct {
	endpoint *url.URL
	client   *http.Client

	// ctx is cancelled by Close; it bounds every request and unblocks Receive, the
	// reader goroutines, and a Send waiting on the response header.
	ctx    context.Context
	cancel context.CancelFunc

	// frames carries the frames the response bodies produce. Receive is the single
	// consumer (core.Transport allows at most one receiving goroutine).
	frames chan []byte

	// wg tracks the body-reading goroutines, so they are accounted for across Close.
	wg sync.WaitGroup

	// mu guards closed.
	mu     sync.Mutex
	closed bool
}

// Send issues one HTTP POST carrying frame as the body and makes the response body's
// frames available through Receive (decision 0009). It blocks until the response header
// has arrived, the caller's context is done, or the transport is closed; the body is
// streamed by a goroutine the transport owns, so a long-lived SSE response yields its
// first frame without Send blocking on the whole body (R-016).
func (d *httpDialer) Send(ctx context.Context, frame []byte) error {
	if d.isClosed() {
		return d.closedErr()
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: send: %w", core.ErrTransport, err)
	}

	// The request lives under the transport's context so Close aborts it, while the
	// caller's context cancels it only for as long as Send is blocked: stop() detaches
	// the caller's context on return, leaving the body to be streamed afterwards.
	reqCtx, cancel := context.WithCancel(d.ctx)
	stop := context.AfterFunc(ctx, cancel)
	defer stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.endpoint.String(), bytes.NewReader(frame))
	if err != nil {
		cancel()
		return fmt.Errorf("%w: send: %w", core.ErrTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", dialAcceptHeader)

	resp, err := d.client.Do(req)
	if err != nil {
		cancel()
		// A cancelled caller context surfaces as the caller's error, not the round
		// trip's incidental one, so the caller can tell the two apart.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("%w: send: %w", core.ErrTransport, ctxErr)
		}
		return fmt.Errorf("%w: send: %w", core.ErrTransport, err)
	}

	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer cancel()
		defer func() { _ = resp.Body.Close() }()
		d.readResponseBody(resp)
	}()
	return nil
}

// Receive returns the next frame a response body produced. It blocks until a frame is
// available, the caller's context is done, or the transport is closed; after Close it
// returns a typed transport error rather than blocking (transport.md Section 1).
func (d *httpDialer) Receive(ctx context.Context) ([]byte, error) {
	if d.isClosed() {
		return nil, d.closedErr()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: receive: %w", core.ErrTransport, err)
	}

	select {
	case <-d.ctx.Done():
		return nil, d.closedErr()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: receive: %w", core.ErrTransport, ctx.Err())
	case frame := <-d.frames:
		return frame, nil
	}
}

// Close releases the transport's resources and unblocks pending Send and Receive calls.
// It cancels the transport context, which aborts in-flight requests and ends the reader
// goroutines. It is idempotent and does not block (transport.md Section 1).
func (d *httpDialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	d.mu.Unlock()

	d.cancel()
	return nil
}

// readResponseBody turns one response body into frames on d.frames, choosing the shape
// from the response Content-Type (decision 0009).
func (d *httpDialer) readResponseBody(resp *http.Response) {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		d.streamSSEBody(resp.Body)
		return
	}
	d.deliverJSONBody(resp.Body)
}

// streamSSEBody delivers one frame per event, in order, as the stream produces them
// (R-016). It returns at end of stream or on the first read error; nothing further is
// delivered from this body.
func (d *httpDialer) streamSSEBody(r io.Reader) {
	reader := newSSEReader(r)
	for {
		ev, err := reader.Next()
		if err != nil {
			return
		}
		if !d.deliver([]byte(ev.Data)) {
			return
		}
	}
}

// deliverJSONBody delivers a JSON response body as exactly one frame, and an empty body —
// the 202 a notification receives — as no frame. The body is bounded by the same 4 MiB
// default as the server side; a body over the bound is dropped rather than delivered
// (R-019).
func (d *httpDialer) deliverJSONBody(r io.Reader) {
	body, err := io.ReadAll(io.LimitReader(r, defaultMaxBodyBytes+1))
	if err != nil {
		return
	}
	if int64(len(body)) > defaultMaxBodyBytes || len(body) == 0 {
		return
	}
	d.deliver(body)
}

// deliver hands one frame to Receive, returning false once the transport is closed so the
// reader goroutine cannot block forever on an abandoned transport.
func (d *httpDialer) deliver(frame []byte) bool {
	select {
	case d.frames <- frame:
		return true
	case <-d.ctx.Done():
		return false
	}
}

func (d *httpDialer) isClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// closedErr is the typed error every post-Close operation returns.
func (d *httpDialer) closedErr() error {
	return fmt.Errorf("%w: transport closed", core.ErrTransport)
}
