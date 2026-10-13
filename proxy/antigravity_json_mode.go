package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tidwall/gjson"
)

// antigravityJSONModeTool is the function Cloud Code adds when a Claude model
// is asked for schema-constrained output (generationConfig.responseSchema).
// The backend forces Claude to call it and hands the call back unchanged: a
// generateContent answer is a json_mode functionCall whose args hold the
// structured object, with finishReason OTHER. streamGenerateContent already
// turns the same call into text deltas ending in STOP.
const antigravityJSONModeTool = "json_mode"

// antigravityClaudeJSONModeRequest reports whether an outgoing envelope asks a
// Claude model for schema-constrained output, so its answer has to be read
// back through antigravityRewriteJSONModeFrame. A request that declares its
// own json_mode tool is left alone: a call to it is a real tool call.
func antigravityClaudeJSONModeRequest(payload []byte) bool {
	if !strings.Contains(strings.ToLower(gjson.GetBytes(payload, "model").String()), "claude") {
		return false
	}
	hasSchema := false
	for _, path := range []string{
		"request.generationConfig.responseSchema", "request.generationConfig.response_schema",
		"request.generation_config.responseSchema", "request.generation_config.response_schema",
	} {
		if value := gjson.GetBytes(payload, path); value.Exists() && value.Type != gjson.Null {
			hasSchema = true
			break
		}
	}
	if !hasSchema {
		return false
	}
	declared := false
	gjson.GetBytes(payload, "request.tools").ForEach(func(_, tool gjson.Result) bool {
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			tool.Get(key).ForEach(func(_, declaration gjson.Result) bool {
				declared = strings.TrimSpace(declaration.Get("name").String()) == antigravityJSONModeTool
				return !declared
			})
			if declared {
				return false
			}
		}
		return true
	})
	return !declared
}

// antigravityRewriteJSONModeFrame turns one upstream frame of a json_mode
// answer (a generateContent body or one streamed data payload) into the answer
// the client asked for:
//   - a json_mode functionCall part becomes a text part holding its args, the
//     structured object. Any other text of that candidate is dropped so the
//     message text stays parseable JSON; thought parts are kept.
//   - finishReason OTHER becomes STOP once the answer has made a tool call.
//     Under the forced tool the backend reports every turn that ends in a
//     tool call as OTHER, a call to one of the client's own tools included.
//
// toolCalled carries across the frames of one stream, because the call and
// the finish reason can arrive in separate frames. The frame is returned as is
// when there is nothing to rewrite.
func antigravityRewriteJSONModeFrame(frame []byte, toolCalled bool) ([]byte, bool) {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return frame, toolCalled
	}
	// Plain text deltas, the bulk of a stream, carry neither a call nor a
	// finish reason and need no decoding.
	if !bytes.Contains(trimmed, []byte(`"functionCall"`)) && !bytes.Contains(trimmed, []byte(`"function_call"`)) &&
		!bytes.Contains(trimmed, []byte(`"finishReason"`)) && !bytes.Contains(trimmed, []byte(`"finish_reason"`)) {
		return frame, toolCalled
	}
	var envelope map[string]any
	if json.Unmarshal(trimmed, &envelope) != nil || envelope == nil {
		return frame, toolCalled
	}
	response, prefix := envelope, ""
	for {
		nested, ok := response["response"].(map[string]any)
		if !ok {
			break
		}
		response, prefix = nested, prefix+"response."
	}
	changed := false
	candidates, _ := response["candidates"].([]any)
	for candidateIndex, rawCandidate := range candidates {
		candidate, _ := rawCandidate.(map[string]any)
		if candidate == nil {
			continue
		}
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		partsPath := prefix + "candidates." + strconv.Itoa(candidateIndex) + ".content.parts."
		answers := map[int]bool{}
		for partIndex, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			key := "functionCall"
			call, _ := part[key].(map[string]any)
			if call == nil {
				key = "function_call"
				call, _ = part[key].(map[string]any)
			}
			if call == nil {
				continue
			}
			toolCalled = true
			if name, _ := call["name"].(string); strings.TrimSpace(name) != antigravityJSONModeTool {
				continue
			}
			// The args are read from the raw frame: decoding them into a map
			// would reorder the object's keys.
			args := gjson.GetBytes(trimmed, partsPath+strconv.Itoa(partIndex)+"."+key+".args")
			parts[partIndex] = map[string]any{"text": antigravityJSONModeText(args)}
			answers[partIndex] = true
		}
		if len(answers) > 0 {
			kept := make([]any, 0, len(parts))
			for partIndex, rawPart := range parts {
				part, _ := rawPart.(map[string]any)
				_, isText := part["text"].(string)
				if thought, _ := part["thought"].(bool); isText && !thought && !answers[partIndex] {
					continue
				}
				kept = append(kept, rawPart)
			}
			content["parts"] = kept
			changed = true
		}
		for _, key := range []string{"finishReason", "finish_reason"} {
			if reason, _ := candidate[key].(string); toolCalled && strings.EqualFold(strings.TrimSpace(reason), "OTHER") {
				candidate[key] = "STOP"
				changed = true
			}
		}
	}
	if !changed {
		return frame, toolCalled
	}
	out, err := json.Marshal(envelope)
	if err != nil {
		return frame, toolCalled
	}
	return out, toolCalled
}

