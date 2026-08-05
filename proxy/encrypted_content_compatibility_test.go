package proxy

import (
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrepareEncryptedContentCompatibilityRequest(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","include":["file_search_call.results"],"input":[]}`)

	got, changed := prepareEncryptedContentCompatibilityRequest(body)

	if !changed {
		t.Fatal("compatibility request should add reasoning controls")
	}
	if !gjson.GetBytes(got, "reasoning").IsObject() {
		t.Fatalf("reasoning controls were not added: %s", got)
	}
	include := gjson.GetBytes(got, "include").Array()
	if len(include) != 2 || include[0].String() != "file_search_call.results" || include[1].String() != "reasoning.encrypted_content" {
		t.Fatalf("include was not preserved and extended: %s", got)
	}
}

func TestPrepareEncryptedContentCompatibilityRequestIsIdempotent(t *testing.T) {
	body := []byte(`{"reasoning":{"effort":"high"},"include":["reasoning.encrypted_content"],"input":[]}`)

	got, changed := prepareEncryptedContentCompatibilityRequest(body)

	if changed || string(got) != string(body) {
		t.Fatalf("already compatible request changed: %s", got)
	}
}

func TestRepairResponsesEncryptedContentForErrorRemovesTargetedReplayItem(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":"keep-first"},
		{"type":"reasoning","encrypted_content":"remove-me"},
		{"type":"compaction","encrypted_content":"keep-last"}
	]}`)
	errorBody := []byte(`{"type":"response.failed","response":{"error":{
		"code":"missing_required_parameter",
		"param":"input[1].encrypted_content",
		"message":"Missing required parameter: 'input[1].encrypted_content'."
	}}}`)

	got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)
	if !report.Handled || !report.Changed || report.Strategy != "targeted-reasoning" || report.Removed != 1 {
		t.Fatalf("report = %+v, want targeted removal", report)
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 2 || items[0].Get("content").String() != "keep-first" ||
		items[1].Get("encrypted_content").String() != "keep-last" {
		t.Fatalf("targeted repair removed unrelated encrypted history: %s", got)
	}
}

func TestRepairResponsesEncryptedContentForErrorProtectsSemanticEncryptedItems(t *testing.T) {
	for _, tc := range []struct {
		name     string
		item     string
		itemType string
		param    string
	}{
		{name: "compaction", item: `{"type":"compaction","encrypted_content":"compact-state"}`, itemType: "compaction", param: "input[1].encrypted_content"},
		{name: "agent message", item: `{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"agent-task"}]}`, itemType: "agent_message", param: "input[1].content[0].encrypted_content"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"input":[{"type":"message","role":"user","content":"keep"},` + tc.item + `]}`)
			errorBody := []byte(`{"error":{"code":"missing_required_parameter","param":"` + tc.param + `","message":"Missing required encrypted_content"}}`)

			got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)

			if !report.Handled || report.Changed || !report.Protected || report.ItemType != tc.itemType {
				t.Fatalf("report = %+v, want protected %s", report, tc.itemType)
			}
			if string(got) != string(body) {
				t.Fatalf("protected replay item changed: %s", got)
			}
		})
	}
}

func TestRepairResponsesEncryptedContentForErrorFallsBackToReasoningOnly(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":"keep"},
		{"type":"reasoning","encrypted_content":"drop"},
		{"type":"compaction","encrypted_content":"keep-compaction"},
		{"type":"agent_message","content":[{"type":"input_text","text":"header"},{"type":"encrypted_content","encrypted_content":"keep-agent"}]}
	]}`)
	errorBody := []byte(`{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted"}}`)

	got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)
	if !report.Handled || !report.Changed || report.Strategy != "reasoning-replay" || report.Removed != 1 {
		t.Fatalf("report = %+v, want reasoning-only fallback", report)
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 3 || items[1].Get("type").String() != "compaction" || items[2].Get("type").String() != "agent_message" {
		t.Fatalf("fallback removed protected encrypted state: %s", got)
	}
}

