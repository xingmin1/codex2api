package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestApplyFastTierPolicy(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		policy     string
		wantTier   string
		wantExists bool
	}{
		{name: "preserve absent", body: `{"model":"gpt-5.4"}`, policy: "preserve"},
		{name: "preserve fast", body: `{"service_tier":"fast"}`, policy: "preserve", wantTier: "priority", wantExists: true},
		{name: "preserve priority", body: `{"service_tier":"priority"}`, policy: "preserve", wantTier: "priority", wantExists: true},
		{name: "preserve camel case", body: `{"serviceTier":"fast"}`, policy: "preserve", wantTier: "priority", wantExists: true},
		{name: "preserve unsupported", body: `{"service_tier":"unsupported"}`, policy: "preserve"},
		{name: "force absent", body: `{"model":"gpt-5.4"}`, policy: "force_fast", wantTier: "priority", wantExists: true},
		{name: "force replaces client tier", body: `{"serviceTier":"flex"}`, policy: "force_fast", wantTier: "priority", wantExists: true},
		{name: "filter snake case", body: `{"service_tier":"priority"}`, policy: "filter_fast"},
		{name: "filter camel case", body: `{"serviceTier":"fast"}`, policy: "filter_fast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, tier := applyFastTierPolicy([]byte(tt.body), tt.policy)
			field := gjson.GetBytes(got, "service_tier")
			if field.Exists() != tt.wantExists {
				t.Fatalf("service_tier exists = %t, want %t; body=%s", field.Exists(), tt.wantExists, got)
			}
			if field.String() != tt.wantTier || tier != tt.wantTier {
				t.Fatalf("service tier = (%q, %q), want %q; body=%s", field.String(), tier, tt.wantTier, got)
			}
			if gjson.GetBytes(got, "serviceTier").Exists() {
				t.Fatalf("camelCase serviceTier should not reach upstream; body=%s", got)
			}
		})
	}
}

func TestApplyFastTierPolicyIsIdempotent(t *testing.T) {
	for _, policy := range []string{
		database.FastTierPolicyPreserve,
		database.FastTierPolicyForce,
		database.FastTierPolicyFilter,
	} {
		first, firstTier := applyFastTierPolicy([]byte(`{"serviceTier":"fast"}`), policy)
		second, secondTier := applyFastTierPolicy(first, policy)
		if string(first) != string(second) || firstTier != secondTier {
			t.Fatalf("policy %q is not idempotent: first=(%s,%q), second=(%s,%q)", policy, first, firstTier, second, secondTier)
		}
	}
}

func TestApplyAccountFastTierPolicyHonorsOverrideAndInheritance(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2,
		FastTierPolicy: database.FastTierPolicyForce,
	})
	t.Cleanup(store.Stop)

	account := &auth.Account{DBID: 1, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(account)

	if body, tier := applyAccountFastTierPolicy([]byte(`{"model":"gpt-5.4"}`), account); tier != "priority" || !gjson.GetBytes(body, "service_tier").Exists() {
		t.Fatalf("inherited force policy = (%s, %q), want priority", body, tier)
	}

	filter := database.FastTierPolicyFilter
	if !store.ApplyAccountFastTierPolicy(account.DBID, &filter) {
		t.Fatal("ApplyAccountFastTierPolicy returned false")
	}
	if body, tier := applyAccountFastTierPolicy([]byte(`{"service_tier":"fast"}`), account); tier != "" || gjson.GetBytes(body, "service_tier").Exists() {
		t.Fatalf("account filter policy = (%s, %q), want no tier", body, tier)
	}

	store.SetFastTierPolicy(database.FastTierPolicyPreserve)
	if body, tier := applyAccountFastTierPolicy([]byte(`{"service_tier":"fast"}`), account); tier != "" || gjson.GetBytes(body, "service_tier").Exists() {
		t.Fatalf("explicit filter should survive global update: (%s, %q)", body, tier)
	}

	if !store.ApplyAccountFastTierPolicy(account.DBID, nil) {
		t.Fatal("clearing account policy returned false")
	}
	if body, tier := applyAccountFastTierPolicy([]byte(`{"model":"gpt-5.4"}`), account); tier != "" || gjson.GetBytes(body, "service_tier").Exists() {
		t.Fatalf("cleared override should inherit preserve: (%s, %q)", body, tier)
	}
}
