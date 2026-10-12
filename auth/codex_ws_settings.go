package auth

import "github.com/codex2api/database"

// SetCodexWSDownstreamKeepaliveSlots 设置每账号独立的空闲聊天连接保留上限。
func (s *Store) SetCodexWSDownstreamKeepaliveSlots(slots int) {
	if s != nil {
		s.codexWSDownstreamKeepaliveSlots.Store(int64(database.NormalizeCodexWSDownstreamKeepaliveSlots(slots)))
	}
}

// CodexWSDownstreamKeepaliveSlots 返回每账号独立的空闲聊天连接保留上限。
func (s *Store) CodexWSDownstreamKeepaliveSlots() int {
	if s == nil {
		return database.DefaultCodexWSDownstreamKeepaliveSlots
	}
	return database.NormalizeCodexWSDownstreamKeepaliveSlots(int(s.codexWSDownstreamKeepaliveSlots.Load()))
}
