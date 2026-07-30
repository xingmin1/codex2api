# 实施计划

1. 扩展 `repairResponsesEncryptedContentForError`，添加函数输出密文识别与专用净化器。
2. 添加纯函数测试，覆盖混合内容、仅密文、非函数输出、受保护状态。
3. 将官方 HTTP/SSE 失败路径接入专用修复，并确保同账号仅重试一次。
4. 扩展 Responses WebSocket 的流式重试结果，携带终止失败负载，外层净化后重试。
5. 添加 relay、官方 SSE 和 WebSocket 端到端回归测试。
6. 运行 `gofmt`、定向测试、proxy 包测试与相关静态检查。
7. 运行 GitNexus `detect_changes`，确认只影响预期符号和执行流。
8. 用源码 flake 构建新 Nix 包，在隔离端口完成健康预检。
9. 写入可回退的 systemd 用户服务 drop-in，优雅重启并验证新进程、健康端点和错误日志。

## 回滚点

- 专用净化器与传输集成分开提交或保持可独立回退。
- 若 WebSocket 状态机测试暴露行为冲突，保留 HTTP/SSE 修复并单独回退 WebSocket 集成。
- 服务切换异常时删除 drop-in、daemon-reload 并重启，恢复原 `/nix/store/xyi1bri8886l5qfm1izh2c8ccpsgn5xn-codex2api-2.5.2-xmin.14` 发布包。
