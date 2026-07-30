package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	upstreamErrorKindInvalidCompactionOutput = "invalid_compaction_output"
	upstreamErrorKindCompactionBufferLimit   = "compaction_buffer_limit"
	nativeCompactionUnsupportedTTL           = 30 * time.Minute
	nativeCompactionSemanticRetryLimit       = 1
)

type remoteCompactionV2OutputTracker struct {
	outputItemCount         int
	compactionCount         int
	compactionContents      []string
	missingEncryptedContent bool
	terminalObserved        bool
}

type remoteCompactionV2OutputError struct {
	outputItemCount         int
	compactionCount         int
	missingEncryptedContent bool
	inconsistent            bool
	capabilityUnsupported   bool
	scope                   string
	terminalStatus          string
}

func (e *remoteCompactionV2OutputError) Error() string {
	if e == nil {
		return "remote compaction v2 output is invalid"
	}
	if e.terminalStatus != "" {
		return fmt.Sprintf("remote compaction v2 response ended with terminal status %q", e.terminalStatus)
	}
	if e.inconsistent {
		return "remote compaction v2 output_item.done and response.completed output disagree"
	}
	if e.missingEncryptedContent {
		return "remote compaction v2 compaction output item is missing non-empty encrypted_content"
	}
	if e.scope != "" && e.outputItemCount == 0 && e.compactionCount == 0 {
		return fmt.Sprintf("remote compaction v2 response has invalid %s", e.scope)
	}
	scope := "output items"
	if e.scope != "" {
		scope = e.scope
	}
	return fmt.Sprintf(
		"remote compaction v2 expected exactly one compaction output item, got %d from %d %s",
		e.compactionCount,
		e.outputItemCount,
		scope,
	)
}

func (t *remoteCompactionV2OutputTracker) observeSSEEvent(event gjson.Result) error {
	if t == nil {
		return nil
	}
	eventType := strings.TrimSpace(event.Get("type").String())
	if t.terminalObserved {
		return &remoteCompactionV2OutputError{terminalStatus: "event_after_terminal"}
	}
	switch eventType {
	case "response.output_item.done":
		t.observeOutputItem(event.Get("item"))
	case "response.completed":
		t.terminalObserved = true
		return t.observeCompletedResponse(event.Get("response"))
	case "response.failed":
		// response.failed 是普通失败终态，不应伪装成 compaction 输出缺失。
		t.terminalObserved = true
	case "response.incomplete", "response.cancelled", "response.canceled":
		t.terminalObserved = true
		return &remoteCompactionV2OutputError{
			terminalStatus:        eventType,
			capabilityUnsupported: false,
		}
	}
	return nil
}

func (t *remoteCompactionV2OutputTracker) observeOutputItem(item gjson.Result) {
	if t == nil {
		return
	}
	t.outputItemCount++
	if item.Get("type").String() != "compaction" {
		return
	}
	t.compactionCount++
	t.compactionContents = append(t.compactionContents, strings.TrimSpace(item.Get("encrypted_content").String()))
	if !compactionItemHasEncryptedContent(item) {
		t.missingEncryptedContent = true
	}
}

func (t *remoteCompactionV2OutputTracker) observeCompletedResponse(response gjson.Result) error {
	if t == nil {
		return nil
	}
	status := strings.ToLower(strings.TrimSpace(response.Get("status").String()))
	if status != "" && status != "completed" {
		return &remoteCompactionV2OutputError{terminalStatus: status}
	}
	if err := t.validate(); err != nil {
		return err
	}

	output := response.Get("output")
	if !output.Exists() {
		return nil
	}
	if !output.IsArray() {
		return &remoteCompactionV2OutputError{
			scope:                 "response.completed.response.output",
			capabilityUnsupported: true,
		}
	}
	completedTracker := trackerFromCompactionOutput(output)
	if err := completedTracker.validateWithScope("response.completed.response.output"); err != nil {
		return err
	}
	if !sameCompactionContents(t.compactionContents, completedTracker.compactionContents) {
		return &remoteCompactionV2OutputError{
			inconsistent:          true,
			capabilityUnsupported: true,
		}
	}
	return nil
}

func (t *remoteCompactionV2OutputTracker) validate() error {
	return t.validateWithScope("")
}

