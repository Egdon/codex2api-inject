package basispoints

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// IsPlaintextAgentContent recognizes text produced by the BPS tool relay that
// the client wrapped in an encrypted schema field. Opaque tokens, wrapped
// base64 and native Fernet context are deliberately left untouched.
func IsPlaintextAgentContent(value string) bool {
	if !utf8.ValidString(value) || strings.HasPrefix(strings.TrimSpace(value), "gAAAA") {
		return false
	}
	hasLetter, hasSpace, hasNonASCIILetter := false, false, false
	for _, r := range value {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
		if unicode.IsLetter(r) {
			hasLetter = true
			if r > unicode.MaxASCII {
				hasNonASCIILetter = true
			}
		}
		if r == ' ' {
			hasSpace = true
		}
	}
	return hasNonASCIILetter || (hasLetter && hasSpace && len(strings.Fields(value)) > 1)
}

// NormalizeAgentMessage repairs only clearly plaintext encrypted parts. It
// preserves the complete text, author, recipient and real encrypted context.
func NormalizeAgentMessage(item map[string]any) bool {
	if text(item["type"]) != "agent_message" {
		return false
	}
	content, _ := item["content"].([]any)
	changed := false
	for _, raw := range content {
		part, ok := raw.(map[string]any)
		if !ok || text(part["type"]) != "encrypted_content" {
			continue
		}
		value, ok := part["encrypted_content"].(string)
		if !ok || !IsPlaintextAgentContent(value) {
			continue
		}
		if _, conflict := part["text"]; conflict {
			continue
		}
		part["type"] = "input_text"
		part["text"] = value
		delete(part, "encrypted_content")
		changed = true
	}
	return changed
}