func TestRepairResponsesEncryptedContentForErrorRemovesOnlyFunctionOutputEncryptedContent(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call_output","call_id":"call-1","output":[
			{"type":"input_text","text":"keep"},
			{"type":"encrypted_content","encrypted_content":"drop"}
		]},
		{"type":"custom_tool_call_output","call_id":"call-2","output":[
			{"type":"encrypted_content","encrypted_content":"drop-too"}
		]},
		{"type":"compaction","encrypted_content":"keep-compaction"},
		{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"keep-agent"}]}
	]}`)
	errorBody := []byte(`{"type":"response.failed","response":{"status":"failed","error":{
		"code":"invalid_encrypted_content",
		"param":"input[0].output[1].encrypted_content",
		"message":"Invalid encrypted function output: encrypted_content could not be decrypted."
	}}}`)

	got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)

	if !report.Handled || !report.Changed || report.Strategy != "function-output" || report.Removed != 1 {
		t.Fatalf("report = %+v, want targeted function output repair", report)
	}
	firstOutput := gjson.GetBytes(got, "input.0.output").Array()
	if len(firstOutput) != 1 || firstOutput[0].Get("text").String() != "keep" {
		t.Fatalf("mixed function output was not preserved correctly: %s", got)
	}
	if gjson.GetBytes(got, "input.0.call_id").String() != "call-1" || gjson.GetBytes(got, "input.1.call_id").String() != "call-2" {
		t.Fatalf("call_id was not preserved: %s", got)
	}
	if output := gjson.GetBytes(got, "input.1.output"); !output.IsArray() || len(output.Array()) != 1 || output.Array()[0].Get("encrypted_content").String() != "drop-too" {
		t.Fatalf("param-targeted repair should not modify unrelated function output: %s", got)
	}
	if gjson.GetBytes(got, "input.2.encrypted_content").String() != "keep-compaction" || gjson.GetBytes(got, "input.3.content.0.encrypted_content").String() != "keep-agent" {
		t.Fatalf("protected state was changed: %s", got)
	}
}

func TestRepairResponsesEncryptedContentForErrorDoesNotTreatGenericInvalidEncryptedContentAsFunctionOutput(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"reasoning","encrypted_content":"drop-reasoning"},
		{"type":"function_call_output","call_id":"call-1","output":[{"type":"encrypted_content","encrypted_content":"keep-function-output"}]},
		{"type":"compaction","encrypted_content":"keep-compaction"}
	]}`)
	errorBody := []byte(`{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted"}}`)

	got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)

	if !report.Handled || !report.Changed || report.Strategy != "reasoning-replay" || report.Removed != 1 {
		t.Fatalf("report = %+v, want existing reasoning fallback", report)
	}
	if gjson.GetBytes(got, "input.0.type").String() != "function_call_output" || gjson.GetBytes(got, "input.0.output.0.encrypted_content").String() != "keep-function-output" {
		t.Fatalf("generic error should not strip function output encrypted content: %s", got)
	}
}

func TestRepairResponsesEncryptedContentForErrorRequiresInvalidEncryptedContentForFunctionOutput(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":[{"type":"encrypted_content","encrypted_content":"keep"}]}]}`)
	errorBody := []byte(`{"error":{"code":"invalid_type","param":"input[0].output[0].encrypted_content","message":"Invalid encrypted function output"}}`)

	got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)

	if report.Changed || string(got) != string(body) {
		t.Fatalf("non invalid_encrypted_content function output error must not use dedicated repair: report=%+v body=%s", report, got)
	}
}

func TestOfficialSSEResponseFailedFunctionOutputRepairUsesDerivedBadRequestStatus(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call_output","call_id":"call-1","output":[{"type":"encrypted_content","encrypted_content":"drop"}]}]}`)
	failure := []byte(`{"type":"response.failed","response":{"status":"failed","error":{
		"code":"invalid_encrypted_content",
		"message":"Invalid encrypted function output: encrypted_content could not be decrypted."
	}}}`)

	got, report := repairResponsesEncryptedContentForError(body, responseFailedStatusCode(failure), failure)

	if !report.Handled || !report.Changed || report.Strategy != "function-output" {
		t.Fatalf("report = %+v, want official SSE response.failed repair", report)
	}
	if output := gjson.GetBytes(got, "input.0.output"); !output.IsArray() || len(output.Array()) != 0 {
		t.Fatalf("official SSE repair should keep function output with empty output array: %s", got)
	}
}

