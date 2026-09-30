package basispoints

import "encoding/json"

// cacheWriteUsageFields lists the cache-creation counters a Responses usage
// object may carry, as paths below the usage object. Basispoints reports
// input_tokens_details.cache_write_tokens; the other spellings are the
// aliases downstream gateways read as cache creation.
var cacheWriteUsageFields = [][]string{
	{"input_tokens_details", "cache_write_tokens"},
	{"prompt_tokens_details", "cache_write_tokens"},
	{"input_tokens_details", "cache_creation_tokens"},
	{"prompt_tokens_details", "cache_creation_tokens"},
	{"cache_write_tokens"},
	{"cache_creation_input_tokens"},
	{"cache_write_input_tokens"},
	{"cache_creation_tokens"},
	{"cache_creation", "ephemeral_5m_input_tokens"},
	{"cache_creation", "ephemeral_1h_input_tokens"},
}

// reportCacheWritesAsInput zeroes the cache-creation counters of every usage
// object in an event (top-level usage and response.usage). OpenAI
// input_tokens already includes cache creation, so those tokens stay billable
// as ordinary input; input_tokens, cached_tokens and total_tokens are kept.
// Native Codex never reports cache creation, so after this a Basispoints
// response bills like a native one. Counters that are absent stay absent.
func reportCacheWritesAsInput(payload object) bool {
	changed := false
	if usage, ok := payload["usage"].(object); ok {
		changed = zeroCacheWrites(usage) || changed
	}
	if response, ok := payload["response"].(object); ok {
		if usage, ok := response["usage"].(object); ok {
			changed = zeroCacheWrites(usage) || changed
		}
	}
	return changed
}

func zeroCacheWrites(usage object) bool {
	changed := false
	for _, path := range cacheWriteUsageFields {
		parent := usage
		for _, key := range path[:len(path)-1] {
			parent, _ = parent[key].(object)
			if parent == nil {
				break
			}
		}
		if parent == nil {
			continue
		}
		key := path[len(path)-1]
		value, exists := parent[key]
		if !exists || isZeroCount(value) {
			continue
		}
		parent[key] = json.Number("0")
		changed = true
	}
	return changed
}

func isZeroCount(value any) bool {
	switch v := value.(type) {
	case json.Number:
		return v == "0"
	case float64:
		return v == 0
	case int:
		return v == 0
	}
	return false
}
