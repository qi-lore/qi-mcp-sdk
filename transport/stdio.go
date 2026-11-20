package transport

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// This file implements the stdio transport from contracts/transport.md Section 3
// (FR-019, FR-020, R-005, V20) according to the framing decisions recorded in
// docs/decisions/0003-stdio-framing.md:
//
//   - Framing: one JSON-RPC frame per newline-terminated line, in both directions.
//   - Bounded read: the maximum line length is enforced by the read primitive
//     (ReadSlice, whose ErrBufferFull is observable), never by ReadBytes, which
//     swallows ErrBufferFull and would let a peer force unbounded allocation.
//   - Parse error: a line that is not valid JSON, or that exceeds the bound, is
//     discarded and answered with a JSON-RPC -32700 parse error the transport emits
//     on its writer, serialised with Send under the same lock. The read loop continues.
//   - Validation scope: framing only. A line that is valid JSON but is not a JSON-RPC
//     request is delivered verbatim to Receive; JSON-RPC semantics belong to the codec
//     (decision 0002) and the server.
//
// The transport never writes diagnostics: under stdio, stdout carries the protocol, so
// the only bytes written to w are the frames Send is handed and the parse errors above
// (FR-057, NFR-011).

// defaultMaxLineBytes is the default maximum accepted line length (R-019): 4 MiB, one
// bound applied uniformly across the stdio, HTTP, and SSE transports.
const defaultMaxLineBytes = 4 << 20

// stdioConfig holds the resolved stdio options. It is unexported because StdioOption
// is the only public knob (contracts/transport.md Section 2).
type stdioConfig struct {
	maxLineBytes int
}

// StdioOption configures the transport Stdio returns.
type StdioOption func(*stdioConfig)

// WithMaxLineBytes sets the maximum accepted line length in bytes (V20). The default is
// 4 MiB; a value that is not positive leaves the default in place.
func WithMaxLineBytes(n int) StdioOption {
	return func(c *stdioConfig) {
		c.maxLineBytes = n
	}
}

