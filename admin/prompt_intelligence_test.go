package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/security/promptfilter"
)

func TestSearchGitHubPromptIntelligence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "jailbreak prompt" {
			t.Fatalf("query = %q", r.URL.Query().Get("q"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"full_name":"owner/repo","html_url":"https://github.com/owner/repo","description":"prompt injection research","updated_at":"2026-07-15T00:00:00Z"}]}`))
	}))
	defer server.Close()
	old := githubPromptSearchBaseURL
	githubPromptSearchBaseURL = server.URL
	defer func() { githubPromptSearchBaseURL = old }()
	items, err := searchGitHubPromptIntelligence(context.Background(), "jailbreak prompt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "owner/repo" {
		t.Fatalf("items = %#v", items)
	}
}

func TestValidateIntelligenceCandidate(t *testing.T) {
	valid := promptIntelligenceCandidate{Name: "new_jailbreak_phrase", Pattern: `(?i)ignore\s+safety`, Weight: 80, Category: "prompt_injection", Strict: true}
	if err := validateIntelligenceCandidate(valid); err != nil {
		t.Fatal(err)
	}
	valid.Pattern = "("
	if err := validateIntelligenceCandidate(valid); err == nil {
		t.Fatal("invalid regexp accepted")
	}
	valid.Pattern = `(?s).*`
	if err := validateIntelligenceCandidate(valid); err == nil {
		t.Fatal("match-all regexp accepted")
	}
	valid.Name = "all"
	valid.Pattern = `(?i)\ball\b`
	if err := validateIntelligenceCandidate(valid); err == nil {
		t.Fatal("generic all regexp accepted")
	}
	if !intelligencePatternHasRiskSignal(`(?i)generate\s+and\s+execute\s+(?:a\s+)?reverse\s+shell`) {
		t.Fatal("known high-risk signal was not recognized")
	}
	if intelligencePatternHasRiskSignal(`(?i)reverse\s+shell`) {
		t.Fatal("partial risk phrase was accepted for automatic rule admission")
	}
	if intelligencePatternHasRiskSignal(`[sS][tT][rR]`) {
		t.Fatal("ordinary code fragment passed the risk corpus by substring")
	}
	if intelligencePatternHasRiskSignal(`(?i)quarterly\s+report`) {
		t.Fatal("benign-only candidate passed the risk corpus")
	}
}

func TestAutomaticIntelligenceCandidateIsAuditOnly(t *testing.T) {
	candidate := promptIntelligenceCandidate{
		Name:     "generated_high_risk_rule",
		Pattern:  `(?i)generate\s+and\s+execute\s+(?:a\s+)?reverse\s+shell`,
		Weight:   100,
		Category: "remote_access",
		Strict:   true,
	}
	automatic := intelligenceCandidatePatternConfig(candidate, true)
	if automatic.Strict || !automatic.SignalOnly || automatic.Weight != maxAutomaticIntelligenceWeight {
		t.Fatalf("automatic candidate can enforce: %#v", automatic)
	}
	manual := intelligenceCandidatePatternConfig(candidate, false)
	if !manual.Strict || manual.SignalOnly || manual.Weight != candidate.Weight {
		t.Fatalf("manual candidate semantics changed: %#v", manual)
	}
}

func TestAutomaticIntelligenceMayUpdateOnlyUnpromotedAuditRules(t *testing.T) {
	disabled := false
	for _, tc := range []struct {
		name string
		rule promptfilter.PatternConfig
		want bool
	}{
		{name: "automatic signal-only rule", rule: promptfilter.PatternConfig{SignalOnly: true}, want: true},
		{name: "disabled rule", rule: promptfilter.PatternConfig{Enabled: &disabled, SignalOnly: true}, want: false},
		{name: "administrator strict promotion", rule: promptfilter.PatternConfig{Strict: true, SignalOnly: true}, want: false},
		{name: "administrator enforcing promotion", rule: promptfilter.PatternConfig{SignalOnly: false}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := automaticIntelligenceMayUpdate(tc.rule); got != tc.want {
				t.Fatalf("automaticIntelligenceMayUpdate(%#v) = %v, want %v", tc.rule, got, tc.want)
			}
		})
	}
}

func TestMergeIntelligenceQueriesIncludesChineseBuiltins(t *testing.T) {
	queries := mergeIntelligenceQueries(defaultIntelligenceQueries, []string{"custom query", "GPT 破甲 提示词"})
	want := map[string]bool{"大模型 破限 提示词": false, "GPT 破甲 提示词": false, "AI 越狱 提示词": false, "custom query": false}
	for _, query := range queries {
		if _, ok := want[query]; ok {
			want[query] = true
		}
	}
	for query, found := range want {
		if !found {
			t.Fatalf("missing query %q in %#v", query, queries)
		}
	}
}
