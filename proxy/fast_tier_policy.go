package proxy

import (
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func applyFastTierPolicy(body []byte, policy string) ([]byte, string) {
	switch database.NormalizeFastTierPolicy(policy) {
	case database.FastTierPolicyForce:
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		body, _ = sjson.SetBytes(body, "service_tier", "priority")
	case database.FastTierPolicyFilter:
		body, _ = sjson.DeleteBytes(body, "service_tier")
		body, _ = sjson.DeleteBytes(body, "serviceTier")
	default:
		body = normalizeServiceTierField(body)
		body = sanitizeServiceTierForUpstream(body)
	}

	return body, strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
}

func applyAccountFastTierPolicy(body []byte, account *auth.Account) ([]byte, string) {
	policy := database.FastTierPolicyPreserve
	if account != nil {
		policy = account.FastTierPolicy()
	}
	return applyFastTierPolicy(body, policy)
}
