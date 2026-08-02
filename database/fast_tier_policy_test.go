package database

import "testing"

func TestParseFastTierPolicy(t *testing.T) {
	for input, want := range map[string]string{
		"preserve":     FastTierPolicyPreserve,
		" FORCE_FAST ": FastTierPolicyForce,
		"Filter_Fast":  FastTierPolicyFilter,
	} {
		got, ok := ParseFastTierPolicy(input)
		if !ok || got != want {
			t.Fatalf("ParseFastTierPolicy(%q) = (%q, %t), want (%q, true)", input, got, ok, want)
		}
	}

	for _, input := range []string{"", "fast", "off", "unknown"} {
		if got, ok := ParseFastTierPolicy(input); ok || got != "" {
			t.Fatalf("ParseFastTierPolicy(%q) = (%q, %t), want invalid", input, got, ok)
		}
	}
}

func TestNormalizeFastTierPolicyFallsBackToPreserve(t *testing.T) {
	for _, input := range []string{"", "unknown", " force "} {
		if got := NormalizeFastTierPolicy(input); got != FastTierPolicyPreserve {
			t.Fatalf("NormalizeFastTierPolicy(%q) = %q, want %q", input, got, FastTierPolicyPreserve)
		}
	}
}
