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
