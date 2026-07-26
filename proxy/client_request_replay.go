package proxy

import (
	"bufio"
	"bytes"
	"context"
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

const (
	clientRequestReplayManagedKey     = "client_request_replay_managed"
	clientRequestReplayBlockReasonKey = "client_request_replay_block_reason"
)

type clientRequestReplayStopReason string

const (
	clientRequestReplayStopSuccess     clientRequestReplayStopReason = "success"
	clientRequestReplayStopMaxRetries  clientRequestReplayStopReason = "max_retries"
	clientRequestReplayStopMaxDuration clientRequestReplayStopReason = "max_duration"
	clientRequestReplayStopClientGone  clientRequestReplayStopReason = "client_closed"
	clientRequestReplayStopWriteFailed clientRequestReplayStopReason = "write_failed"
	clientRequestReplayStopCyberPolicy clientRequestReplayStopReason = "cyber_policy"
)

type clientRequestReplayControllerContextKey struct{}

type clientRequestReplayController struct {
	mu              sync.RWMutex
	ctx             context.Context
	cancel          context.CancelFunc
	clientCtx       context.Context
	startedAt       time.Time
	deadlineTimer   *time.Timer
	businessStarted bool
	stopReason      clientRequestReplayStopReason
}

func newClientRequestReplayController(clientCtx context.Context, maxDuration time.Duration) *clientRequestReplayController {
	if clientCtx == nil {
		clientCtx = context.Background()
	}
	baseCtx, cancel := context.WithCancel(context.WithoutCancel(clientCtx))
	controller := &clientRequestReplayController{
		cancel:    cancel,
		clientCtx: clientCtx,
		startedAt: time.Now(),
	}
	controller.ctx = context.WithValue(baseCtx, clientRequestReplayControllerContextKey{}, controller)
	if maxDuration > 0 {
		controller.deadlineTimer = time.AfterFunc(maxDuration, controller.expire)
	}
	go func() {
		select {
		case <-clientCtx.Done():
			controller.stop(clientRequestReplayStopClientGone)
		case <-controller.ctx.Done():
		}
	}()
	return controller
}

func (c *clientRequestReplayController) expire() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.businessStarted || c.stopReason != "" {
		c.mu.Unlock()
		return
	}
	c.stopReason = clientRequestReplayStopMaxDuration
	c.mu.Unlock()
	c.cancel()
}

func (c *clientRequestReplayController) stop(reason clientRequestReplayStopReason) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopReason == "" {
		c.stopReason = reason
	}
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
	}
	c.mu.Unlock()
	c.cancel()
}

func (c *clientRequestReplayController) succeed() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.stopReason == "" {
		c.stopReason = clientRequestReplayStopSuccess
	}
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
	}
	c.mu.Unlock()
}

func (c *clientRequestReplayController) markBusinessStarted() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.businessStarted = true
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
	}
	c.mu.Unlock()
}

func (c *clientRequestReplayController) hasBusinessStarted() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.businessStarted
}

func (c *clientRequestReplayController) reason() clientRequestReplayStopReason {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	reason := c.stopReason
	c.mu.RUnlock()
	if reason == "" && c.clientCtx.Err() != nil {
		return clientRequestReplayStopClientGone
	}
	return reason
}

func (c *clientRequestReplayController) context() context.Context {
	if c == nil {
		return context.Background()
	}
	return c.ctx
}

func (c *clientRequestReplayController) elapsed() time.Duration {
	if c == nil {
		return 0
	}
	return time.Since(c.startedAt)
}

func (c *clientRequestReplayController) wait(delay time.Duration) bool {
	if c == nil || c.context().Err() != nil {
		return false
	}
	if delay <= 0 {
		return c.context().Err() == nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-c.context().Done():
		return false
	}
}

func (c *clientRequestReplayController) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.deadlineTimer != nil {
		c.deadlineTimer.Stop()
	}
	c.mu.Unlock()
	c.cancel()
}

func clientRequestReplayControllerFromContext(ctx context.Context) *clientRequestReplayController {
	if ctx == nil {
		return nil
	}
	controller, _ := ctx.Value(clientRequestReplayControllerContextKey{}).(*clientRequestReplayController)
	return controller
}

