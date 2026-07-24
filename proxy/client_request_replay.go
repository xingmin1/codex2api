package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const clientRequestReplayManagedKey = "client_request_replay_managed"

var codexClientReplayKeepalive = []byte("event: codex2api.keepalive\ndata: {\"type\":\"codex2api.keepalive\"}\n\n")
var genericClientReplayKeepalive = []byte(": codex2api.keepalive\n\n")

type clientRequestReplayDelivery struct {
	mu              sync.Mutex
	downstream      gin.ResponseWriter
	codexClient     bool
	keepaliveSent   bool
	businessStarted bool
	writeErr        error
}

type clientRequestReplayWriter struct {
	delivery *clientRequestReplayDelivery
	header   http.Header
	status   int
	size     int
	stream   bool
	buffer   bytes.Buffer
}

func newClientRequestReplayDelivery(downstream gin.ResponseWriter, codexClient bool) *clientRequestReplayDelivery {
	return &clientRequestReplayDelivery{downstream: downstream, codexClient: codexClient}
}

func newClientRequestReplayWriter(delivery *clientRequestReplayDelivery, stream bool) *clientRequestReplayWriter {
	return &clientRequestReplayWriter{
		delivery: delivery,
		header:   make(http.Header),
		status:   http.StatusOK,
		size:     -1,
		stream:   stream,
	}
}

func copyReplayHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
}

func (d *clientRequestReplayDelivery) writeKeepalive() error {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.businessStarted || d.writeErr != nil {
		return d.writeErr
	}

	header := d.downstream.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	if !d.downstream.Written() {
		d.downstream.WriteHeader(http.StatusOK)
	}
	payload := genericClientReplayKeepalive
	if d.codexClient {
		payload = codexClientReplayKeepalive
	}
	_, d.writeErr = d.downstream.Write(payload)
	if d.writeErr == nil {
		d.downstream.Flush()
		d.keepaliveSent = true
	}
	return d.writeErr
}

func (d *clientRequestReplayDelivery) writeBusiness(header http.Header, status int, data []byte) (int, error) {
	if d == nil {
		return 0, io.ErrClosedPipe
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.writeErr != nil {
		return 0, d.writeErr
	}
	if !d.businessStarted {
		copyReplayHeaders(d.downstream.Header(), header)
		if !d.downstream.Written() {
			d.downstream.WriteHeader(status)
		}
		d.businessStarted = true
	}
	n, err := d.downstream.Write(data)
	if err != nil {
		d.writeErr = err
	}
	return n, err
}

func (d *clientRequestReplayDelivery) flushBusiness() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.businessStarted && d.writeErr == nil {
		d.downstream.Flush()
	}
}

func (d *clientRequestReplayDelivery) hasBusinessStarted() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.businessStarted
}

func (d *clientRequestReplayDelivery) sentKeepalive() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.keepaliveSent
}

func (d *clientRequestReplayDelivery) hasWriteError() bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeErr != nil
}

func (d *clientRequestReplayDelivery) commitBuffered(writer *clientRequestReplayWriter) {
	if d == nil || writer == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	copyReplayHeaders(d.downstream.Header(), writer.header)
	if !d.downstream.Written() {
		d.downstream.WriteHeader(writer.status)
	}
	if writer.buffer.Len() > 0 {
		_, d.writeErr = d.downstream.Write(writer.buffer.Bytes())
	}
}

func (d *clientRequestReplayDelivery) commitSSEFailure(status int, body []byte) {
	if d == nil {
		return
	}
	message := usageLogErrorMessage(status, body)
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("HTTP %d", status)
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"status": "failed",
			"error": gin.H{
				"code":        fmt.Sprintf("upstream_%d", status),
				"message":     message,
				"status_code": status,
				"type":        "server_error",
			},
		},
	})

	d.mu.Lock()
	defer d.mu.Unlock()
	header := d.downstream.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-cache, no-transform")
	header.Set("X-Accel-Buffering", "no")
	if !d.downstream.Written() {
		d.downstream.WriteHeader(http.StatusOK)
	}
	if d.writeErr == nil {
		_, d.writeErr = fmt.Fprintf(d.downstream, "event: response.failed\ndata: %s\n\n", payload)
	}
	if d.writeErr == nil {
		d.downstream.Flush()
	}
}

func (w *clientRequestReplayWriter) Header() http.Header {
	return w.header
}

func (w *clientRequestReplayWriter) WriteHeader(status int) {
	if status <= 0 || w.Written() {
		return
	}
	w.status = status
}

func (w *clientRequestReplayWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
	}
}

func (w *clientRequestReplayWriter) Write(data []byte) (int, error) {
	if !w.Written() {
		w.WriteHeaderNow()
	}
	if w.stream && w.status >= 200 && w.status < 300 {
		n, err := w.delivery.writeBusiness(w.header, w.status, data)
		w.size += n
		return n, err
	}
	n, err := w.buffer.Write(data)
	w.size += n
	return n, err
}

func (w *clientRequestReplayWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *clientRequestReplayWriter) Status() int {
	return w.status
}

func (w *clientRequestReplayWriter) Size() int {
	return w.size
}

func (w *clientRequestReplayWriter) Written() bool {
	return w.size >= 0
}

func (w *clientRequestReplayWriter) Flush() {
	w.delivery.flushBusiness()
}

