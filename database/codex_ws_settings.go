package database

const (
	DefaultCodexWSDownstreamKeepaliveSlots = 8
	MaxCodexWSDownstreamKeepaliveSlots     = 32
)

// NormalizeCodexWSDownstreamKeepaliveSlots 归一化空闲聊天连接保留上限，0 表示不保留。
func NormalizeCodexWSDownstreamKeepaliveSlots(slots int) int {
	if slots < 0 {
		return DefaultCodexWSDownstreamKeepaliveSlots
	}
	if slots > MaxCodexWSDownstreamKeepaliveSlots {
		return MaxCodexWSDownstreamKeepaliveSlots
	}
	return slots
}