// antigravityJSONModeText renders json_mode args as the JSON text of the
// structured answer.
func antigravityJSONModeText(args gjson.Result) string {
	switch {
	case !args.Exists() || args.Type == gjson.Null:
		return "{}"
	case args.Type == gjson.String:
		// Args sent as an already-encoded JSON document.
		return args.String()
	}
	var compact bytes.Buffer
	if json.Compact(&compact, []byte(args.Raw)) != nil {
		return args.Raw
	}
	return compact.String()
}

// antigravityJSONModeRejection replaces the upstream 400 a Claude model gives
// for the forced json_mode tool with an error that names what the client
// actually asked for. Claude 5.5 refuses forced tool use outright ("tool_choice:
// type "tool" and "any" are not supported for this model") and Claude 4.x
// refuses it while thinking is on; either way the client never chose a tool,
// so the upstream wording points at the wrong cause. Other 400s pass through.
func antigravityJSONModeRejection(resp *http.Response, model string) (*http.Response, error) {
	if resp == nil || resp.StatusCode != http.StatusBadRequest || resp.Body == nil {
		return resp, nil
	}
	body, err := readBoundedAntigravityBody(resp.Body, antigravityErrorBodyLimit)
	if err != nil {
		body = []byte(`{"error":{"message":"Antigravity upstream error response exceeded the safe read limit","type":"upstream_error"}}`)
		resp.Header.Set("Content-Type", "application/json")
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	upstreamMessage := antigravityUpstreamErrorMessage(body)
	if !strings.Contains(strings.ToLower(upstreamMessage), "tool_choice") {
		return resp, nil
	}
	_ = resp.Body.Close()
	return nil, &Error{
		Code: "antigravity_feature_unsupported",
		Message: fmt.Sprintf("Antigravity model %s does not support json_schema structured output: the upstream implements it as a forced tool call, which this model rejects (upstream: %s)",
			strings.TrimSpace(model), upstreamMessage),
		Type:       ErrorTypeInvalidRequest,
		HTTPStatus: http.StatusBadRequest,
	}
}

// antigravityUpstreamErrorMessage returns the message of a Google error body,
// unwrapping the Anthropic error document the backend embeds for Claude.
func antigravityUpstreamErrorMessage(body []byte) string {
	message := strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	if gjson.Valid(message) {
		if inner := strings.TrimSpace(gjson.Get(message, "error.message").String()); inner != "" {
			return inner
		}
	}
	return message
}