func TestOfficialSSEResponseFailedRemovesTopLevelFunctionOutputEncryptedContent(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"function_call_output","call_id":"call-1","output":"keep output","encrypted_content":"drop"},
		{"type":"compaction","encrypted_content":"keep-compaction"},
		{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"keep-agent"}]}
	]}`)
	failure := []byte(`{"type":"response.failed","response":{"status":"failed","error":{
		"status_code":400,
		"code":"invalid_encrypted_content",
		"message":"Encrypted function output content could not be decrypted or decoded."
	}}}`)

	got, report := repairResponsesEncryptedContentForError(body, responseFailedStatusCode(failure), failure)

	if !report.Handled || !report.Changed || report.Strategy != "function-output" || report.Removed != 1 {
		t.Fatalf("report = %+v, want top-level function output repair", report)
	}
	if gjson.GetBytes(got, "input.0.encrypted_content").Exists() {
		t.Fatalf("top-level function output encrypted_content was not removed: %s", got)
	}
	if gjson.GetBytes(got, "input.0.output").String() != "keep output" || gjson.GetBytes(got, "input.0.call_id").String() != "call-1" {
		t.Fatalf("function output payload changed during repair: %s", got)
	}
	if gjson.GetBytes(got, "input.1.encrypted_content").String() != "keep-compaction" || gjson.GetBytes(got, "input.2.content.0.encrypted_content").String() != "keep-agent" {
		t.Fatalf("protected encrypted state was changed: %s", got)
	}
}

func TestOfficialSSEResponseFailedRepairsAgentMessageFunctionOutputFallback(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"reasoning","encrypted_content":"drop-reasoning"},
		{"type":"agent_message","content":[
			{"type":"input_text","text":"keep agent output"},
			{"type":"encrypted_content","encrypted_content":"drop-agent"}
		]},
		{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"keep-protected"}]},
		{"type":"compaction","encrypted_content":"keep-compaction"}
	]}`)
	failure := []byte(`{"type":"response.failed","response":{"status":"failed","error":{
		"status_code":400,
		"code":"invalid_encrypted_content",
		"message":"Encrypted function output content could not be decrypted or decoded."
	}}}`)

	got, report := repairResponsesEncryptedContentForError(body, responseFailedStatusCode(failure), failure)

	if !report.Changed || report.Strategy != "function-output" || report.Removed != 1 {
		t.Fatalf("report = %+v, want one agent message encrypted attachment removed", report)
	}
	if gjson.GetBytes(got, "input.1.content.#").Int() != 1 || gjson.GetBytes(got, "input.1.content.0.text").String() != "keep agent output" {
		t.Fatalf("agent message plaintext was not preserved: %s", got)
	}
	if gjson.GetBytes(got, "input.2.content.0.encrypted_content").String() != "keep-protected" {
		t.Fatalf("pure encrypted agent message must remain protected: %s", got)
	}
	if gjson.GetBytes(got, "input.0.encrypted_content").String() != "drop-reasoning" || gjson.GetBytes(got, "input.3.encrypted_content").String() != "keep-compaction" {
		t.Fatalf("function-output fallback changed unrelated encrypted state: %s", got)
	}
}

