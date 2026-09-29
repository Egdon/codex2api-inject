package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Requests that need a hosted capability stay on native Codex: live web
// search, and explicit or natural-language image generation. The Basispoints
// bridge can only omit hosted tools, which would silently degrade them.
// Cached web search and an unused image tool are omitted as before.

// excelBPSImageIntentReason inspects the prepared Codex body.
func excelBPSImageIntentReason(body []byte) string {
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "tool_choice").String()), "none") {
		return ""
	}
	if rawResponsesBodyShouldForceHTTPForImageGeneration(body) {
		return "image_generation"
	}
	return ""
}

// excelBPSLiveWebSearchReason inspects the client's own Responses body:
// request preparation normalizes web_search declarations and drops the
// external_web_access/search_context_size hints that tell live from cached.
func excelBPSLiveWebSearchReason(raw []byte) string {
	if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(raw, "tool_choice").String()), "none") {
		return ""
	}
	choice := strings.ToLower(strings.TrimSpace(gjson.GetBytes(raw, "tool_choice.type").String()))
	if strings.HasPrefix(choice, "web_search") {
		return "web_search"
	}
	live := false
	gjson.GetBytes(raw, "tools").ForEach(func(_, tool gjson.Result) bool {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(tool.Get("type").String())), "web_search") &&
			(tool.Get("external_web_access").Bool() || strings.EqualFold(tool.Get("search_context_size").String(), "high")) {
			live = true
			return false
		}
		return true
	})
	if live {
		return "web_search"
	}
	return ""
}

// isExcelBPSInvalidEncryptedContent recognizes the upstream rejection of
// encrypted reasoning it cannot verify, by code or by its fixed message.
func isExcelBPSInvalidEncryptedContent(body []byte) bool {
	if gjson.GetBytes(body, "error.code").String() == "invalid_encrypted_content" {
		return true
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	return strings.Contains(message, "encrypted content") &&
		(strings.Contains(message, "could not be verified") || strings.Contains(message, "could not be decrypted"))
}