func (t *remoteCompactionV2OutputTracker) validateWithScope(scope string) error {
	if t == nil {
		return nil
	}
	// Codex compaction v2 的协议要求 completed 之前恰好出现一个 output_item.done，
	// 且该项就是唯一的 compaction 项，不能用 completed.output 事后补齐。
	if t.outputItemCount != 1 || t.compactionCount != 1 {
		return &remoteCompactionV2OutputError{
			outputItemCount:         t.outputItemCount,
			compactionCount:         t.compactionCount,
			missingEncryptedContent: t.missingEncryptedContent,
			capabilityUnsupported:   true,
			scope:                   scope,
		}
	}
	if t.missingEncryptedContent {
		return &remoteCompactionV2OutputError{
			outputItemCount:         t.outputItemCount,
			compactionCount:         t.compactionCount,
			missingEncryptedContent: true,
			capabilityUnsupported:   true,
			scope:                   scope,
		}
	}
	return nil
}

func sameCompactionContents(done, completed []string) bool {
	if len(done) != len(completed) {
		return false
	}
	for index := range done {
		if strings.TrimSpace(done[index]) != strings.TrimSpace(completed[index]) {
			return false
		}
	}
	return true
}

func compactionItemHasEncryptedContent(item gjson.Result) bool {
	return item.Get("type").String() == "compaction" && strings.TrimSpace(item.Get("encrypted_content").String()) != ""
}

func trackerFromCompactionOutput(output gjson.Result) *remoteCompactionV2OutputTracker {
	tracker := &remoteCompactionV2OutputTracker{}
	if !output.Exists() || !output.IsArray() {
		return tracker
	}
	for _, item := range output.Array() {
		tracker.observeOutputItem(item)
	}
	return tracker
}

func validateRemoteCompactionV2ResponseJSON(body []byte) error {
	if responsesPayloadIsFailed(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	status := strings.ToLower(strings.TrimSpace(root.Get("status").String()))
	if status == "" {
		status = strings.ToLower(strings.TrimSpace(root.Get("response.status").String()))
	}
	if status != "" && status != "completed" {
		return &remoteCompactionV2OutputError{terminalStatus: status}
	}
	output := root.Get("output")
	if !output.Exists() {
		output = root.Get("response.output")
	}
	return trackerFromCompactionOutput(output).validate()
}

// validateRemoteCompactionV2CompletedResponseJSON 同时校验 SSE done 事件和最终 JSON。
func validateRemoteCompactionV2CompletedResponseJSON(body []byte, doneTracker *remoteCompactionV2OutputTracker) error {
	if failed := responseFailedPayload(body); len(failed) > 0 {
		return nil
	}
	if doneTracker == nil {
		return validateRemoteCompactionV2ResponseJSON(body)
	}
	return doneTracker.observeCompletedResponse(gjson.ParseBytes(body))
}

func isRemoteCompactionSemanticValidationError(err error) bool {
	var outputErr *remoteCompactionV2OutputError
	return errors.As(err, &outputErr) && outputErr.capabilityUnsupported
}

func invalidRemoteCompactionV2Outcome(err error) streamOutcome {
	if errors.Is(err, errRemoteCompactionV2BufferLimit) {
		return streamOutcome{
			logStatusCode:  http.StatusBadGateway,
			failureKind:    upstreamErrorKindCompactionBufferLimit,
			failureMessage: err.Error(),
			penalize:       false,
		}
	}
	message := "remote compaction v2 response is invalid"
	if err != nil {
		message = err.Error()
	}
	return streamOutcome{
		logStatusCode:  http.StatusBadGateway,
		failureKind:    upstreamErrorKindInvalidCompactionOutput,
		failureMessage: message,
		penalize:       true,
	}
}

func nativeCompactionCapabilityKey(accountID int64, model string) string {
	return fmt.Sprintf("%d:%s", accountID, strings.TrimSpace(model))
}

func (h *Handler) rememberNativeCompactionUnsupported(accountID int64, model string) {
	if h == nil || accountID <= 0 {
		return
	}
	h.nativeCompactionUnsupported.Store(
		nativeCompactionCapabilityKey(accountID, model),
		time.Now().Add(nativeCompactionUnsupportedTTL).UnixNano(),
	)
}

func (h *Handler) supportsNativeCompaction(accountID int64, model string) bool {
	if h == nil || accountID <= 0 {
		return true
	}
	key := nativeCompactionCapabilityKey(accountID, model)
	raw, found := h.nativeCompactionUnsupported.Load(key)
	if !found {
		return true
	}
	expiresAt, ok := raw.(int64)
	if !ok || time.Now().UnixNano() >= expiresAt {
		// 只有仍然持有同一个过期值时才删除，避免并发请求把新 TTL 误删。
		h.nativeCompactionUnsupported.CompareAndDelete(key, raw)
		return true
	}
	return false
}

func (h *Handler) nativeCompactionAccountFilter(model string, base auth.AccountFilter) auth.AccountFilter {
	return func(account *auth.Account) bool {
		if base != nil && !base(account) {
			return false
		}
		return account != nil && h.supportsNativeCompaction(account.ID(), model)
	}
}
