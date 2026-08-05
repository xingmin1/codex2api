package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

type encryptedContentCompatibilityReport struct {
	Handled   bool
	Changed   bool
	Protected bool
	Removed   int
	Param     string
	ItemType  string
	Strategy  string
}

type encryptedContentErrorInfo struct {
	Code    string
	Param   string
	Message string
}

var (
	encryptedContentInputIndexPattern        = regexp.MustCompile(`(?i)input(?:\[(\d+)\]|\.(\d+))(?:\.[a-z_][a-z0-9_]*|\[\d+\])*\.encrypted_content`)
	encryptedFunctionOutputInputIndexPattern = regexp.MustCompile(`(?i)input(?:\[(\d+)\]|\.(\d+))\.output(?:\[\d+\]|\.\d+)(?:\.[a-z_][a-z0-9_]*|\[\d+\])*\.encrypted_content`)
)

func prepareEncryptedContentCompatibilityRequest(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, false
	}

	changed := false
	if reasoning, ok := root["reasoning"]; !ok || reasoning == nil {
		root["reasoning"] = map[string]any{}
		changed = true
	}

	const encryptedReasoningInclude = "reasoning.encrypted_content"
	rawInclude, exists := root["include"]
	if !exists || rawInclude == nil {
		root["include"] = []string{encryptedReasoningInclude}
		changed = true
	} else if include, ok := rawInclude.([]any); ok {
		found := false
		for _, value := range include {
			if strings.EqualFold(strings.TrimSpace(firstNonEmptyAnyString(value)), encryptedReasoningInclude) {
				found = true
				break
			}
		}
		if !found {
			root["include"] = append(include, encryptedReasoningInclude)
			changed = true
		}
	}

	if !changed {
		return body, false
	}
	result, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return result, true
}

func responsesBodyHasEncryptedContent(body []byte) bool {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	return valueHasEncryptedContent(root)
}

func responsesPayloadIsFailed(body []byte) bool {
	if responsePayloadIsFailedJSON(body) {
		return true
	}
	return responseFailedPayload(body) != nil
}

func responsePayloadIsFailedJSON(body []byte) bool {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "type").String()), "response.failed") {
		return true
	}
	for _, path := range []string{"status", "response.status"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "failed") {
			return true
		}
	}
	return false
}

// responseFailedPayload 从 JSON 或标准 SSE framing 中提取失败事件，供 compact
// 的 HTTP 200 兼容路径复用；普通 JSON 直接返回原始切片，避免改变调用方行为。
func responseFailedPayload(body []byte) []byte {
	if responsePayloadIsFailedJSON(body) {
		return body
	}

	var dataLines [][]byte
	flushEvent := func() []byte {
		if len(dataLines) == 0 {
			return nil
		}
		payload := bytes.Join(dataLines, []byte("\n"))
		dataLines = dataLines[:0]
		if responsePayloadIsFailedJSON(payload) {
			return payload
		}
		return nil
	}

	for _, rawLine := range bytes.Split(body, []byte("\n")) {
		line := bytes.TrimSpace(bytes.TrimSuffix(rawLine, []byte("\r")))
		if len(line) == 0 {
			if payload := flushEvent(); len(payload) > 0 {
				return payload
			}
			continue
		}
		if bytes.HasPrefix(line, []byte(":")) || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			dataLines = append(dataLines, bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
		}
	}
	return flushEvent()
}

func valueHasEncryptedContent(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if valueHasEncryptedContent(item) {
				return true
			}
		}
	case map[string]any:
		if encrypted, ok := typed["encrypted_content"].(string); ok && strings.TrimSpace(encrypted) != "" {
			return true
		}
		for _, child := range typed {
			if valueHasEncryptedContent(child) {
				return true
			}
		}
	}
	return false
}

func repairResponsesEncryptedContentForError(body []byte, statusCode int, errorBody []byte) ([]byte, encryptedContentCompatibilityReport) {
	info, ok := recoverableEncryptedContentError(statusCode, errorBody)
	if !ok {
		return body, encryptedContentCompatibilityReport{}
	}
	if repaired, removed := removeEncryptedFunctionOutputContent(body, info); removed > 0 {
		return repaired, encryptedContentCompatibilityReport{
			Handled:  true,
			Changed:  true,
			Removed:  removed,
			Param:    info.Param,
			ItemType: "function_call_output",
			Strategy: "function-output",
		}
	}
	report := encryptedContentCompatibilityReport{
		Handled: true,
		Param:   info.Param,
	}

	if index, ok := encryptedContentInputIndex(info); ok {
		repaired, itemType, removed := removeTargetedEncryptedReplayItem(body, index)
		report.ItemType = itemType
		if removed {
			report.Changed = true
			report.Removed = 1
			report.Strategy = "targeted-reasoning"
			return repaired, report
		}
		// compaction 和 agent_message 的密文就是该项的语义载荷。删除整个项虽然能让请求
		// 通过校验，却会分别造成压缩记忆或子代理任务丢失，因此明确阻止破坏性兜底。
		report.Protected = isProtectedEncryptedReplayItemType(itemType)
		report.Strategy = "protected-target"
		return body, report
	}

	repaired, removed := stripDiscardableEncryptedReplayItemsForCompatibility(body)
	if removed == 0 {
		report.Protected = responsesBodyHasProtectedEncryptedReplay(body)
		report.Strategy = "no-safe-repair"
		return body, report
	}
	report.Changed = true
	report.Removed = removed
	report.Strategy = "reasoning-replay"
	return repaired, report
}