func clientRequestReplayDelay(baseInterval, maxInterval time.Duration, replayIndex int) time.Duration {
	if baseInterval <= 0 || maxInterval <= 0 {
		return 0
	}
	if baseInterval >= maxInterval {
		return maxInterval
	}
	delay := baseInterval
	for i := 0; i < replayIndex && delay < maxInterval; i++ {
		if delay > maxInterval/2 {
			return maxInterval
		}
		delay *= 2
	}
	if delay > maxInterval {
		return maxInterval
	}
	return delay
}

var codexClientReplayKeepalive = []byte("event: codex2api.keepalive\ndata: {\"type\":\"codex2api.keepalive\"}\n\n")
var genericClientReplayKeepalive = []byte(": codex2api.keepalive\n\n")

type clientRequestReplayDelivery struct {
	mu              sync.Mutex
	downstream      gin.ResponseWriter
	controller      *clientRequestReplayController
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

func newClientRequestReplayDelivery(downstream gin.ResponseWriter, controller *clientRequestReplayController, codexClient bool) *clientRequestReplayDelivery {
	return &clientRequestReplayDelivery{downstream: downstream, controller: controller, codexClient: codexClient}
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
	} else if d.controller != nil {
		d.controller.stop(clientRequestReplayStopWriteFailed)
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
		d.businessStarted = true
		if d.controller != nil {
			d.controller.markBusinessStarted()
		}
		copyReplayHeaders(d.downstream.Header(), header)
		if !d.downstream.Written() {
			d.downstream.WriteHeader(status)
		}
	}
	n, err := d.downstream.Write(data)
	if err != nil {
		d.writeErr = err
		if d.controller != nil {
			d.controller.stop(clientRequestReplayStopWriteFailed)
		}
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
		if d.writeErr != nil && d.controller != nil {
			d.controller.stop(clientRequestReplayStopWriteFailed)
		}
	}
}

