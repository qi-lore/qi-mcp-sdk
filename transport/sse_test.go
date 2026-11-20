package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// These tests pin the bounded SSE reader and writer contract of transport.md
// Section 5 (R-006, FR-021, V20): the reader parses the event/data/id/retry fields,
// joins the multiple data lines of one event with "\n", ignores comment lines
// beginning with ":", discards an incomplete final event at EOF, and enforces a
// maximum event size (default 4 MiB, R-019) as a typed transport error; the writer
// emits well-formed events, flushes after each one, enforces a maximum write size per
// event as a typed transport error, and serialises concurrent emissions so events
// cannot interleave.
//
// The standard library has no SSE support and the zero-dependency rule forbids a
// library (R-006), so transport ships this hand-written implementation over bufio.
// The reader and writer are internal to transport — http.go (T037) and httpdial.go
// (T039) are their only consumers — so the surface pinned here is unexported. This
// file is written first and is expected to fail to compile until sse.go (T035) lands;
// it names exactly the symbols T035 owes and nothing else.
var (
	_ func(io.Reader, ...sseOption) *sseReader = newSSEReader
	_ func(io.Writer, ...sseOption) *sseWriter = newSSEWriter
	_ func(int) sseOption                      = withMaxEventBytes
	_ func(*sseReader) (sseEvent, error)       = (*sseReader).Next
	_ func(*sseWriter, sseEvent) error         = (*sseWriter).Write
)

// readEvents drains an sseReader until EOF and returns the events it yielded. It
// fails the test on any error other than io.EOF, so a stream that ends cleanly is
// distinguishable from one that errors.
func readEvents(t *testing.T, r *sseReader) []sseEvent {
	t.Helper()
	var out []sseEvent
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

// flushRecorder is an io.Writer that records how many times Flush was called, so a
// test can assert the writer flushes after each event. It implements the
// interface{ Flush() } shape http.Flusher satisfies.
type flushRecorder struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (f *flushRecorder) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(p)
}

func (f *flushRecorder) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
}

func (f *flushRecorder) flushCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

func (f *flushRecorder) bytes() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.buf.Bytes()...)
}

// TestSSEReaderParsesEventDataIDRetry pins that the reader surfaces all four SSE
// fields named in transport.md Section 5, including the numeric retry field.
func TestSSEReaderParsesEventDataIDRetry(t *testing.T) {
	const in = "event: update\ndata: hello\nid: 42\nretry: 3000\n\n"
	r := newSSEReader(strings.NewReader(in))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Event != "update" {
		t.Errorf("Event = %q, want \"update\"", ev.Event)
	}
	if ev.Data != "hello" {
		t.Errorf("Data = %q, want \"hello\"", ev.Data)
	}
	if ev.ID != "42" {
		t.Errorf("ID = %q, want \"42\"", ev.ID)
	}
	if ev.Retry != 3000 {
		t.Errorf("Retry = %d, want 3000", ev.Retry)
	}
}

// TestSSEReaderJoinsMultilineData pins that several data lines of one event are
// joined with "\n" into a single Data value (transport.md Section 5).
func TestSSEReaderJoinsMultilineData(t *testing.T) {
	const in = "data: line one\ndata: line two\ndata: line three\n\n"
	r := newSSEReader(strings.NewReader(in))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got, want := ev.Data, "line one\nline two\nline three"; got != want {
		t.Errorf("Data = %q, want %q", got, want)
	}
}

// TestSSEReaderIgnoresComments pins that lines beginning with ":" contribute no
// field (transport.md Section 5), and that a block carrying no data field dispatches
// nothing rather than an empty event.
func TestSSEReaderIgnoresComments(t *testing.T) {
	const in = ": a leading comment\ndata: kept\n: a trailing comment\n\n"
	r := newSSEReader(strings.NewReader(in))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.Data != "kept" {
		t.Errorf("Data = %q, want \"kept\"", ev.Data)
	}
	if ev.Event != "" {
		t.Errorf("Event = %q, want empty (a comment is not an event field)", ev.Event)
	}

	t.Run("a comment-only block dispatches nothing", func(t *testing.T) {
		r := newSSEReader(strings.NewReader(": only a comment\n\n"))
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("Next() = %v, want io.EOF (no data field, so no event is dispatched)", err)
		}
	})
}

