package wsrelay

import "time"

// Touch 更新最后业务活跃时间，并永久标记该会话曾承载用户聊天。
func (s *Session) Touch() {
	s.touchActivity(true)
}

func (s *Session) touchActivity(userContext bool) {
	s.mu.Lock()
	s.LastActiveAt = time.Now()
	s.userContext = s.userContext || userContext
	s.mu.Unlock()
}

func (s *Session) hasUserContext() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.userContext
}

func (s *Session) markUserContext() {
	s.mu.Lock()
	s.userContext = true
	s.mu.Unlock()
}

// IsExpired 检查业务空闲超时；空白会话在普通模式下继续保持连接。
func (s *Session) IsExpired() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.userContext && !weakNetworkModeEnabled() {
		return false
	}
	return time.Since(s.LastActiveAt) >= connectionIdleTimeout()
}
