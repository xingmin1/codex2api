import assert from "node:assert/strict";
import test from "node:test";

import { accountSupportsQualityEval } from "./accountCapabilities.ts";

test("ordinary Codex accounts support quality evaluation", () => {
  assert.equal(accountSupportsQualityEval({ openai_responses_api: false }), true);
});

test("explicit Responses API model list is authoritative", () => {
  assert.equal(
    accountSupportsQualityEval({
      openai_responses_api: true,
      models: ["gpt-5.6-sol"],
      quality_eval_supported: false,
    }),
    true,
  );
  assert.equal(
    accountSupportsQualityEval({
      openai_responses_api: true,
      models: ["gpt-5.5"],
      quality_eval_supported: true,
    }),
    false,
  );
});

test("missing model list falls back to the backend capability field", () => {
  assert.equal(
    accountSupportsQualityEval({
      openai_responses_api: true,
      quality_eval_supported: true,
    }),
    true,
  );
  assert.equal(
    accountSupportsQualityEval({
      openai_responses_api: true,
      quality_eval_supported: false,
    }),
    false,
  );
});