// TestSSEReaderDeliversEventsInOrder pins that consecutive events in one stream are
// yielded in order, each starting a fresh field set.
func TestSSEReaderDeliversEventsInOrder(t *testing.T) {
	const in = "data: one\n\ndata: two\n\nid: 3\ndata: three\n\n"
	r := newSSEReader(strings.NewReader(in))

	got := readEvents(t, r)
	want := []sseEvent{{Data: "one"}, {Data: "two"}, {Data: "three", ID: "3"}}
	if len(got) != len(want) {
		t.Fatalf("read %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestSSEReaderDiscardsIncompleteFinalEvent pins transport.md Section 5's rule that an
// incomplete final event at EOF is discarded: the stream ends with io.EOF, and the
// partial event is never yielded.
func TestSSEReaderDiscardsIncompleteFinalEvent(t *testing.T) {
	t.Run("no terminator at EOF", func(t *testing.T) {
		r := newSSEReader(strings.NewReader("data: partial"))
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("Next() = %v, want io.EOF (incomplete event discarded)", err)
		}
	})

	t.Run("complete event then incomplete tail", func(t *testing.T) {
		r := newSSEReader(strings.NewReader("data: complete\n\ndata: incomplete\n"))

		ev, err := r.Next()
		if err != nil {
			t.Fatalf("Next() error = %v, want the complete event", err)
		}
		if ev.Data != "complete" {
			t.Errorf("Data = %q, want \"complete\"", ev.Data)
		}
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("second Next() = %v, want io.EOF (incomplete tail discarded)", err)
		}
	})
}

// TestSSEReaderEnforcesMaxEventSize pins transport.md Section 5's bound: an event over
// the maximum is a typed transport error. The bound covers the whole event, not a
// single line, so many small data fields that accumulate past it are rejected too.
func TestSSEReaderEnforcesMaxEventSize(t *testing.T) {
	t.Run("single oversized data field", func(t *testing.T) {
		in := "data: " + strings.Repeat("x", 256) + "\n\n"
		r := newSSEReader(strings.NewReader(in), withMaxEventBytes(64))
		if _, err := r.Next(); !errors.Is(err, core.ErrTransport) {
			t.Fatalf("Next() = %v, want an error matching core.ErrTransport", err)
		}
	})

	t.Run("many small data fields accumulate past the bound", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 10; i++ {
			b.WriteString("data: " + strings.Repeat("y", 32) + "\n")
		}
		b.WriteString("\n")
		r := newSSEReader(strings.NewReader(b.String()), withMaxEventBytes(64))
		if _, err := r.Next(); !errors.Is(err, core.ErrTransport) {
			t.Fatalf("Next() = %v, want an error matching core.ErrTransport", err)
		}
	})
}

// TestSSEReaderDefaultMaxEventBytesIsFourMiB pins the R-019 default: with no bound
// configured, an event just under 4 MiB is delivered and one just over is rejected.
// The 4 KiB margins keep the test off the unfixed boundary.
func TestSSEReaderDefaultMaxEventBytesIsFourMiB(t *testing.T) {
	const fourMiB = 4 << 20

	t.Run("under the default is delivered", func(t *testing.T) {
		payload := strings.Repeat("x", fourMiB-4096)
		r := newSSEReader(strings.NewReader("data: " + payload + "\n\n"))
		ev, err := r.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if len(ev.Data) != len(payload) {
			t.Errorf("Data length = %d, want %d", len(ev.Data), len(payload))
		}
	})

	t.Run("over the default is rejected", func(t *testing.T) {
		payload := strings.Repeat("x", fourMiB+4096)
		r := newSSEReader(strings.NewReader("data: " + payload + "\n\n"))
		if _, err := r.Next(); !errors.Is(err, core.ErrTransport) {
			t.Fatalf("Next() = %v, want an error matching core.ErrTransport", err)
		}
	})
}

// TestSSEReaderTerminalErrorIsSticky pins decision 0006: once Next has reported a terminal
// condition — io.EOF, or a typed error because an event exceeded the bound — the reader is
// exhausted and every later call returns that same error, never resuming mid-stream.
func TestSSEReaderTerminalErrorIsSticky(t *testing.T) {
	t.Run("after io.EOF", func(t *testing.T) {
		r := newSSEReader(strings.NewReader("data: one\n\n"))
		if _, err := r.Next(); err != nil {
			t.Fatalf("first Next: %v", err)
		}
		for i := 0; i < 2; i++ {
			if _, err := r.Next(); !errors.Is(err, io.EOF) {
				t.Fatalf("Next() after EOF = %v, want io.EOF (terminal error is sticky)", err)
			}
		}
	})

	t.Run("after a bound error", func(t *testing.T) {
		// The oversized event is followed by a well-formed one; a reader that resumed
		// mid-stream would surface "after" as a fresh event instead of staying exhausted.
		in := "data: " + strings.Repeat("x", 256) + "\n\n" + "data: after\n\n"
		r := newSSEReader(strings.NewReader(in), withMaxEventBytes(64))

		if _, err := r.Next(); !errors.Is(err, core.ErrTransport) {
			t.Fatalf("first Next() = %v, want an error matching core.ErrTransport", err)
		}
		if _, err := r.Next(); !errors.Is(err, core.ErrTransport) {
			t.Fatalf("second Next() = %v, want the same typed error (sticky), not the next event", err)
		}
	})
}

