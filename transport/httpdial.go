package transport

import (
	"bytes"
	"context"
	"errors"
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
// The context and failure semantics decision 0009 left implicit are fixed in
// docs/decisions/0010-streamable-http-dialer-failure.md:
//
//   - Send's context bounds the request only until the response header arrives, the phase
//     in which Send blocks; once Send returns, the body is read under the transport's own
//     lifetime. Receive's context bounds only that Receive call. A caller's context
//     typically ends when Send returns, so binding the whole body read to it would abort
//     the stream the moment Send returned and defeat R-016's streaming.
//   - A body that ends normally (EOF) contributes no further frames and is not an error: a
//     JSON body is one frame (or none, when empty), and an SSE body's stream is simply
//     finished. A body that fails to read, or exceeds the 4 MiB bound, is a transport
//     failure: the dialer fails terminally and every later Send and Receive returns a
//     typed error matchable with errors.Is(err, core.ErrTransport), rather than Receive
//     blocking forever (transport.md Section 1). The framed channel carries no request
//     identity, so the failure is transport-wide.
//
// Send returns once the response header has arrived and hands the body to a reader
// goroutine, so a long-lived SSE stream yields its first frame without Send blocking on
// the whole body. The reader goroutines are owned by the transport and released on Close
// or on a terminal failure (R-017, V20): the transport's context is cancelled by Close or
// by the first body failure, which aborts the in-flight requests. Nothing is ever written
// to standard output (FR-057, NFR-011).

// dialAcceptHeader is the Accept value the binding requires of a client: it negotiates a
// JSON body or an SSE body (transport.md Section 4). It is a private constant because the
// package's test files already declare the same literal under their own name.
const dialAcceptHeader = "application/json, text/event-stream"

// errResponseBodyTooLarge marks a JSON response body that exceeded the configured bound.
var errResponseBodyTooLarge = errors.New("transport: http response body exceeds maximum size")

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
	// request and unblocks the reader goroutines; a body failure cancels it too, carrying
	// the read error as the cancellation cause. It is deliberately not derived from a
	// caller's Send context, whose lifetime ends when Send returns while the response
	// body may still be streaming (decision 0010).
	ctx, cancel := context.WithCancelCause(context.Background())
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

	// ctx is the transport's lifetime: it is cancelled by Close, or by the first body
	// failure, and bounds every request. Its cancellation cause is the typed transport
	// error Send and Receive report (closedErr for Close, the read error otherwise).
	ctx    context.Context
	cancel context.CancelCauseFunc

	// frames carries the frames the response bodies produce. Receive is the single
	// consumer (core.Transport allows at most one receiving goroutine).
	frames chan []byte

	// wg tracks the body-reading goroutines, so they are accounted for across a failure.
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
	if err := d.doneErr(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: send: %w", core.ErrTransport, err)
	}

	// The request lives under the transport's context so Close — or a body failure —
	// aborts it, while the caller's context cancels it only for as long as Send is
	// blocked: stop() detaches the caller's context on return, leaving the body to be
	// streamed afterwards (decision 0010).
	reqCtx, reqCancel := context.WithCancel(d.ctx)
	stop := context.AfterFunc(ctx, reqCancel)
	defer stop()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, d.endpoint.String(), bytes.NewReader(frame))
	if err != nil {
		reqCancel()
		return fmt.Errorf("%w: send: %w", core.ErrTransport, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", dialAcceptHeader)

	resp, err := d.client.Do(req)
	if err != nil {
		reqCancel()
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
		defer reqCancel()
		defer func() { _ = resp.Body.Close() }()
		d.readResponseBody(resp)
	}()
	return nil
}

// Receive returns the next frame a response body produced. It blocks until a frame is
// available, the caller's context is done, or the transport ends — on Close or on a body
// failure — after which it returns a typed transport error rather than blocking forever
// (transport.md Section 1).
func (d *httpDialer) Receive(ctx context.Context) ([]byte, error) {
	if err := d.doneErr(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: receive: %w", core.ErrTransport, err)
	}

	select {
	case <-d.ctx.Done():
		return nil, d.ctxErr()
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

	// The first cancellation wins: if a body failure already ended the transport, the
	// read error stays the reported cause (context.CancelCauseFunc ignores later calls).
	d.cancel(fmt.Errorf("%w: transport closed", core.ErrTransport))
	return nil
}

// readResponseBody turns one response body into frames on d.frames, choosing the shape
// from the response Content-Type (decision 0009). A normal end of body delivers no further
// frames; a read failure or an over-bound body fails the transport (decision 0010).
func (d *httpDialer) readResponseBody(resp *http.Response) {
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		d.streamSSEBody(resp.Body)
		return
	}
	d.deliverJSONBody(resp.Body)
}

// streamSSEBody delivers one frame per event, in order, as the stream produces them
// (R-016). End of stream ends this body's frames and is not an error; any other read
// failure — including the reader's own over-bound typed error (transport.md Section 5) —
// fails the transport.
func (d *httpDialer) streamSSEBody(r io.Reader) {
	reader := newSSEReader(r)
	for {
		ev, err := reader.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			if !errors.Is(err, core.ErrTransport) {
				err = fmt.Errorf("%w: read response body: %w", core.ErrTransport, err)
			}
			d.fail(err)
			return
		}
		if !d.deliver([]byte(ev.Data)) {
			return
		}
	}
}

// deliverJSONBody delivers a JSON response body as exactly one frame, and an empty body —
// the 202 a notification receives — as no frame. A read failure fails the transport; the
// body is bounded by the same 4 MiB default as the server side, and an over-bound body
// fails the transport rather than being dropped silently (R-019, decision 0010).
func (d *httpDialer) deliverJSONBody(r io.Reader) {
	body, err := io.ReadAll(io.LimitReader(r, defaultMaxBodyBytes+1))
	if err != nil {
		d.fail(fmt.Errorf("%w: read response body: %w", core.ErrTransport, err))
		return
	}
	if int64(len(body)) > defaultMaxBodyBytes {
		d.fail(fmt.Errorf("%w: %w", core.ErrTransport, errResponseBodyTooLarge))
		return
	}
	if len(body) == 0 {
		return
	}
	d.deliver(body)
}

// deliver hands one frame to Receive, returning false once the transport has ended so the
// reader goroutine cannot block forever on an abandoned transport.
func (d *httpDialer) deliver(frame []byte) bool {
	select {
	case d.frames <- frame:
		return true
	case <-d.ctx.Done():
		return false
	}
}

// fail ends the transport with err as the cancellation cause, so every later Send and
// Receive reports it. The first cause wins, so a failure never masks Close, nor the
// reverse (context.CancelCauseFunc ignores later calls).
func (d *httpDialer) fail(err error) {
	d.cancel(err)
}

// doneErr reports the transport's terminal error, or nil while it is live.
func (d *httpDialer) doneErr() error {
	if d.ctx.Err() == nil {
		return nil
	}
	return d.ctxErr()
}

// ctxErr reports why the transport ended: the cancellation cause Close or fail set.
func (d *httpDialer) ctxErr() error {
	if cause := context.Cause(d.ctx); cause != nil {
		return cause
	}
	return d.closedErr()
}

// closedErr is the fallback typed error when the transport ended without a cause.
func (d *httpDialer) closedErr() error {
	return fmt.Errorf("%w: transport closed", core.ErrTransport)
}