func (w *clientRequestReplayWriter) CloseNotify() <-chan bool {
	return w.delivery.downstream.CloseNotify()
}

func (w *clientRequestReplayWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.delivery.downstream.Hijack()
}

func (w *clientRequestReplayWriter) Pusher() http.Pusher {
	return w.delivery.downstream.Pusher()
}

func (w *clientRequestReplayWriter) Unwrap() http.ResponseWriter {
	return w.delivery.downstream
}

func (w *clientRequestReplayWriter) successfulNonStreamResponse() bool {
	if w == nil || !w.Written() || w.status < 200 || w.status >= 300 {
		return false
	}
	if w.status == http.StatusNoContent {
		return true
	}
	if w.buffer.Len() == 0 {
		return false
	}
	body := w.buffer.Bytes()
	if gjson.GetBytes(body, "type").String() == "response.failed" {
		return false
	}
	if gjson.GetBytes(body, "error").Exists() || gjson.GetBytes(body, "response.error").Exists() {
		return false
	}
	for _, path := range []string{"status", "response.status"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "failed") {
			return false
		}
	}
	return true
}

func (w *clientRequestReplayWriter) ensureFailureResponse() {
	if w == nil || w.Written() {
		return
	}
	w.status = http.StatusBadGateway
	w.size = 0
	w.header.Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.buffer.WriteString(`{"error":{"code":"empty_internal_response","message":"请求未产生可交付响应","type":"server_error"}}`)
}

func cloneGinKeys(keys map[any]any) map[any]any {
	if len(keys) == 0 {
		return nil
	}
	cloned := make(map[any]any, len(keys))
	for key, value := range keys {
		cloned[key] = value
	}
	return cloned
}

func clientRequestReplayManaged(c *gin.Context) bool {
	if c == nil {
		return false
	}
	managed, _ := c.Get(clientRequestReplayManagedKey)
	enabled, _ := managed.(bool)
	return enabled
}

func (h *Handler) handleWithClientRequestReplay(c *gin.Context, endpoint string, attempt func(*gin.Context)) {
	if h == nil || h.store == nil || !h.store.ClientRequestReplayEnabled() {
		attempt(c)
		return
	}

	originalBody, err := readRawRequestBody(c)
	if err != nil {
		attempt(c)
		return
	}
	originalBody = append([]byte(nil), originalBody...)
	stream := gjson.GetBytes(originalBody, "stream").Bool()
	baseKeys := cloneGinKeys(c.Keys)
	downstream := c.Writer
	codexClient := IsCodexStrictOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("Originator"))
	delivery := newClientRequestReplayDelivery(downstream, codexClient)
	keepaliveSeconds := h.store.ClientRequestReplayKeepaliveSeconds()

	var stopKeepalive chan struct{}
	var keepaliveDone chan struct{}
	if stream && keepaliveSeconds > 0 {
		stopKeepalive = make(chan struct{})
		keepaliveDone = make(chan struct{})
		go func() {
			defer close(keepaliveDone)
			ticker := time.NewTicker(time.Duration(keepaliveSeconds) * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-c.Request.Context().Done():
					return
				case <-stopKeepalive:
					return
				case <-ticker.C:
					if delivery.hasBusinessStarted() {
						return
					}
					if err := delivery.writeKeepalive(); err != nil {
						return
					}
				}
			}
		}()
	}
	stopHeartbeat := func() {
		if stopKeepalive == nil {
			return
		}
		close(stopKeepalive)
		<-keepaliveDone
		stopKeepalive = nil
	}
	defer func() {
		stopHeartbeat()
		c.Writer = downstream
	}()

	maxRetries := h.store.ClientRequestReplayMaxRetries()
	var lastWriter *clientRequestReplayWriter
	for replay := 0; ; replay++ {
		c.Keys = cloneGinKeys(baseKeys)
		setRawRequestBody(c, originalBody)
		c.Set(clientRequestReplayManagedKey, true)
		c.Request.Body = io.NopCloser(bytes.NewReader(originalBody))
		c.Request.ContentLength = int64(len(originalBody))
		c.Errors = c.Errors[:0]

		writer := newClientRequestReplayWriter(delivery, stream)
		lastWriter = writer
		c.Writer = writer
		attempt(c)
		c.Writer = downstream

		if delivery.hasBusinessStarted() {
			stopHeartbeat()
			return
		}
		if !stream && writer.successfulNonStreamResponse() {
			stopHeartbeat()
			delivery.commitBuffered(writer)
			return
		}
		if c.Request.Context().Err() != nil {
			return
		}
		if delivery.hasWriteError() {
			return
		}
		if maxRetries > 0 && replay >= maxRetries {
			break
		}

		log.Printf("整请求代重发：%s 第 %d 轮未成功且尚未输出业务数据，按原请求入口重新调度", endpoint, replay+1)
		if !h.waitBeforeRetry(c.Request.Context()) {
			return
		}
	}

	stopHeartbeat()
	if lastWriter == nil {
		return
	}
	lastWriter.ensureFailureResponse()
	if stream && delivery.sentKeepalive() {
		status := lastWriter.status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		delivery.commitSSEFailure(status, lastWriter.buffer.Bytes())
		return
	}
	delivery.commitBuffered(lastWriter)
}
