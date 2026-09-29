package auth

import "sync/atomic"

// The zero value preserves the pre-master-switch behavior. This process-wide
// gate is independent of every account's durable opt-in flag.
var openAIExcelBPSDisabled atomic.Bool

func OpenAIExcelBPSEnabled() bool { return !openAIExcelBPSDisabled.Load() }

// SetOpenAIExcelBPSEnabled publishes a successfully persisted setting.
func SetOpenAIExcelBPSEnabled(enabled bool) { openAIExcelBPSDisabled.Store(!enabled) }

func (s *Store) OpenAIExcelBPSEnabled() bool           { return OpenAIExcelBPSEnabled() }
func (s *Store) SetOpenAIExcelBPSEnabled(enabled bool) { SetOpenAIExcelBPSEnabled(enabled) }
