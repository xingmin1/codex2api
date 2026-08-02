package database

const (
	// DefaultClientRequestReplayMaxRetries 是原始请求失败后的默认额外重发次数。
	DefaultClientRequestReplayMaxRetries = 5
	// MinClientRequestReplayMaxRetries 是允许的最小额外重发次数。
	MinClientRequestReplayMaxRetries = 1
	// MaxClientRequestReplayMaxRetries 是允许的最大额外重发次数。
	MaxClientRequestReplayMaxRetries = 10

	// DefaultClientRequestReplayMaxDurationSeconds 是首个业务输出前的默认总预算。
	DefaultClientRequestReplayMaxDurationSeconds = 600
	// MinClientRequestReplayMaxDurationSeconds 是允许的最小总预算。
	MinClientRequestReplayMaxDurationSeconds = 30
	// MaxClientRequestReplayMaxDurationSeconds 是允许的最大总预算。
	MaxClientRequestReplayMaxDurationSeconds = 3600

	// DefaultClientRequestReplayBaseIntervalMS 是第一次额外重发前的默认等待时间。
	DefaultClientRequestReplayBaseIntervalMS = 1000
	// MaxClientRequestReplayBaseIntervalMS 是允许的最大基础等待时间。
	MaxClientRequestReplayBaseIntervalMS = 60000

	// DefaultClientRequestReplayMaxIntervalSeconds 是指数退避的默认最大间隔。
	DefaultClientRequestReplayMaxIntervalSeconds = 30
	// MinClientRequestReplayMaxIntervalSeconds 是允许的最小最大间隔。
	MinClientRequestReplayMaxIntervalSeconds = 1
	// MaxClientRequestReplayMaxIntervalSeconds 是允许的最大最大间隔。
	MaxClientRequestReplayMaxIntervalSeconds = 300
)

// NormalizeClientRequestReplayMaxRetries 将旧版无限值和越界值收敛到有限范围。
func NormalizeClientRequestReplayMaxRetries(value int) int {
	if value < MinClientRequestReplayMaxRetries {
		return DefaultClientRequestReplayMaxRetries
	}
	if value > MaxClientRequestReplayMaxRetries {
		return MaxClientRequestReplayMaxRetries
	}
	return value
}

// NormalizeClientRequestReplayMaxDurationSeconds 归一化首个业务输出前的总预算。
func NormalizeClientRequestReplayMaxDurationSeconds(value int) int {
	if value <= 0 {
		return DefaultClientRequestReplayMaxDurationSeconds
	}
	if value < MinClientRequestReplayMaxDurationSeconds {
		return MinClientRequestReplayMaxDurationSeconds
	}
	if value > MaxClientRequestReplayMaxDurationSeconds {
		return MaxClientRequestReplayMaxDurationSeconds
	}
	return value
}

// NormalizeClientRequestReplayBaseIntervalMS 归一化整请求指数退避的基础间隔。
func NormalizeClientRequestReplayBaseIntervalMS(value int) int {
	if value < 0 {
		return 0
	}
	if value > MaxClientRequestReplayBaseIntervalMS {
		return MaxClientRequestReplayBaseIntervalMS
	}
	return value
}

// NormalizeClientRequestReplayMaxIntervalSeconds 归一化整请求指数退避的最大间隔。
func NormalizeClientRequestReplayMaxIntervalSeconds(value int) int {
	if value < MinClientRequestReplayMaxIntervalSeconds {
		return DefaultClientRequestReplayMaxIntervalSeconds
	}
	if value > MaxClientRequestReplayMaxIntervalSeconds {
		return MaxClientRequestReplayMaxIntervalSeconds
	}
	return value
}
