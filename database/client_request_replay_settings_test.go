package database

import "testing"

func TestNormalizeClientRequestReplaySettings(t *testing.T) {
	if got := NormalizeClientRequestReplayMaxRetries(0); got != DefaultClientRequestReplayMaxRetries {
		t.Fatalf("旧版无限值 0 = %d, want %d", got, DefaultClientRequestReplayMaxRetries)
	}
	if got := NormalizeClientRequestReplayMaxRetries(99); got != MaxClientRequestReplayMaxRetries {
		t.Fatalf("过大重发次数 = %d, want %d", got, MaxClientRequestReplayMaxRetries)
	}
	if got := NormalizeClientRequestReplayMaxDurationSeconds(0); got != DefaultClientRequestReplayMaxDurationSeconds {
		t.Fatalf("空总预算 = %d, want %d", got, DefaultClientRequestReplayMaxDurationSeconds)
	}
	if got := NormalizeClientRequestReplayBaseIntervalMS(-1); got != 0 {
		t.Fatalf("负基础间隔 = %d, want 0", got)
	}
	if got := NormalizeClientRequestReplayMaxIntervalSeconds(0); got != DefaultClientRequestReplayMaxIntervalSeconds {
		t.Fatalf("空最大间隔 = %d, want %d", got, DefaultClientRequestReplayMaxIntervalSeconds)
	}
}
