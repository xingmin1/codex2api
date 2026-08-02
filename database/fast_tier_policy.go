package database

import "strings"

const (
	FastTierPolicyPreserve = "preserve"
	FastTierPolicyForce    = "force_fast"
	FastTierPolicyFilter   = "filter_fast"
)

// ParseFastTierPolicy 校验并归一化 Fast Tier 出站策略。
func ParseFastTierPolicy(policy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case FastTierPolicyPreserve:
		return FastTierPolicyPreserve, true
	case FastTierPolicyForce:
		return FastTierPolicyForce, true
	case FastTierPolicyFilter:
		return FastTierPolicyFilter, true
	default:
		return "", false
	}
}

// NormalizeFastTierPolicy 归一化 Fast Tier 出站策略，空值或未知值回退为保持请求。
func NormalizeFastTierPolicy(policy string) string {
	if normalized, ok := ParseFastTierPolicy(policy); ok {
		return normalized
	}
	return FastTierPolicyPreserve
}