// TestSSEWriterEmitsDataOnlyEvent pins the exact bytes of the canonical minimal event:
// a lone data field terminated by the blank line that dispatches it. A zero-valued
// event, id, and retry emit no line.
func TestSSEWriterEmitsDataOnlyEvent(t *testing.T) {
	var buf bytes.Buffer
	w := newSSEWriter(&buf)

	if err := w.Write(sseEvent{Data: "hello"}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got, want := buf.String(), "data: hello\n\n"; got != want {
		t.Fatalf("writer emitted %q, want %q", got, want)
	}
}

// TestSSEWriterRoundTripsEvents pins that what the writer emits the reader parses back
// unchanged, across the field combinations and the multi-line data case. This fixes
// "well-formed" without over-constraining the field order the writer chooses.
func TestSSEWriterRoundTripsEvents(t *testing.T) {
	cases := map[string]sseEvent{
		"data only":            {Data: "hello"},
		"event and data":       {Event: "update", Data: "hello"},
		"all fields":           {Event: "update", Data: "hello", ID: "42", Retry: 3000},
		"multi-line data":      {Data: "line one\nline two\nline three"},
		"multi-line with rest": {Event: "chunk", Data: "a\nb", ID: "7", Retry: 1500},
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			w := newSSEWriter(&buf)
			if err := w.Write(want); err != nil {
				t.Fatalf("Write: %v", err)
			}

			r := newSSEReader(bytes.NewReader(buf.Bytes()))
			got, err := r.Next()
			if err != nil {
				t.Fatalf("Next after round trip: %v", err)
			}
			if got != want {
				t.Errorf("round trip = %+v, want %+v (wire %q)", got, want, buf.Bytes())
			}
		})
	}
}

// TestSSEWriterEnforcesMaxEventSize pins transport.md Section 5's writer bound: an
// event over the maximum write size returns a typed transport error.
func TestSSEWriterEnforcesMaxEventSize(t *testing.T) {
	var buf bytes.Buffer
	w := newSSEWriter(&buf, withMaxEventBytes(16))

	if err := w.Write(sseEvent{Data: strings.Repeat("x", 100)}); !errors.Is(err, core.ErrTransport) {
		t.Fatalf("Write() = %v, want an error matching core.ErrTransport", err)
	}
}

// TestSSEWriterSerializesConcurrentWrites pins that concurrent emissions cannot
// interleave: every event is emitted whole, so the reader recovers each one intact and
// no data is lost or duplicated.
func TestSSEWriterSerializesConcurrentWrites(t *testing.T) {
	var buf bytes.Buffer
	w := newSSEWriter(&buf)

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := w.Write(sseEvent{Data: fmt.Sprintf("event-%02d", i)}); err != nil {
				t.Errorf("Write(%d): %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	r := newSSEReader(bytes.NewReader(buf.Bytes()))
	seen := make(map[string]bool, n)
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v (concurrent writes interleaved?)", err)
		}
		if seen[ev.Data] {
			t.Fatalf("duplicate event %q — writes interleaved", ev.Data)
		}
		seen[ev.Data] = true
	}
	for i := 0; i < n; i++ {
		if want := fmt.Sprintf("event-%02d", i); !seen[want] {
			t.Errorf("event %q missing — writes interleaved or lost", want)
		}
	}
}

// TestSSEWriterFlushesAfterEachEvent pins transport.md Section 5's rule that the writer
// flushes after each event, so a consumer receives an event as soon as it is produced.
func TestSSEWriterFlushesAfterEachEvent(t *testing.T) {
	rec := &flushRecorder{}
	w := newSSEWriter(rec)

	for i, data := range []string{"one", "two"} {
		if err := w.Write(sseEvent{Data: data}); err != nil {
			t.Fatalf("Write #%d: %v", i, err)
		}
		if got := rec.flushCount(); got < i+1 {
			t.Fatalf("after Write #%d, flushes = %d, want at least %d (flush after each event)", i, got, i+1)
		}
	}
	if got := string(rec.bytes()); got != "data: one\n\ndata: two\n\n" {
		t.Errorf("recorded wire = %q, want both events flushed", got)
	}
}
