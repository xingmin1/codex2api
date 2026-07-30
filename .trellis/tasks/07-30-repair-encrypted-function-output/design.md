# 技术设计

## 数据流

客户端请求体 → Responses 请求预处理 → 上游 HTTP/WebSocket → HTTP 错误或 `response.failed` → 错误分类 → 函数输出密文专用净化 → 同账号重试 → 仅转发最终结果。

## 修复边界

在 `encrypted_content_compatibility.go` 中扩展现有错误修复器，集中拥有 JSON 契约和净化规则：

1. 从现有错误形态中提取 code、param、message。
2. 当错误明确指向函数输出时，仅遍历顶层 input 内的 `function_call_output` 和 `custom_tool_call_output`。
3. 对 output 内容数组删除 `type=encrypted_content` 项，保留其他项。数组可合法为空。
4. 返回结构化报告，供各传输路径记录策略和控制单次重试。

## 传输集成

- relay HTTP/SSE：复用既有 compatibility repair，无需单独复制净化逻辑。
- 官方 HTTP/SSE：在通用全量密文剥离前优先调用专用修复，`response.failed` 在首个语义内容前进行同账号重试。
- 下游 WebSocket：流读取函数把“可修复的无效函数输出密文”作为带终止负载的重试结果返回，外层净化原始体与 Codex 体后同账号重试。

## 安全与兼容

- 仅精确错误触发，不对普通 4xx 或传输错误更改请求。
- 不删除完整函数输出项，避免形成悬空 tool call。
- 不触碰 compaction、context_compaction 或 agent_message。
- 单个请求只修复一次，防止无限循环。
- 若专用净化没有产生变化，则保持现有失败处理。

## 部署切换

- 使用源码 flake 构建独立 Nix store 包，不覆盖当前运行二进制。
- 先以隔离端口和临时数据库启动新包并检查 `/health`，确认可执行文件可运行。
- 通过 systemd 用户服务 drop-in 替换 `ExecStart`，重载后重启服务。现有程序收到 SIGTERM 后调用 `http.Server.Shutdown`，最多等待 360 秒完成在途请求。
- 切换后校验新 `ExecStart`、PID、`/health` 与日志。回滚只需删除 drop-in 并重启，恢复当前发布包。
