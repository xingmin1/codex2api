package proxy

import (
	"context"
	"time"
)

// upstreamDrainTimeout 控制客户端断开后，最多继续读上游多久以提取 usage。
// 取值参考：上游 response.completed 一般在断开后 1-3 秒内到达；
// 5 秒兜住绝大多数情况，又不会在上游卡住时无限占用连接。
const upstreamDrainTimeout = 5 * time.Second

// newDrainableUpstreamContext 创建一个可有限排空的上游 context。
// 普通请求保持原有排空行为；整请求代重发只在业务数据已交付后排空，
// 首个业务输出前的取消会立即停止上游，避免客户端断开后继续创建或轮转请求。
//
// 行为：
//   - 客户端 ctx 取消后，等待 drainTimeout 再 cancel 上游
//   - 如果在 drainTimeout 之前调用方主动 cancel，立即停止
//   - parent 为 nil 时退化到 context.Background
func newDrainableUpstreamContext(clientCtx context.Context, drainTimeout time.Duration) (context.Context, context.CancelFunc) {
	upstreamCtx, cancelUpstream := context.WithCancel(context.Background())
	if clientCtx == nil || drainTimeout <= 0 {
		return upstreamCtx, cancelUpstream
	}
	if clientCtx.Err() != nil {
		cancelUpstream()
		return upstreamCtx, cancelUpstream
	}
	go func() {
		select {
		case <-clientCtx.Done():
			controller := clientRequestReplayControllerFromContext(clientCtx)
			if controller != nil && !controller.hasBusinessStarted() {
				cancelUpstream()
				return
			}
			timer := time.NewTimer(drainTimeout)
			defer timer.Stop()
			select {
			case <-timer.C:
				cancelUpstream()
			case <-upstreamCtx.Done():
			}
		case <-upstreamCtx.Done():
		}
	}()
	return upstreamCtx, cancelUpstream
}