// parseErrorResponse is the JSON-RPC error the transport writes for a line it cannot
// frame: a parse error has no request id, so id is null and the code is -32700. The
// message text is not part of the contract.
var parseErrorResponse = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`)

// errLineTooLong marks a line that exceeded the configured maximum and whose remainder
// has already been discarded up to the next newline.
var errLineTooLong = errors.New("transport: line exceeds maximum length")

// stdio is the newline-framed, stdlib-only core.Transport over an io.Reader/io.Writer
// pair. Send and Receive are independent: a Send is satisfied by writing to w even when
// the read side has reached EOF (stdin EOF does not close stdout).
type stdio struct {
	cfg stdioConfig
	r   io.Reader
	w   io.Writer

	// writeMu serialises every write to w — Send frames and parse errors alike — so two
	// writes cannot interleave bytes within a line.
	writeMu sync.Mutex

	// mu guards closed and termErr.
	mu      sync.Mutex
	closed  bool
	termErr error

	// done is closed by Close to unblock pending Send, Receive, and the read loop.
	done chan struct{}

	// startOnce starts the single read goroutine on the first Receive, so the transport
	// owns no goroutines until then (contracts/core-api.md "Package transport").
	startOnce sync.Once
	frames    chan []byte
}

// Stdio returns a Transport that reads newline-delimited JSON-RPC frames from r and
// writes them to w (FR-020). It owns no goroutines until Receive is called and releases
// them on Close.
func Stdio(r io.Reader, w io.Writer, opts ...StdioOption) core.Transport {
	cfg := stdioConfig{maxLineBytes: defaultMaxLineBytes}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.maxLineBytes <= 0 {
		cfg.maxLineBytes = defaultMaxLineBytes
	}
	return &stdio{
		cfg:    cfg,
		r:      r,
		w:      w,
		done:   make(chan struct{}),
		frames: make(chan []byte),
	}
}

// Send writes one complete frame followed by a terminating newline, serialised with
// every other write so frames cannot interleave (transport.md Section 3, V20).
func (s *stdio) Send(ctx context.Context, frame []byte) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: send: %w", core.ErrTransport, err)
	}
	return s.write(frame)
}

// Receive returns the next framed line, with its terminator stripped. It returns a
// typed transport error — matchable with errors.Is(err, core.ErrTransport) — once the
// read side has reached EOF or errored and after Close (transport.md Section 3).
func (s *stdio) Receive(ctx context.Context) ([]byte, error) {
	if s.isClosed() {
		return nil, s.closedErr()
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: receive: %w", core.ErrTransport, err)
	}

	s.startOnce.Do(func() { go s.readLoop() })

	select {
	case <-s.done:
		return nil, s.closedErr()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: receive: %w", core.ErrTransport, ctx.Err())
	case line, ok := <-s.frames:
		if ok {
			return line, nil
		}
		return nil, s.terminalErr()
	}
}

// Close releases the transport's resources and unblocks pending Send and Receive calls.
// It is idempotent and does not block.
func (s *stdio) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	close(s.done)

	// If the caller handed us a closable reader, closing it unblocks the read goroutine
	// from a pending ReadSlice; otherwise the goroutine ends when the underlying read
	// returns. Either way Receive is already unblocked by done.
	if c, ok := s.r.(io.Closer); ok {
		_ = c.Close()
	}
	return nil
}

// readLoop is the single reader goroutine: it reads, validates framing, and either
// delivers a line to Receive or emits a parse error on the writer. It closes s.frames
// on exit so a waiting Receive observes the terminal error.
func (s *stdio) readLoop() {
	defer close(s.frames)

	br := bufio.NewReaderSize(s.r, s.cfg.maxLineBytes+1)
	for {
		line, err := s.readLine(br)
		if err != nil {
			if errors.Is(err, errLineTooLong) {
				if !s.emitParseError() {
					return
				}
				continue
			}
			// EOF or a reader failure: the read side is spent.
			s.setTerminal(err)
			return
		}

		// Framing only: a line that is not valid JSON is a parse error; a valid-JSON line
		// that is not a JSON-RPC request is delivered as-is (decision 0002, decision 0003).
		if !json.Valid(line) {
			if !s.emitParseError() {
				return
			}
			continue
		}

		if !s.deliver(line) {
			return
		}
	}
}

// readLine reads one newline-terminated line, bounded by the configured maximum. It
// returns the line without its terminator, errLineTooLong when the bound is exceeded
// (having discarded the remainder of that line), or the underlying read error (io.EOF at
// end of stream). The returned slice never aliases the bufio buffer.
//
// The bound is enforced by ReadSlice, whose ErrBufferFull is observable; ReadBytes would
// swallow it and buffer an unbounded line (decision 0003).
func (s *stdio) readLine(br *bufio.Reader) ([]byte, error) {
	chunk, err := br.ReadSlice('\n')
	switch {
	case err == nil:
		line := make([]byte, len(chunk)-1)
		copy(line, chunk[:len(chunk)-1])
		return line, nil
	case errors.Is(err, bufio.ErrBufferFull):
		if derr := discardLine(br); derr != nil && !errors.Is(derr, io.EOF) {
			return nil, derr
		}
		return nil, errLineTooLong
	default:
		// io.EOF (or a reader error). Any partial, unterminated fragment is discarded: a
		// frame must be newline-terminated.
		return nil, err
	}
}

// discardLine drops the remainder of the current line up to and including the next
// newline, returning io.EOF if the stream ends first.
func discardLine(br *bufio.Reader) error {
	for {
		_, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			return nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return err
		}
	}
}

// deliver hands a framed line to Receive, returning false once Close has been called.
func (s *stdio) deliver(line []byte) bool {
	select {
	case s.frames <- line:
		return true
	case <-s.done:
		return false
	}
}

// write serialises one frame plus its newline terminator onto w, refusing to write once
// the transport is closed.
func (s *stdio) write(frame []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if s.isClosed() {
		return s.closedErr()
	}

	buf := make([]byte, len(frame)+1)
	copy(buf, frame)
	buf[len(frame)] = '\n'
	if _, err := s.w.Write(buf); err != nil {
		return fmt.Errorf("%w: write frame: %w", core.ErrTransport, err)
	}
	return nil
}

// emitParseError writes the -32700 parse error for a discarded line and reports whether
// the read loop should continue.
func (s *stdio) emitParseError() bool {
	if err := s.write(parseErrorResponse); err != nil {
		s.setTerminal(err)
		return false
	}
	return true
}

func (s *stdio) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *stdio) setTerminal(err error) {
	s.mu.Lock()
	s.termErr = err
	s.mu.Unlock()
}

// terminalErr reports why the read side ended, as a typed transport error.
func (s *stdio) terminalErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.termErr == nil {
		return s.closedErr()
	}
	return fmt.Errorf("%w: %w", core.ErrTransport, s.termErr)
}

// closedErr is the typed error every post-Close operation returns.
func (s *stdio) closedErr() error {
	return fmt.Errorf("%w: transport closed", core.ErrTransport)
}
