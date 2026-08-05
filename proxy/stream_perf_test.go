package proxy

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/codex2api/security/promptfilter"
	"github.com/tidwall/gjson"
)

func TestStreamFlushWriterWriteSSEData(t *testing.T) {
	var buf bytes.Buffer
	writer := &streamFlushWriter{writer: &buf}

	if err := writer.WriteSSEData([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)); err != nil {
		t.Fatalf("WriteSSEData returned error: %v", err)
	}

	want := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("unexpected SSE payload:\n got %q\nwant %q", got, want)
	}
}

func TestStreamFlushWriterWriteBytesNoStringConversionRequired(t *testing.T) {
	var buf bytes.Buffer
	writer := &streamFlushWriter{writer: &buf}

	payload := []byte("data: [DONE]\n\n")
	if err := writer.WriteBytes(payload); err != nil {
		t.Fatalf("WriteBytes returned error: %v", err)
	}
	if got := buf.String(); got != string(payload) {
		t.Fatalf("unexpected payload: got %q want %q", got, string(payload))
	}
}

func TestStreamTranslatorTranslateParsedMatchesTranslate(t *testing.T) {
	events := [][]byte{
		[]byte(`{"type":"response.output_item.added","item":{"type":"function_call","id":"item_1","call_id":"call_1","name":"lookup"}}`),
		[]byte(`{"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"city\":\"Paris\"}"}`),
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`),
	}

	fromRaw := NewStreamTranslator("chatcmpl-test", "gpt-test", 123)
	fromParsed := NewStreamTranslator("chatcmpl-test", "gpt-test", 123)

	for _, event := range events {
		rawChunk, rawDone := fromRaw.Translate(event)
		parsedChunk, parsedDone := fromParsed.TranslateParsed(gjson.ParseBytes(event))

		if rawDone != parsedDone {
			t.Fatalf("done mismatch for %s: raw=%v parsed=%v", event, rawDone, parsedDone)
		}
		if string(rawChunk) != string(parsedChunk) {
			t.Fatalf("chunk mismatch for %s:\n raw=%s\nparsed=%s", event, rawChunk, parsedChunk)
		}
	}
}

func TestUpstreamErrorConsoleBodyTruncatesLargePayload(t *testing.T) {
	body := []byte(strings.Repeat("x", consoleUpstreamErrorLogMaxBytes+128))
	got := upstreamErrorConsoleBody(body)
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if len(got) > consoleUpstreamErrorLogMaxBytes+len(" ... [truncated]") {
		t.Fatalf("truncated console body too large: %d", len(got))
	}
}

func TestUpstreamErrorFileLogBodyTruncatesLargePayload(t *testing.T) {
	body := []byte(strings.Repeat("x", upstreamErrorLogBodyMaxBytes+128))
	got := upstreamErrorLogBody(body)
	if !strings.Contains(got, "[truncated]") {
		t.Fatalf("expected truncation marker, got %q", got)
	}
	if len(got) > 5000+len(" ... [truncated]") {
		t.Fatalf("truncated file log body too large: %d", len(got))
	}
}

func TestWriteDeferredSSEDataFlushesPendingWithCurrentEvent(t *testing.T) {
	var buf bytes.Buffer
	writer := &streamFlushWriter{writer: &buf}
	var pending bytes.Buffer

	wrote, err := writeDeferredSSEData(writer, &pending, []byte(`{"type":"response.created"}`), true)
	if err != nil {
		t.Fatalf("defer lifecycle event returned error: %v", err)
	}
	if wrote {
		t.Fatal("deferred lifecycle event should not write before first token")
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected early output: %q", buf.String())
	}

	wrote, err = writeDeferredSSEData(writer, &pending, []byte(`{"type":"response.output_text.delta","delta":"hi"}`), false)
	if err != nil {
		t.Fatalf("first content event returned error: %v", err)
	}
	if !wrote {
		t.Fatal("first content event should flush pending data")
	}

	want := "data: {\"type\":\"response.created\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("unexpected deferred SSE output:\n got %q\nwant %q", got, want)
	}
	if pending.Len() != 0 {
		t.Fatalf("pending buffer not reset: %d", pending.Len())
	}
}

func TestWriteDeferredSSEDataKeepsLargePreContentEventRetryable(t *testing.T) {
	var buf bytes.Buffer
	writer := &streamFlushWriter{writer: &buf}
	var pending bytes.Buffer
	payload := []byte(strings.Repeat("x", 1024*1024))

	wrote, err := writeDeferredSSEData(writer, &pending, payload, true)
	if err != nil {
		t.Fatalf("large deferred event returned error: %v", err)
	}
	if wrote || buf.Len() != 0 {
		t.Fatalf("large pre-content event leaked before retry boundary: wrote=%t bytes=%d", wrote, buf.Len())
	}

	content := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	wrote, err = writeDeferredSSEData(writer, &pending, content, false)
	if err != nil {
		t.Fatalf("first content event returned error: %v", err)
	}
	if !wrote {
		t.Fatal("first content event should flush the buffered pre-content event")
	}
	want := "data: " + string(payload) + "\n\ndata: " + string(content) + "\n\n"
	if got := buf.String(); got != want {
		t.Fatalf("unexpected large deferred output length/content: got len=%d want len=%d equal=%v", len(got), len(want), got == want)
	}
	if pending.Len() != 0 {
		t.Fatalf("pending buffer not reset: %d", pending.Len())
	}
}

func TestWriteDeferredSSEDataRejectsUnboundedPreContentBuffer(t *testing.T) {
	var buf bytes.Buffer
	writer := &streamFlushWriter{writer: &buf}
	var pending bytes.Buffer
	pending.Grow(pendingFirstTokenMaxBytes + 1)
	pending.Write(bytes.Repeat([]byte("x"), pendingFirstTokenMaxBytes))

	wrote, err := writeDeferredSSEData(writer, &pending, []byte("x"), true)
	if !errors.Is(err, errPendingFirstTokenBufferLimit) {
		t.Fatalf("error = %v, want %v", err, errPendingFirstTokenBufferLimit)
	}
	if wrote || buf.Len() != 0 || pending.Len() != 0 {
		t.Fatalf("overflow must fail closed without delivery: wrote=%t output=%d pending=%d", wrote, buf.Len(), pending.Len())
	}
}

func TestWriteDeferredSSEDataReportsActualOutputScannerDelivery(t *testing.T) {
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Advanced.Output.Enabled = true
	cfg.Advanced.Output.BufferBytes = 512
	cfg.Advanced.Output.OverlapBytes = 64
	var buf bytes.Buffer
	writer := &streamFlushWriter{writer: &buf, outputScanner: promptfilter.NewOutputScanner(cfg)}
	var pending bytes.Buffer

	wrote, err := writeDeferredSSEData(writer, &pending, []byte(`{"type":"response.output_text.delta","delta":"hi"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if wrote || buf.Len() != 0 {
		t.Fatalf("scanner-buffered event reported client delivery: wrote=%t bytes=%d", wrote, buf.Len())
	}

	wrote, err = writeDeferredSSEData(writer, &pending, []byte(`{"type":"response.completed"}`), false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote || buf.Len() == 0 {
		t.Fatalf("terminal scanner release was not reported: wrote=%t bytes=%d", wrote, buf.Len())
	}
}

func TestShouldSuppressRetryableResponseFailedBeforeFirstToken(t *testing.T) {
	retryableFailed := []byte(`{"type":"response.failed","response":{"error":{"message":"rate limited","status_code":429,"code":"rate_limit_exceeded"}}}`)
	nonRetryableFailed := []byte(`{"type":"response.failed","response":{"error":{"message":"bad request","status_code":400,"code":"invalid_request_error"}}}`)

	if !shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 0, 1, nil, nil) {
		t.Fatal("retryable response.failed before first token should be suppressed while another attempt remains")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, false, 1, 1, nil, nil) {
		t.Fatal("last attempt should not suppress response.failed")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", nonRetryableFailed, false, false, 0, 1, nil, nil) {
		t.Fatal("non-retryable response.failed should still be sent to the client")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, true, false, 0, 1, nil, nil) {
		t.Fatal("response.failed after first token should not be suppressed")
	}
	if shouldSuppressRetryableResponseFailedBeforeFirstToken("response.failed", retryableFailed, false, true, 0, 1, nil, nil) {
		t.Fatal("response.failed after writing any body should not be suppressed")
	}
}

func BenchmarkStreamFlushWriterWriteSSEData(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	for i := 0; i < b.N; i++ {
		var buf bytes.Buffer
		writer := &streamFlushWriter{writer: &buf}
		if err := writer.WriteSSEData(payload); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadSSEStream(b *testing.B) {
	var input bytes.Buffer
	for i := 0; i < 1024; i++ {
		input.WriteString("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n")
	}
	input.WriteString("data: [DONE]\n\n")
	payload := input.Bytes()

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := ReadSSEStream(bytes.NewReader(payload), func([]byte) bool { return true }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStreamTranslatorTranslateParsed(b *testing.B) {
	event := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	parsed := gjson.ParseBytes(event)
	st := NewStreamTranslator("chatcmpl-bench", "gpt-test", 123)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if chunk, done := st.TranslateParsed(parsed); done || chunk == nil {
			b.Fatalf("unexpected translation result: done=%v chunk=%s", done, chunk)
		}
	}
}