func TestOfficialSSEResponseFailedRepairsAllCallOutputDialectsAndNestedContent(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"tool_call_output","call_id":"call-1","output":{"content":[{"type":"input_text","text":"keep"},{"type":"encrypted_content","encrypted_content":"drop-item"}],"metadata":{"encrypted_content":"drop-field"}}},
		{"type":"mcp_tool_call_output","call_id":"call-2","output":{"encrypted_content":"drop-mcp","text":"keep-mcp"}},
		{"type":"compaction","encrypted_content":"keep-compaction"},
		{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"keep-agent"}]}
	]}`)
	failure := []byte(`{"type":"response.failed","response":{"status":"failed","error":{
		"status_code":400,
		"code":"invalid_encrypted_content",
		"message":"Encrypted function output content could not be decrypted or decoded."
	}}}`)

	got, report := repairResponsesEncryptedContentForError(body, responseFailedStatusCode(failure), failure)

	if !report.Changed || report.Strategy != "function-output" || report.Removed != 3 {
		t.Fatalf("report = %+v, want three encrypted function output values removed", report)
	}
	if gjson.GetBytes(got, "input.0.output.content.#").Int() != 1 || gjson.GetBytes(got, "input.0.output.content.0.text").String() != "keep" {
		t.Fatalf("tool output content was not repaired safely: %s", got)
	}
	if gjson.GetBytes(got, "input.0.output.metadata.encrypted_content").Exists() || gjson.GetBytes(got, "input.1.output.encrypted_content").Exists() {
		t.Fatalf("nested encrypted fields remain in call outputs: %s", got)
	}
	if gjson.GetBytes(got, "input.1.output.text").String() != "keep-mcp" {
		t.Fatalf("ordinary MCP output changed during repair: %s", got)
	}
	if gjson.GetBytes(got, "input.2.encrypted_content").String() != "keep-compaction" || gjson.GetBytes(got, "input.3.content.0.encrypted_content").String() != "keep-agent" {
		t.Fatalf("protected encrypted state was changed: %s", got)
	}
}

func TestRepairResponsesEncryptedContentForErrorRecognizesFunctionOutputFromParamOnly(t *testing.T) {
	body := []byte(`{"input":[
		{"type":"message","role":"user","content":"keep"},
		{"type":"function_call_output","call_id":"call-1","output":[{"type":"encrypted_content","encrypted_content":"drop"}]}
	]}`)
	errorBody := []byte(`{"error":{
		"code":"invalid_encrypted_content",
		"param":"input[1].output[0].encrypted_content",
		"message":"Encrypted content could not be decrypted."
	}}`)

	got, report := repairResponsesEncryptedContentForError(body, http.StatusBadRequest, errorBody)

	if !report.Handled || !report.Changed || report.Strategy != "function-output" || report.Removed != 1 {
		t.Fatalf("report = %+v, want param-only function output repair", report)
	}
	if output := gjson.GetBytes(got, "input.1.output"); !output.IsArray() || len(output.Array()) != 0 {
		t.Fatalf("param-only repair should produce empty function output array: %s", got)
	}
}

func TestResponsesWebSocketRetryPayloadCanRepairRawAndCodexBodies(t *testing.T) {
	rawBody := []byte(`{"input":[{"type":"function_call_output","call_id":"raw-call","output":[{"type":"encrypted_content","encrypted_content":"drop-raw"}]}]}`)
	codexBody := []byte(`{"input":[{"type":"function_call_output","call_id":"codex-call","output":[{"type":"input_text","text":"keep"},{"type":"encrypted_content","encrypted_content":"drop-codex"}]}]}`)
	failure := []byte(`{"type":"response.failed","response":{"status":"failed","error":{
		"code":"invalid_encrypted_content",
		"param":"input[0].output[0].encrypted_content",
		"message":"Invalid encrypted function output: encrypted_content could not be decrypted."
	}}}`)

	repairedRaw, rawReport := repairResponsesEncryptedContentForError(rawBody, responseFailedStatusCode(failure), failure)
	repairedCodex, codexReport := repairResponsesEncryptedContentForError(codexBody, responseFailedStatusCode(failure), failure)

	if !rawReport.Changed || !codexReport.Changed || rawReport.Strategy != "function-output" || codexReport.Strategy != "function-output" {
		t.Fatalf("reports = raw:%+v codex:%+v, want WS raw/codex repair", rawReport, codexReport)
	}
	if len(gjson.GetBytes(repairedRaw, "input.0.output").Array()) != 0 {
		t.Fatalf("raw body was not sanitized to an empty function output array: %s", repairedRaw)
	}
	if output := gjson.GetBytes(repairedCodex, "input.0.output").Array(); len(output) != 1 || output[0].Get("text").String() != "keep" {
		t.Fatalf("codex body did not preserve non-encrypted output content: %s", repairedCodex)
	}
}

func TestRecoverableEncryptedContentErrorShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "top-level missing",
			body: `{"error":{"code":"missing_required_parameter","param":"input[5].encrypted_content","message":"Missing required parameter: input[5].encrypted_content"}}`,
		},
		{
			name: "response error nested missing",
			body: `{"type":"response.failed","response":{"error":{"code":"missing_required_parameter","param":"input[5].content[1].encrypted_content","message":"Missing required parameter: input[5].content[1].encrypted_content"}}}`,
		},
		{
			name: "status details oversized",
			body: `{"response":{"status_details":{"error":{"code":"string_above_max_length","param":"input[9].encrypted_content","message":"encrypted_content is too long"}}}}`,
		},
		{
			name: "provider decryption failure",
			body: `{"error":{"code":"invalid_encrypted_content","message":"Encrypted content could not be verified or decrypted"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := recoverableEncryptedContentError(http.StatusBadRequest, []byte(tc.body)); !ok {
				t.Fatalf("error shape was not recognized: %s", tc.body)
			}
		})
	}
	if _, ok := recoverableEncryptedContentError(http.StatusInternalServerError, []byte(tests[0].body)); ok {
		t.Fatal("non-400 response must not use encrypted-content compatibility repair")
	}
}
