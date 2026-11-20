package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qi-lore/qi-mcp-sdk/core"
)

// These tests pin the stdio framing contract (FR-020, R-005, V20): one JSON-RPC
// message per newline-terminated line, bounded by WithMaxLineBytes (default 4 MiB);
// a malformed or overlong line produces a JSON-RPC parse error (-32700) that the
// transport emits on its writer and discards, without tearing the read loop down;
// Send serialises concurrent writes so frames cannot interleave; and EOF surfaces as
// a typed error matchable with errors.Is(err, core.ErrTransport) rather than a panic.
//
// The contract is transport.md Section 3 and core-api.md "Package transport": the
// transport is a byte-level, symmetric channel. It validates only framing — a line
// that is not valid JSON, or that exceeds the bound — and never JSON-RPC semantics:
// a valid-JSON line that is not a JSON-RPC request is delivered as-is and left for
// the server's codec to reject (V1, decision 0002).
//
// This file is written first and is expected to fail to compile until stdio.go
// (T033) lands. It names three symbols from that file — Stdio, WithMaxLineBytes, and
// the StdioOption type, its declared surface in transport.md Section 2 — plus the
// core.Transport surface and the core.ErrTransport sentinel, which already exist.

var (
	// Stdio and WithMaxLineBytes are pinned to the exact signatures in
	// transport.md Section 2 / core-api.md "Package transport".
	_ func(io.Reader, io.Writer, ...StdioOption) core.Transport = Stdio
	_ func(int) StdioOption                                     = WithMaxLineBytes
)

var _ io.Writer = (*syncBuffer)(nil)

// syncBuffer is a goroutine-safe io.Writer capturing a transport's output. The stdio
// transport may write a parse error from a goroutine it owns, so the test-side buffer
// must not race with Send or with the read loop.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// Bytes returns a copy of the buffer's contents so callers never alias the internal
// slice.
func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

// frames splits a transport's output into the newline-terminated frames it wrote. The
// trailing newline is a frame terminator, not part of any frame.
func frames(t *testing.T, b []byte) [][]byte {
	t.Helper()
	if len(b) == 0 {
		return nil
	}
	if b[len(b)-1] != '\n' {
		t.Fatalf("transport output is not newline-terminated: %q", b)
	}
	return bytes.Split(b[:len(b)-1], []byte{'\n'})
}

// rpcError is the subset of a JSON-RPC error response these tests assert.
type rpcError struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Error   struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseErrorFrame asserts the transport wrote exactly one frame for the single bad
// line it was fed, that the frame is a well-formed JSON-RPC error response with a
// null id and code -32700, and that the bad line itself was discarded (not
// delivered, not echoed).
func parseErrorFrame(t *testing.T, b []byte) {
	t.Helper()
	got := frames(t, b)
	if len(got) != 1 {
		t.Fatalf("transport wrote %d output frames, want exactly 1 parse error: %q", len(got), b)
	}
	var e rpcError
	if err := json.Unmarshal(got[0], &e); err != nil {
		t.Fatalf("parse error frame %q is not JSON: %v", got[0], err)
	}
	if e.JSONRPC != "2.0" {
		t.Errorf("parse error jsonrpc = %q, want \"2.0\"", e.JSONRPC)
	}
	if e.ID != nil {
		t.Errorf("parse error id = %v, want null (a parse error has no id)", e.ID)
	}
	if e.Error.Code != -32700 {
		t.Errorf("parse error code = %d, want -32700", e.Error.Code)
	}
}

// jsonFrameOfLen builds a valid JSON value whose encoded length is exactly n bytes.
func jsonFrameOfLen(n int) []byte {
	const prefix = `{"jsonrpc":"2.0","id":1,"method":"`
	const suffix = `"}`
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		panic(fmt.Sprintf("jsonFrameOfLen(%d): n is too small", n))
	}
	return []byte(prefix + strings.Repeat("a", pad) + suffix)
}

func TestStdioSendWritesOneNewlineTerminatedFramePerCall(t *testing.T) {
	var out syncBuffer
	tr := Stdio(strings.NewReader(""), &out)
	t.Cleanup(func() { _ = tr.Close() })

	want := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`),
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call"}`),
	}
	for _, frame := range want {
		if err := tr.Send(context.Background(), frame); err != nil {
			t.Fatalf("Send(%s): %v", frame, err)
		}
	}

	got := frames(t, out.Bytes())
	if len(got) != len(want) {
		t.Fatalf("wrote %d frames, want %d: %q", len(got), len(want), out.Bytes())
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestStdioReceiveReadsOneFramePerLine(t *testing.T) {
	want := []string{
		`{"jsonrpc":"2.0","id":1,"method":"a"}`,
		`{"jsonrpc":"2.0","id":2,"method":"b"}`,
	}
	var out syncBuffer
	tr := Stdio(strings.NewReader(strings.Join(want, "\n")+"\n"), &out)
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for i, w := range want {
		frame, err := tr.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive #%d: %v", i, err)
		}
		if string(frame) != w {
			t.Errorf("Receive #%d = %q, want %q", i, frame, w)
		}
	}
}

func TestStdioMalformedLineYieldsParseErrorAndLoopContinues(t *testing.T) {
	const valid = `{"jsonrpc":"2.0","id":7,"method":"ping"}`
	var out syncBuffer
	tr := Stdio(strings.NewReader("this is not json\n"+valid+"\n"), &out)
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(frame) != valid {
		t.Fatalf("Receive = %q, want the valid frame %q after the malformed line", frame, valid)
	}
	parseErrorFrame(t, out.Bytes())
}