func removeEncryptedFunctionOutputContent(body []byte, info encryptedContentErrorInfo) ([]byte, int) {
	if !isInvalidEncryptedFunctionOutputError(info) {
		return body, 0
	}

	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, 0
	}
	input, ok := root["input"].([]any)
	if !ok {
		return body, 0
	}

	targetIndex, hasTargetIndex := encryptedContentOutputInputIndex(info)
	removed := 0
	for index, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		itemType := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		if isFunctionOutputItemType(itemType) {
			if hasTargetIndex && index != targetIndex {
				continue
			}
			removed += removeEncryptedContentFields(item)
			continue
		}
		if !hasTargetIndex && itemType == "agent_message" {
			removed += removeEncryptedAgentMessageContent(item)
		}
	}
	if removed == 0 {
		return body, 0
	}

	repaired, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return repaired, removed
}

func removeEncryptedAgentMessageContent(item map[string]any) int {
	content, ok := item["content"].([]any)
	if !ok {
		return 0
	}

	plainContent := 0
	for _, rawContent := range content {
		contentItem, isObject := rawContent.(map[string]any)
		if !isObject || strings.TrimSpace(firstNonEmptyAnyString(contentItem["type"])) != "encrypted_content" {
			plainContent++
		}
	}
	if plainContent == 0 {
		return 0
	}

	cleaned := make([]any, 0, plainContent)
	removed := 0
	for _, rawContent := range content {
		contentItem, isObject := rawContent.(map[string]any)
		if isObject && strings.TrimSpace(firstNonEmptyAnyString(contentItem["type"])) == "encrypted_content" {
			removed++
			continue
		}
		cleaned = append(cleaned, rawContent)
	}
	if removed > 0 {
		item["content"] = cleaned
	}
	return removed
}

func removeEncryptedContentFields(value any) int {
	removed := 0
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			removed += removeEncryptedContentFields(child)
		}
	case map[string]any:
		if _, exists := typed["encrypted_content"]; exists {
			delete(typed, "encrypted_content")
			removed++
		}
		for key, child := range typed {
			items, ok := child.([]any)
			if !ok {
				removed += removeEncryptedContentFields(child)
				continue
			}
			cleaned := make([]any, 0, len(items))
			for _, item := range items {
				content, isObject := item.(map[string]any)
				if isObject && strings.TrimSpace(firstNonEmptyAnyString(content["type"])) == "encrypted_content" {
					removed++
					continue
				}
				removed += removeEncryptedContentFields(item)
				cleaned = append(cleaned, item)
			}
			if len(cleaned) != len(items) {
				typed[key] = cleaned
			}
		}
	}
	return removed
}

func isInvalidEncryptedFunctionOutputError(info encryptedContentErrorInfo) bool {
	code := strings.ToLower(strings.TrimSpace(info.Code))
	if code != "invalid_encrypted_content" {
		return false
	}
	if _, ok := encryptedContentOutputInputIndex(info); ok {
		return true
	}
	haystack := strings.ToLower(info.Param + "\n" + info.Message)
	mentionsEncrypted := strings.Contains(haystack, "encrypted_content") || strings.Contains(haystack, "encrypted content") || strings.Contains(haystack, "encrypted")
	mentionsFunctionOutput := strings.Contains(haystack, "function_call_output") ||
		strings.Contains(haystack, "custom_tool_call_output") ||
		strings.Contains(haystack, "function output") ||
		strings.Contains(haystack, "tool call output")
	return mentionsEncrypted && mentionsFunctionOutput
}

func encryptedContentOutputInputIndex(info encryptedContentErrorInfo) (int, bool) {
	for _, candidate := range []string{info.Param, info.Message} {
		matches := encryptedFunctionOutputInputIndexPattern.FindStringSubmatch(candidate)
		if len(matches) != 3 {
			continue
		}
		rawIndex := matches[1]
		if rawIndex == "" {
			rawIndex = matches[2]
		}
		index, err := strconv.Atoi(rawIndex)
		if err == nil {
			return index, true
		}
	}
	return 0, false
}

