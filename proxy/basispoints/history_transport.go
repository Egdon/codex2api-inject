package basispoints

import (
	"encoding/json"
	"fmt"
	"strings"
)

// toolDefinitionFingerprint identifies a declaration by its call contract.
// Descriptions and lazy-loading hints change as clients refresh their tool
// catalog without changing how a call is decoded, so they must not turn a
// repeated declaration into a "conflicting duplicate" and reject the request.
// Every other field, including unknown ones, stays part of the identity.
func toolDefinitionFingerprint(item object) string {
	contract := make(object, len(item))
	for field, value := range item {
		switch field {
		case "description", "defer_loading":
			continue
		}
		contract[field] = value
	}
	return fingerprint(contract)
}

// rebuildHistoryCall rebuilds a client tool call that has no cached native
// item, using the transport the current catalog tells the model to use.
// Otherwise history would show a custom or raw-field tool called through the
// JSON envelope while the catalog asks for the raw form, which invites the
// model to emit malformed calls. Anything else keeps the JSON envelope.
func (b *Bridge) rebuildHistoryCall(item object) (object, error) {
	if isBuiltinCall(item) {
		return rebuildNativeHistoryCall(item)
	}
	name := text(item["name"])
	if namespace := text(item["namespace"]); namespace != "" {
		name = namespace + "." + name
	}
	info, declared := b.tools[name]
	if !declared {
		return rebuildNativeHistoryCall(item)
	}
	switch {
	case text(item["type"]) == "custom_tool_call" && info.Kind == "custom":
		input, ok := item["input"].(string)
		if !ok {
			return rebuildNativeHistoryCall(item)
		}
		return rawTransportHistoryCall(item, customTransportPrefix+name, input)
	case text(item["type"]) == "function_call" && info.Kind == "function" && info.RawField != "" && !info.RawAsList:
		arguments := item["arguments"]
		if encoded, ok := arguments.(string); ok {
			if decode([]byte(encoded), &arguments) != nil {
				return rebuildNativeHistoryCall(item)
			}
		}
		args, ok := arguments.(object)
		value, isString := args[info.RawField].(string)
		// The raw form carries only the raw field; other arguments would be lost.
		if !ok || len(args) != 1 || !isString {
			return rebuildNativeHistoryCall(item)
		}
		return rawTransportHistoryCall(item, rawFieldTransportPrefix+name+"/"+info.RawField, value)
	}
	return rebuildNativeHistoryCall(item)
}

// rawTransportHistoryCall wraps raw client input in the run_officejs item BPS
// knows, keeping the client's call_id so the following result still pairs.
func rawTransportHistoryCall(item object, summary, code string) (object, error) {
	id := text(item["call_id"])
	if id == "" || strings.TrimSpace(id) != id {
		return nil, fmt.Errorf("basispoints history recovery requires a complete tool call with nonempty call_id")
	}
	arguments, err := json.Marshal(object{
		"code": code, "summary": summary,
		"extended_summary": "The supplied client history contains this tool call; consume its recorded result without repeating it.",
		"destructive":      false, "references": []any{},
	})
	if err != nil {
		return nil, fmt.Errorf("basispoints history transport cannot be serialized")
	}
	itemID := text(item["id"])
	if !strings.HasPrefix(itemID, "fc_") || len(itemID) > 64 {
		itemID = "fc_" + fingerprint(id)
	}
	return object{
		"type": "function_call", "id": itemID, "call_id": id, "name": "run_officejs",
		"arguments": string(arguments), "status": "completed",
	}, nil
}

// StripReasoningEncryptedContent removes opaque reasoning items from a
// prepared Basispoints request so it can be retried once after the upstream
// rejects encrypted reasoning it cannot verify (for example, reasoning that
// native Codex produced before a fallback). It refuses when no such item
// exists or when any other opaque content remains, since that content would
// fail the same way and must not be silently dropped.
func StripReasoningEncryptedContent(prepared []byte) ([]byte, bool) {
	var request object
	if decode(prepared, &request) != nil || request == nil {
		return nil, false
	}
	input, ok := request["input"].([]any)
	if !ok {
		return nil, false
	}
	kept := make([]any, 0, len(input))
	removed := false
	for _, raw := range input {
		item, _ := raw.(object)
		if text(item["type"]) == "reasoning" && text(item["encrypted_content"]) != "" {
			removed = true
			continue
		}
		if containsEncryptedContent(item) {
			return nil, false
		}
		kept = append(kept, raw)
	}
	if !removed {
		return nil, false
	}
	request["input"] = kept
	body, err := json.Marshal(request)
	if err != nil {
		return nil, false
	}
	return body, true
}

func containsEncryptedContent(value any) bool {
	switch typed := value.(type) {
	case object:
		for key, nested := range typed {
			if key == "encrypted_content" && text(nested) != "" {
				return true
			}
			if text(typed["type"]) == "encrypted_content" {
				return true
			}
			if containsEncryptedContent(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if containsEncryptedContent(nested) {
				return true
			}
		}
	}
	return false
}