func TestStdioOverlongLineIsRejectedAndLoopContinues(t *testing.T) {
	const max = 16
	const valid = `{"ok":true}`
	overlong := jsonFrameOfLen(64)
	if len(overlong) <= max {
		t.Fatalf("test bug: overlong line is %d bytes, want > %d", len(overlong), max)
	}
	var out syncBuffer
	tr := Stdio(strings.NewReader(string(overlong)+"\n"+valid+"\n"), &out, WithMaxLineBytes(max))
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(frame) != valid {
		t.Fatalf("Receive = %q, want the valid frame %q after the overlong line", frame, valid)
	}
	parseErrorFrame(t, out.Bytes())
}

// TestStdioValidJSONThatIsNotARequestIsDeliveredAsIs pins the transport's boundary:
// only invalid JSON (or an overlong line) is a framing parse error. A line that is
// valid JSON but not a JSON-RPC request is delivered verbatim; rejecting it is the
// codec's job (V1, decision 0002), not the transport's.
func TestStdioValidJSONThatIsNotARequestIsDeliveredAsIs(t *testing.T) {
	const line = `{"hello":"world"}`
	var out syncBuffer
	tr := Stdio(strings.NewReader(line+"\n"), &out)
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame, err := tr.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(frame) != line {
		t.Errorf("Receive = %q, want the raw line %q", frame, line)
	}
	if got := out.Bytes(); len(got) != 0 {
		t.Errorf("transport wrote %q for a valid-JSON line, want nothing", got)
	}
}

func TestStdioConcurrentSendDoesNotInterleaveFrames(t *testing.T) {
	var out syncBuffer
	tr := Stdio(strings.NewReader(""), &out)
	t.Cleanup(func() { _ = tr.Close() })

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			frame := []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/list"}`, i))
			if err := tr.Send(context.Background(), frame); err != nil {
				t.Errorf("Send #%d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	got := frames(t, out.Bytes())
	if len(got) != n {
		t.Fatalf("wrote %d frames, want %d", len(got), n)
	}
	seen := make(map[int]bool, n)
	for _, frame := range got {
		var probe struct {
			ID int `json:"id"`
		}
		if err := json.Unmarshal(frame, &probe); err != nil {
			t.Fatalf("frame %q is not a complete JSON object (interleaved write?): %v", frame, err)
		}
		seen[probe.ID] = true
	}
	if len(seen) != n {
		t.Errorf("saw %d distinct ids across %d frames, want %d (a Send overwrote another)", len(seen), len(got), n)
	}
}

func TestStdioEOFReturnsTransportError(t *testing.T) {
	var out syncBuffer
	tr := Stdio(strings.NewReader(""), &out)
	t.Cleanup(func() { _ = tr.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	frame, err := tr.Receive(ctx)
	if err == nil {
		t.Fatalf("Receive returned frame %q at EOF, want a typed transport error", frame)
	}
	if !errors.Is(err, core.ErrTransport) {
		t.Fatalf("Receive error = %v, want errors.Is(err, core.ErrTransport)", err)
	}
}

func TestStdioClosedTransportReturnsTransportError(t *testing.T) {
	var out syncBuffer
	tr := Stdio(strings.NewReader(""), &out)
	if err := tr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := tr.Receive(context.Background()); !errors.Is(err, core.ErrTransport) {
		t.Errorf("Receive after Close = %v, want errors.Is(err, core.ErrTransport)", err)
	}
	if err := tr.Send(context.Background(), []byte(`{"jsonrpc":"2.0"}`)); !errors.Is(err, core.ErrTransport) {
		t.Errorf("Send after Close = %v, want errors.Is(err, core.ErrTransport)", err)
	}
}

// TestStdioDefaultMaxLineBytesIsFourMiB pins the R-019 resolved default (4 MiB): a
// line comfortably under the default is delivered, a line comfortably over it is
// discarded as a framing parse error. The margins avoid the exact boundary, which the
// contract does not fix.
func TestStdioDefaultMaxLineBytesIsFourMiB(t *testing.T) {
	const fourMiB = 4 << 20
	const valid = `{"ok":true}`

	t.Run("under the default is delivered", func(t *testing.T) {
		under := jsonFrameOfLen(fourMiB - 4096)
		var out syncBuffer
		tr := Stdio(strings.NewReader(string(under)+"\n"), &out)
		t.Cleanup(func() { _ = tr.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		frame, err := tr.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if !bytes.Equal(frame, under) {
			t.Errorf("Receive returned %d bytes, want the %d-byte line under the default bound", len(frame), len(under))
		}
	})

	t.Run("over the default is rejected", func(t *testing.T) {
		over := jsonFrameOfLen(fourMiB + 4096)
		var out syncBuffer
		tr := Stdio(strings.NewReader(string(over)+"\n"+valid+"\n"), &out)
		t.Cleanup(func() { _ = tr.Close() })

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		frame, err := tr.Receive(ctx)
		if err != nil {
			t.Fatalf("Receive: %v", err)
		}
		if string(frame) != valid {
			t.Fatalf("Receive = %q, want the valid frame %q after the overlong line", frame, valid)
		}
		parseErrorFrame(t, out.Bytes())
	})
}
