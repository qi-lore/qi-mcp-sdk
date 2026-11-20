package transport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// This file implements the bounded, hand-written SSE reader and writer of
// contracts/transport.md Section 5 (R-006, FR-021, V20), with the unexported surface
// and semantics fixed by docs/decisions/0005-sse-surface.md:
//
//   - The standard library has no SSE support and the zero-dependency rule forbids a
//     library (R-006), so this is a hand-written implementation over bufio.
//   - The surface is internal to transport: its only consumers are the Streamable HTTP
//     handler (http.go, T037) and dialer (httpdial.go, T039), both in-package.
//   - The reader parses event/data/id/retry, joins multiple data lines of one event with
//     "\n", ignores comment lines beginning with ":", discards an incomplete final event
//     at EOF (reporting io.EOF), and enforces a maximum event size as a typed transport
//     error. A block carrying no data field dispatches nothing.
//   - The writer emits well-formed events, flushes after each through interface{ Flush() },
//     enforces a maximum write size per event, and serialises concurrent emissions.
//   - Both bounds default to 4 MiB (R-019) and are configured by withMaxEventBytes; a
//     non-positive value leaves the default in place (decision 0004, mirrored).
//
// Turning the reader's io.EOF into a typed transport error is the enclosing transport's
// job (transport.md Section 1), not the reader's; an event over the bound is a typed error
// wrapping core.ErrTransport on both sides.

// defaultMaxEventBytes is the default maximum size of a single SSE event (R-019): 4 MiB,
// the same bound applied uniformly across the stdio, HTTP, and SSE transports.
const defaultMaxEventBytes = 4 << 20

// sseReadBufferSize is the size of the bufio buffer the reader reads lines through. A
// line longer than this arrives in fragments that readSSELine accumulates while checking
// the event bound, so a peer cannot force unbounded allocation with one long line.
const sseReadBufferSize = 4096

// sseConfig holds the resolved SSE options, shared by the reader and the writer.
type sseConfig struct {
	maxEventBytes int
}

// sseOption configures the reader or writer newSSEReader/newSSEWriter return.
type sseOption func(*sseConfig)

// withMaxEventBytes bounds the size of a single event, for both the reader and the
// writer. A value that is not positive leaves the 4 MiB default in place (R-019,
// decision 0004).
func withMaxEventBytes(n int) sseOption {
	return func(c *sseConfig) {
		c.maxEventBytes = n
	}
}

// sseEvent is one decoded server-sent event (decision 0005).
type sseEvent struct {
	Event string // "event" field; "" is the default "message" type
	Data  string // the event's "data" fields joined with "\n"
	ID    string // the most recent "id" field
	Retry int    // the "retry" field in milliseconds; 0 when absent
}

// errEventTooLarge marks an event that exceeded the configured maximum.
var errEventTooLarge = errors.New("transport: sse event exceeds maximum size")

// sseReader parses a server-sent event stream from r. It is not safe for concurrent use
// by multiple goroutines; the enclosing transport owns the single reading goroutine.
type sseReader struct {
	br  *bufio.Reader
	cfg sseConfig

	// err is a sticky terminal error: once the stream ends (io.EOF) or an event exceeds
	// the bound, every later Next returns it rather than resuming mid-stream.
	err error
}

// newSSEReader returns a reader that parses SSE events from r (transport.md Section 5).
func newSSEReader(r io.Reader, opts ...sseOption) *sseReader {
	cfg := sseConfig{maxEventBytes: defaultMaxEventBytes}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.maxEventBytes <= 0 {
		cfg.maxEventBytes = defaultMaxEventBytes
	}
	return &sseReader{
		br:  bufio.NewReaderSize(r, sseReadBufferSize),
		cfg: cfg,
	}
}

// Next returns the next event from the stream. It reports io.EOF once the stream ends; an
// incomplete final event is discarded, never yielded. An event that exceeds the configured
// maximum size is a typed transport error wrapping core.ErrTransport. A block carrying no
// data field dispatches nothing, so Next advances to the next real event or to io.EOF.
func (r *sseReader) Next() (sseEvent, error) {
	if r.err != nil {
		return sseEvent{}, r.err
	}
	ev, err := r.readEvent()
	if err != nil {
		r.err = err
	}
	return ev, err
}

// readEvent reads lines until it has a complete, dispatchable event, the stream ends, or
// the bound is exceeded. The field state is local to one event and reset after each
// dispatch, as the SSE grammar requires.
func (r *sseReader) readEvent() (sseEvent, error) {
	var (
		data      []byte
		eventType string
		lastID    string
		retry     int
		size      int
	)

	for {
		line, err := readSSELine(r.br, r.cfg.maxEventBytes)
		if err != nil {
			switch {
			case errors.Is(err, errEventTooLarge):
				return sseEvent{}, fmt.Errorf("%w: %w", core.ErrTransport, err)
			case errors.Is(err, io.EOF):
				// End of stream: an event that was never closed by a blank line is
				// incomplete and is discarded, never yielded (transport.md Section 5).
				return sseEvent{}, io.EOF
			default:
				return sseEvent{}, err
			}
		}

		// Bound the whole event, not a single line: many small fields that accumulate
		// past the maximum are rejected just as one oversized field is.
		size += len(line) + 1
		if size > r.cfg.maxEventBytes {
			return sseEvent{}, fmt.Errorf("%w: %w", core.ErrTransport, errEventTooLarge)
		}

		line = bytes.TrimSuffix(line, []byte("\r"))

		if len(line) == 0 {
			// A blank line dispatches the event. A block that carried no data field
			// (for example a run of comment lines) dispatches nothing.
			if len(data) == 0 {
				continue
			}
			return sseEvent{
				Event: eventType,
				Data:  string(data[:len(data)-1]), // drop the trailing separator
				ID:    lastID,
				Retry: retry,
			}, nil
		}

		name, value := splitSSEField(line)
		switch name {
		case "event":
			eventType = value
		case "data":
			data = append(data, value...)
			data = append(data, '\n')
		case "id":
			lastID = value
		case "retry":
			if n, ok := parseSSERetry(value); ok {
				retry = n
			}
		}
		// Unknown field names and comments (":") are ignored, as the SSE grammar says.
	}
}

