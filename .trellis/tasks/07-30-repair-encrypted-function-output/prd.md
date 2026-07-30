# 自动恢复无效的加密函数输出

## Goal

当 Responses 上游拒绝无法解密的函数调用输出密文时，由代理透明、保守地清理不可用密文并用同一账号重试一次，避免请求持续失败。

## Requirements

- 识别 HTTP 400 以及 HTTP 200 流中的 `response.failed`，错误码为 `invalid_encrypted_content`，消息指向 encrypted function output。
- 仅移除 `function_call_output` 与 `custom_tool_call_output` 的 `output` 内容数组中 `type=encrypted_content` 的项。
- 保留函数输出项本身、`call_id`、其他明文或媒体内容，以及 reasoning 之外受保护的 compaction 和 agent_message 状态。
- HTTP、SSE 和下游 Responses WebSocket 入口都必须执行同一修复语义。
- 每个请求最多自动执行一次该修复，并优先使用原账号重试。
- 首次失败前未发送语义内容时，不向下游泄露被修复的失败事件。

## Acceptance Criteria

- [ ] 专用单元测试证明只删除函数输出密文项并保留其余字段。
- [ ] relay HTTP/SSE 兼容路径能修复并成功重试。
- [ ] 官方 Responses SSE `response.failed` 路径能修复并成功重试。
- [ ] Responses WebSocket 的 `response.failed` 路径能修复并成功重试。
- [ ] 已有 reasoning、compaction 与 agent_message 兼容测试保持通过。
- [ ] 修复失败或第二次仍失败时不进入无限重试。
- [ ] 新版本构建成功，服务切换后健康检查、进程版本与错误恢复行为均符合预期，旧版本仍可快速回退。

## Notes

- 上游 `FunctionCallOutputBody` 的当前契约为字符串或内容数组，其中内容项包含 `encrypted_content` 变体。
- 无法解密的内容不可恢复，因此使用合法的空内容数组表达“无可用输出”，不伪造明文结果。
