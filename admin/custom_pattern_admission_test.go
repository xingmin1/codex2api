package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestUpdateSettingsQuarantinesBroadCustomRuleAndAppliesUnrelatedSettings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.PromptFilterCustomPatterns = `[{
		"name":"existing_safe_rule",
		"pattern":"terminal-safe-marker",
		"weight":60,
		"category":"custom"
	}]`
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	submitted := []promptfilter.PatternConfig{{Name: "all", Pattern: `(?i)\ball\b`, Weight: 100, Strict: true}}
	submittedJSON, _ := json.Marshal(submitted)
	body, _ := json.Marshal(map[string]any{
		"site_name":                     "quarantine-save-succeeded",
		"prompt_filter_custom_patterns": string(submittedJSON),
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	runtimeRules := store.GetPromptFilterConfig().CustomPatterns
	if len(runtimeRules) != 1 || runtimeRules[0].Enabled == nil || *runtimeRules[0].Enabled {
		t.Fatalf("unsafe rule was not disabled in runtime config: %#v", runtimeRules)
	}
	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if persisted.SiteName != "quarantine-save-succeeded" {
		t.Fatalf("unrelated setting was not applied: %q", persisted.SiteName)
	}
	persistedRules, err := promptfilter.ParseCustomPatterns(persisted.PromptFilterCustomPatterns)
	if err != nil {
		t.Fatalf("parse persisted rules: %v", err)
	}
	if len(persistedRules) != 1 || persistedRules[0].Enabled == nil || *persistedRules[0].Enabled {
		t.Fatalf("unsafe rule was not persisted as disabled: %#v", persistedRules)
	}
	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(response.PromptFilterPatternQuarantines) != 1 || response.PromptFilterPatternQuarantines[0].Name != "all" {
		t.Fatalf("quarantine details missing from response: %#v", response.PromptFilterPatternQuarantines)
	}
}

func TestUpdateSettingsAllowsExplicitlyDisabledBroadLegacyRule(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	disabled := false
	submitted := []promptfilter.PatternConfig{{Name: "all", Pattern: `(?i)\ball\b`, Weight: 100, Strict: true, Enabled: &disabled}}
	submittedJSON, _ := json.Marshal(submitted)
	body, _ := json.Marshal(map[string]string{"prompt_filter_custom_patterns": string(submittedJSON)})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", bytes.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 body=%s", recorder.Code, recorder.Body.String())
	}
	got := store.GetPromptFilterConfig().CustomPatterns
	if len(got) != 1 || got[0].Enabled == nil || *got[0].Enabled {
		t.Fatalf("disabled legacy rule was not retained: %#v", got)
	}
}
