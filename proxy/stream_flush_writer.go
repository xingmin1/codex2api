package proxy

import (
	"bytes"
	"io"
	"net/http"
	"time"
)

const (
	pendingFirstTokenFlushBytes   = 1024 * 1024
	completionBufferedSSEMaxBytes = 64 * 1024 * 1024
)

var (
	sseDataPrefix = []byte("data: ")
	sseDataSuffix = []byte("\n\n")
)

type streamFlushWriter struct {
	writer    io.Writer
	flusher   http.Flusher
	policy    string
	interval  time.Duration
	lastFlush time.Time
	buffer    bytes.Buffer
}

type completionBufferedSSEWriter struct {
	buffer      bytes.Buffer
	passthrough bool
}

func newStreamFlushWriter(writer io.Writer, flusher http.Flusher) *streamFlushWriter {
	settings := CurrentRuntimeSettings()
	return &streamFlushWriter{
		writer:   writer,
		flusher:  flusher,
		policy:   settings.StreamFlushPolicy,
		interval: currentStreamFlushInterval(),
	}
}

func appendSSEData(buf *bytes.Buffer, data []byte) {
	if buf == nil {
		return
	}
	buf.Write(sseDataPrefix)
	buf.Write(data)
	buf.Write(sseDataSuffix)
}

func writeDeferredSSEData(streamWriter *streamFlushWriter, pending *bytes.Buffer, data []byte, shouldDefer bool) (bool, error) {
	if streamWriter == nil {
		return false, nil
	}
	if shouldDefer {
		appendSSEData(pending, data)
		if pending != nil && pending.Len() <= pendingFirstTokenFlushBytes {
			return false, nil
		}
	}
	if pending != nil && pending.Len() > 0 {
		if !shouldDefer {
			appendSSEData(pending, data)
		}
		if err := streamWriter.WriteBytes(pending.Bytes()); err != nil {
			return false, err
		}
		pending.Reset()
		return true, nil
	}
	if shouldDefer {
		return false, nil
	}
	if err := streamWriter.WriteSSEData(data); err != nil {
		return false, err
	}
	return true, nil
}

func newCompletionBufferedSSEWriter(enabled bool) *completionBufferedSSEWriter {
	if !enabled {
		return nil
	}
	return &completionBufferedSSEWriter{}
}

func (w *completionBufferedSSEWriter) writeEvent(streamWriter *streamFlushWriter, pending *bytes.Buffer, data []byte, eventType string, shouldDefer bool) (bool, error) {
	if w == nil {
		return writeDeferredSSEData(streamWriter, pending, data, shouldDefer)
	}
	if w.passthrough {
		return writeDeferredSSEData(streamWriter, nil, data, false)
	}

	appendSSEData(&w.buffer, data)
	if eventType != "response.completed" && w.buffer.Len() <= completionBufferedSSEMaxBytes {
		return false, nil
	}

	// v2 压缩只有在 compaction 输出和 response.completed 同时到齐时才有效。
	// 完整终态前不提交下游响应，断流后才能透明换号；极端超大响应超过上限时
	// 退化为普通透传，避免并发压缩占用无界内存。
	w.passthrough = true
	if err := streamWriter.WriteBytes(w.buffer.Bytes()); err != nil {
		return false, err
	}
	w.buffer.Reset()
	return true, nil
}

func (w *completionBufferedSSEWriter) discard() {
	if w == nil {
		return
	}
	w.buffer.Reset()
}

func (w *streamFlushWriter) WriteString(data string) error {
	if w == nil || w.writer == nil {
		return nil
	}
	if w.policy != StreamFlushPolicyCoalesce {
		if _, err := io.WriteString(w.writer, data); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if _, err := w.buffer.WriteString(data); err != nil {
		return err
	}
	if w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval {
		return w.Flush()
	}
	return nil
}

func (w *streamFlushWriter) WriteBytes(data []byte) error {
	if w == nil || w.writer == nil || len(data) == 0 {
		return nil
	}
	if w.policy != StreamFlushPolicyCoalesce {
		if _, err := w.writer.Write(data); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	if _, err := w.buffer.Write(data); err != nil {
		return err
	}
	if w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval {
		return w.Flush()
	}
	return nil
}

func (w *streamFlushWriter) WriteSSEData(data []byte) error {
	if w == nil || w.writer == nil {
		return nil
	}
	if w.policy != StreamFlushPolicyCoalesce {
		if _, err := w.writer.Write(sseDataPrefix); err != nil {
			return err
		}
		if _, err := w.writer.Write(data); err != nil {
			return err
		}
		if _, err := w.writer.Write(sseDataSuffix); err != nil {
			return err
		}
		w.flushTransport()
		return nil
	}
	appendSSEData(&w.buffer, data)
	if w.lastFlush.IsZero() || time.Since(w.lastFlush) >= w.interval {
		return w.Flush()
	}
	return nil
}

func (w *streamFlushWriter) Flush() error {
	if w == nil {
		return nil
	}
	if w.buffer.Len() > 0 {
		if _, err := w.writer.Write(w.buffer.Bytes()); err != nil {
			return err
		}
		w.buffer.Reset()
	}
	w.flushTransport()
	return nil
}

func (w *streamFlushWriter) flushTransport() {
	if w == nil || w.flusher == nil {
		return
	}
	w.flusher.Flush()
	w.lastFlush = time.Now()
}
