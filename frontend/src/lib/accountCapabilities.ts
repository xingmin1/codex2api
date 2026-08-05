import type { AccountRow } from "../types";

export type AccountCapabilityRow = Pick<
  AccountRow,
  "models" | "openai_responses_api" | "quality_eval_supported"
>;

/** 判断账号是否具备 gpt-5.6-sol 质量检测能力。 */
export function accountSupportsQualityEval(
  account: AccountCapabilityRow | null | undefined,
): boolean {
  if (!account) return false;
  if (!account.openai_responses_api) return true;
  if (account.models !== undefined) {
    return account.models.some(
      (model) => model.trim().toLowerCase() === "gpt-5.6-sol",
    );
  }
  return account.quality_eval_supported ?? false;
}