func (d *clientRequestReplayDelivery) commitSSEFailure(status int, body []byte, reason clientRequestReplayStopReason) {
	if d == nil {
		return
	}
	message := usageLogErrorMessage(status, body)
	if strings.TrimSpace(message) == "" {
		message = fmt.Sprintf("HTTP %d", status)
	}
	errorCode := "client_request_replay_exhausted"
	errorType := "server_error"
	if reason == clientRequestReplayStopCyberPolicy {
		errorCode = string(clientRequestReplayStopCyberPolicy)
		errorType = "upstream_error"
	}
	payload, _ := json.Marshal(gin.H{
		"type": "response.failed",
		"response": gin.H{
			"status": "failed",
			"error": gin.H{
				"code":        errorCode,
				"message":     message,
				"status_code": status,
				"stop_reason": string(reason),
				"type":        errorType,
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
		if d.writeErr != nil && d.controller != nil {
			d.controller.stop(clientRequestReplayStopWriteFailed)
		}
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

func (w *clientRequestReplayWriter) replaceWithDurationExceededResponse() {
	if w == nil {
		return
	}
	lastMessage := usageLogErrorMessage(w.status, w.buffer.Bytes())
	if strings.TrimSpace(lastMessage) == "" {
		lastMessage = "上游未在整请求总预算内返回可交付响应"
	}
	w.header = make(http.Header)
	w.header.Set("Content-Type", "application/json; charset=utf-8")
	w.status = http.StatusGatewayTimeout
	w.size = 0
	w.buffer.Reset()
	payload, _ := json.Marshal(gin.H{
		"error": gin.H{
			"code":        "client_request_replay_exhausted",
			"message":     lastMessage,
			"stop_reason": string(clientRequestReplayStopMaxDuration),
			"type":        "server_error",
		},
	})
	_, _ = w.buffer.Write(payload)
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

func blockClientRequestReplay(c *gin.Context, reason clientRequestReplayStopReason) {
	if c == nil || reason == "" {
		return
	}
	c.Set(clientRequestReplayBlockReasonKey, reason)
}

func clientRequestReplayBlockReason(c *gin.Context) clientRequestReplayStopReason {
	if c == nil {
		return ""
	}
	reason, _ := c.Get(clientRequestReplayBlockReasonKey)
	blocked, _ := reason.(clientRequestReplayStopReason)
	return blocked
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
	originalRequest := c.Request
	maxRetries := h.store.ClientRequestReplayMaxRetries()
	maxDuration := time.Duration(h.store.ClientRequestReplayMaxDurationSeconds()) * time.Second
	baseInterval := time.Duration(h.store.ClientRequestReplayBaseIntervalMS()) * time.Millisecond
	maxInterval := time.Duration(h.store.ClientRequestReplayMaxIntervalSeconds()) * time.Second
	controller := newClientRequestReplayController(originalRequest.Context(), maxDuration)
	defer controller.close()
	codexClient := IsCodexStrictOfficialClientByHeaders(c.GetHeader("User-Agent"), c.GetHeader("Originator"))
	delivery := newClientRequestReplayDelivery(downstream, controller, codexClient)
	keepaliveSeconds := h.store.ClientRequestReplayKeepaliveSeconds()
	attempts := 0
	lastStatus := 0
	defer func() {
		reason := controller.reason()
		if attempts > 1 || reason != clientRequestReplayStopSuccess {
			log.Printf("整请求代重发结束：%s 总轮数=%d 总耗时=%s 最后状态=%d 停止原因=%s", endpoint, attempts, controller.elapsed().Round(time.Millisecond), lastStatus, reason)
		}
	}()

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
				case <-controller.context().Done():
					return
				case <-stopKeepalive:
					return
				case <-ticker.C:
					if originalRequest.Context().Err() != nil || controller.context().Err() != nil {
						return
					}
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
		c.Request = originalRequest
	}()

	var lastWriter *clientRequestReplayWriter
	for replay := 0; replay <= maxRetries; replay++ {
		if controller.context().Err() != nil {
			break
		}
		c.Keys = cloneGinKeys(baseKeys)
		setRawRequestBody(c, originalBody)
		c.Set(clientRequestReplayManagedKey, true)
		c.Request = originalRequest.Clone(controller.context())
		c.Request.Body = io.NopCloser(bytes.NewReader(originalBody))
		c.Request.ContentLength = int64(len(originalBody))
		c.Errors = c.Errors[:0]

		writer := newClientRequestReplayWriter(delivery, stream)
		lastWriter = writer
		c.Writer = writer
		attempts++
		attempt(c)
		c.Writer = downstream
		lastStatus = writer.status

		if blockedReason := clientRequestReplayBlockReason(c); blockedReason != "" {
			controller.stop(blockedReason)
			stopHeartbeat()
			if delivery.hasBusinessStarted() {
				return
			}
			break
		}
		if delivery.hasBusinessStarted() {
			stopHeartbeat()
			controller.succeed()
			return
		}
		reason := controller.reason()
		if reason == clientRequestReplayStopClientGone || reason == clientRequestReplayStopWriteFailed {
			return
		}
		if !stream && writer.successfulNonStreamResponse() {
			stopHeartbeat()
			delivery.commitBuffered(writer)
			if !delivery.hasWriteError() {
				controller.succeed()
			}
			return
		}
		if reason == clientRequestReplayStopMaxDuration {
			break
		}
		if replay >= maxRetries {
			controller.stop(clientRequestReplayStopMaxRetries)
			break
		}

		delay := clientRequestReplayDelay(baseInterval, maxInterval, replay)
		log.Printf("整请求代重发：%s 第 %d 轮未成功，等待 %s 后按原请求入口重新调度，累计耗时=%s", endpoint, replay+1, delay, controller.elapsed().Round(time.Millisecond))
		if !controller.wait(delay) {
			break
		}
	}

	stopHeartbeat()
	if originalRequest.Context().Err() != nil {
		return
	}
	reason := controller.reason()
	if reason == clientRequestReplayStopClientGone || reason == clientRequestReplayStopWriteFailed {
		return
	}
	if lastWriter == nil {
		return
	}
	lastWriter.ensureFailureResponse()
	if reason == clientRequestReplayStopMaxDuration {
		lastWriter.replaceWithDurationExceededResponse()
		lastStatus = lastWriter.status
	}
	if stream && delivery.sentKeepalive() {
		status := lastWriter.status
		if status < 400 || status > 599 {
			status = http.StatusBadGateway
		}
		delivery.commitSSEFailure(status, lastWriter.buffer.Bytes(), reason)
		return
	}
	delivery.commitBuffered(lastWriter)
}