func isFunctionOutputItemType(itemType string) bool {
	_, _, isOutput := responseContextPairType(itemType)
	return isOutput
}

func removeTargetedEncryptedReplayItem(body []byte, index int) ([]byte, string, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, "", false
	}
	input, ok := root["input"].([]any)
	if !ok || index < 0 || index >= len(input) {
		return body, "", false
	}
	item, ok := input[index].(map[string]any)
	if !ok {
		return body, "", false
	}
	itemType := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
	if !isDiscardableEncryptedReplayItemType(itemType) {
		return body, itemType, false
	}

	root["input"] = append(input[:index:index], input[index+1:]...)
	repaired, err := json.Marshal(root)
	if err != nil {
		return body, itemType, false
	}
	return repaired, itemType, true
}

func stripDiscardableEncryptedReplayItemsForCompatibility(body []byte) ([]byte, int) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, 0
	}
	input, ok := root["input"].([]any)
	if !ok {
		return body, 0
	}

	cleaned := make([]any, 0, len(input))
	removed := 0
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if ok && isDiscardableEncryptedReplayItemType(strings.TrimSpace(firstNonEmptyAnyString(item["type"]))) {
			removed++
			continue
		}
		cleaned = append(cleaned, rawItem)
	}
	if removed == 0 {
		return body, 0
	}
	root["input"] = cleaned
	repaired, err := json.Marshal(root)
	if err != nil {
		return body, 0
	}
	return repaired, removed
}

func isDiscardableEncryptedReplayItemType(itemType string) bool {
	return itemType == "reasoning" || itemType == "encrypted_content"
}

func isProtectedEncryptedReplayItemType(itemType string) bool {
	switch itemType {
	case "compaction", "context_compaction", "agent_message":
		return true
	default:
		return false
	}
}

func responsesBodyHasProtectedEncryptedReplay(body []byte) bool {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return false
	}
	input, ok := root["input"].([]any)
	if !ok {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if ok && isProtectedEncryptedReplayItemType(strings.TrimSpace(firstNonEmptyAnyString(item["type"]))) {
			return true
		}
	}
	return false
}

func recoverableEncryptedContentError(statusCode int, body []byte) (encryptedContentErrorInfo, bool) {
	if statusCode != http.StatusBadRequest || len(body) == 0 {
		return encryptedContentErrorInfo{}, false
	}
	info := extractEncryptedContentErrorInfo(body)
	code := strings.ToLower(strings.TrimSpace(info.Code))
	param := strings.ToLower(strings.TrimSpace(info.Param))
	message := strings.ToLower(strings.TrimSpace(info.Message))
	bodyText := strings.ToLower(string(body))

	if code == "invalid_encrypted_content" || strings.Contains(bodyText, "invalid_encrypted_content") {
		return info, true
	}
	if strings.HasSuffix(param, ".encrypted_content") {
		switch code {
		case "missing_required_parameter", "string_above_max_length", "string_too_long", "invalid_value", "invalid_type":
			return info, true
		}
		if strings.Contains(message, "encrypted_content") {
			return info, true
		}
	}
	mentionsEncryptedContent := strings.Contains(message, "encrypted content") || strings.Contains(message, "encrypted_content")
	if mentionsEncryptedContent && (strings.Contains(message, "missing") ||
		strings.Contains(message, "required") ||
		strings.Contains(message, "decrypt") ||
		strings.Contains(message, "verif") ||
		strings.Contains(message, "too long") ||
		strings.Contains(message, "maximum length")) {
		return info, true
	}
	return encryptedContentErrorInfo{}, false
}

func extractEncryptedContentErrorInfo(body []byte) encryptedContentErrorInfo {
	for _, prefix := range []string{"error", "response.error", "response.status_details.error", "detail"} {
		result := gjson.GetBytes(body, prefix)
		if !result.Exists() || !result.IsObject() {
			continue
		}
		return encryptedContentErrorInfo{
			Code:    result.Get("code").String(),
			Param:   result.Get("param").String(),
			Message: result.Get("message").String(),
		}
	}
	return encryptedContentErrorInfo{
		Code:    gjson.GetBytes(body, "code").String(),
		Param:   gjson.GetBytes(body, "param").String(),
		Message: firstNonEmptyString(gjson.GetBytes(body, "message").String(), gjson.GetBytes(body, "detail").String()),
	}
}

func encryptedContentInputIndex(info encryptedContentErrorInfo) (int, bool) {
	for _, candidate := range []string{info.Param, info.Message} {
		matches := encryptedContentInputIndexPattern.FindStringSubmatch(candidate)
		if len(matches) != 3 {
			continue
		}
		rawIndex := matches[1]
		if rawIndex == "" {
			rawIndex = matches[2]
		}
		index, err := strconv.Atoi(rawIndex)
		if err == nil {
			return index, true
		}
	}
	return 0, false
}