// readSSELine reads one newline-terminated line, returning it without its terminator. It
// returns errEventTooLarge when the line exceeds limit bytes (so a single long line cannot
// force unbounded allocation) and io.EOF at end of stream, discarding any unterminated
// final fragment. The returned slice never aliases the bufio buffer.
func readSSELine(br *bufio.Reader, limit int) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		switch {
		case err == nil:
			if buf == nil {
				line := make([]byte, len(chunk)-1)
				copy(line, chunk[:len(chunk)-1])
				return line, nil
			}
			return append(buf, chunk[:len(chunk)-1]...), nil
		case errors.Is(err, bufio.ErrBufferFull):
			buf = append(buf, chunk...)
			if len(buf) > limit {
				return nil, errEventTooLarge
			}
		default:
			// io.EOF (or a reader error): a line must be newline-terminated, so any
			// partial fragment is discarded.
			return nil, err
		}
	}
}

// splitSSEField splits a line into its field name and value per the SSE grammar. A line
// beginning with ":" is a comment and yields an empty name (ignored by the caller); a line
// with no colon is a field with an empty value; a single space after the colon is stripped
// from the value.
func splitSSEField(line []byte) (name, value string) {
	if len(line) > 0 && line[0] == ':' {
		return "", ""
	}
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return string(line), ""
	}
	v := line[i+1:]
	if len(v) > 0 && v[0] == ' ' {
		v = v[1:]
	}
	return string(line[:i]), string(v)
}

// parseSSERetry parses a retry field value, which must be an all-digit integer; any other
// value is ignored (the previous retry, or the zero default, stands).
func parseSSERetry(value string) (int, bool) {
	if value == "" {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sseWriter emits server-sent events to w. It is safe for concurrent use: emissions are
// serialised so events cannot interleave (transport.md Section 5, V20).
type sseWriter struct {
	mu  sync.Mutex
	w   io.Writer
	cfg sseConfig
}

// newSSEWriter returns a writer that emits SSE events to w (transport.md Section 5).
func newSSEWriter(w io.Writer, opts ...sseOption) *sseWriter {
	cfg := sseConfig{maxEventBytes: defaultMaxEventBytes}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.maxEventBytes <= 0 {
		cfg.maxEventBytes = defaultMaxEventBytes
	}
	return &sseWriter{w: w, cfg: cfg}
}

// Write emits one well-formed event and flushes it. It returns a typed transport error
// wrapping core.ErrTransport when the encoded event exceeds the configured maximum size or
// the underlying write fails. Concurrent calls are serialised.
func (w *sseWriter) Write(ev sseEvent) error {
	frame := encodeSSEEvent(ev)
	if len(frame) > w.cfg.maxEventBytes {
		return fmt.Errorf("%w: %w", core.ErrTransport, errEventTooLarge)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := w.w.Write(frame); err != nil {
		return fmt.Errorf("%w: write sse event: %w", core.ErrTransport, err)
	}
	// Flush through interface{ Flush() } — the shape http.Flusher satisfies — so an
	// http.ResponseWriter streams the event immediately while a plain io.Writer (such as
	// a bytes.Buffer) is written unflushed (decision 0005).
	if f, ok := w.w.(interface{ Flush() }); ok {
		f.Flush()
	}
	return nil
}

// encodeSSEEvent renders an event in SSE wire format. The canonical minimal event (a lone
// data field) is "data: <value>\n\n"; a zero event, id, or retry emits no line; and
// multi-line data becomes one data line per line. Field order is not normative, but the
// output always round-trips through the reader unchanged.
func encodeSSEEvent(ev sseEvent) []byte {
	var b bytes.Buffer
	if ev.Event != "" {
		b.WriteString("event: ")
		b.WriteString(ev.Event)
		b.WriteByte('\n')
	}
	for _, line := range splitSSEData(ev.Data) {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	if ev.ID != "" {
		b.WriteString("id: ")
		b.WriteString(ev.ID)
		b.WriteByte('\n')
	}
	if ev.Retry > 0 {
		b.WriteString("retry: ")
		b.WriteString(strconv.Itoa(ev.Retry))
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return b.Bytes()
}

// splitSSEData splits an event's data into the lines the writer emits one data field per.
// An empty data value yields a single empty line, so an event always carries a data field
// and round-trips.
func splitSSEData(data string) []string {
	return strings.Split(data, "\n")
}
