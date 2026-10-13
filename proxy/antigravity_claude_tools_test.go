package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// collectSchemaTypes returns every "type" value found anywhere in a schema.
func collectSchemaTypes(value any, out *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == "type" {
				if s, ok := item.(string); ok {
					*out = append(*out, s)
				}
				continue
			}
			collectSchemaTypes(item, out)
		}
	case []any:
		for _, item := range typed {
			collectSchemaTypes(item, out)
		}
	}
}

// Claude models receive parametersJsonSchema verbatim as Anthropic input_schema,
// which rejects any uppercase type at any depth (issue #783).
func TestAntigravityToolSchemaTypesAreLowercaseAtEveryLevel(t *testing.T) {
	schema := antigravityGeminiParameters(map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"message": map[string]any{"type": "STRING"},
			"count":   map[string]any{"type": []any{"INTEGER", "null"}},
			"tags":    map[string]any{"type": "ARRAY", "items": map[string]any{"type": "String"}},
			"nested": map[string]any{
				"anyOf": []any{
					map[string]any{"type": "null"},
					map[string]any{"type": "OBJECT", "properties": map[string]any{"flag": map[string]any{"type": "BOOLEAN"}}},
				},
			},
			"untyped": map[string]any{"properties": map[string]any{"x": map[string]any{"type": "NUMBER"}}},
		},
		"required": []any{"message"},
	})
	var types []string
	collectSchemaTypes(schema, &types)
	if len(types) == 0 {
		t.Fatalf("no types collected: %#v", schema)
	}
	for _, schemaType := range types {
		if schemaType != strings.ToLower(schemaType) {
			encoded, _ := json.Marshal(schema)
			t.Fatalf("type %q is not lowercase in %s", schemaType, encoded)
		}
	}
	if schema["type"] != "object" {
		t.Fatalf("root type = %#v", schema["type"])
	}
}

func TestAntigravityToolSchemaDefaultsAreLowercase(t *testing.T) {
	for name, raw := range map[string]any{
		"missing":    nil,
		"empty":      map[string]any{},
		"properties": map[string]any{"properties": map[string]any{"q": map[string]any{"type": "string"}}},
	} {
		schema := antigravityGeminiParameters(raw)
		var types []string
		collectSchemaTypes(schema, &types)
		for _, schemaType := range types {
			if schemaType != strings.ToLower(schemaType) {
				t.Fatalf("%s: type %q is not lowercase in %#v", name, schemaType, schema)
			}
		}
	}
	custom := antigravityGeminiCustomToolDeclaration(map[string]any{"type": "custom", "name": "apply_patch"})
	var types []string
	collectSchemaTypes(custom["parametersJsonSchema"], &types)
	for _, schemaType := range types {
		if schemaType != strings.ToLower(schemaType) {
			t.Fatalf("custom tool type %q is not lowercase in %#v", schemaType, custom)
		}
	}
}

func TestRelaxAntigravityClaudeForcedToolChoice(t *testing.T) {
	cases := []struct {
		name      string
		model     string
		mode      string
		thinking  map[string]any
		wantMode  string
		wantNames bool
	}{
		{name: "claude 5.5 refuses forced tools", model: "claude-sonnet-5-5-low", mode: "ANY", wantMode: "AUTO"},
		{name: "claude 5.5 opus tier", model: "claude-opus-5-5-high", mode: "ANY", wantMode: "AUTO"},
		{name: "claude 5.5 refuses even with zero budget", model: "claude-sonnet-5-5-medium", mode: "ANY", thinking: map[string]any{"thinkingBudget": 0}, wantMode: "AUTO"},
		{name: "unknown future claude degrades", model: "claude-opus-6-0", mode: "ANY", wantMode: "AUTO"},
		{name: "claude 4.x without thinking keeps forcing", model: "claude-sonnet-4-6", mode: "ANY", wantMode: "ANY", wantNames: true},
		{name: "claude 4.x thinking model id without config keeps forcing", model: "claude-opus-4-6-thinking", mode: "ANY", wantMode: "ANY", wantNames: true},
		{name: "claude 4.x with thinking budget degrades", model: "claude-opus-4-6-thinking", mode: "ANY", thinking: map[string]any{"thinkingBudget": 2048, "includeThoughts": true}, wantMode: "AUTO"},
		{name: "claude 4.x with thinking level degrades", model: "claude-opus-4-6-thinking", mode: "ANY", thinking: map[string]any{"thinkingLevel": "HIGH"}, wantMode: "AUTO"},
		{name: "claude 4.x with thinking disabled keeps forcing", model: "claude-opus-4-6-thinking", mode: "ANY", thinking: map[string]any{"thinkingBudget": 0}, wantMode: "ANY", wantNames: true},
		{name: "claude auto untouched", model: "claude-sonnet-5-5-low", mode: "AUTO", wantMode: "AUTO", wantNames: true},
		{name: "claude validated untouched", model: "claude-sonnet-5-5-low", mode: "VALIDATED", wantMode: "VALIDATED", wantNames: true},
		{name: "gemini keeps forcing", model: "gemini-3-flash", mode: "ANY", thinking: map[string]any{"thinkingBudget": 2048}, wantMode: "ANY", wantNames: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := map[string]any{
				"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
				"toolConfig": map[string]any{"functionCallingConfig": map[string]any{
					"mode":                 tc.mode,
					"allowedFunctionNames": []any{"test_tool"},
				}},
			}
			if tc.thinking != nil {
				request["generationConfig"] = map[string]any{"thinkingConfig": tc.thinking}
			}
			payload, _ := json.Marshal(map[string]any{"project": "p", "model": tc.model, "request": request})
			out := relaxAntigravityClaudeForcedToolChoice(payload)
			if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.mode").String(); got != tc.wantMode {
				t.Fatalf("mode = %q, want %q", got, tc.wantMode)
			}
			if got := gjson.GetBytes(out, "request.toolConfig.functionCallingConfig.allowedFunctionNames").Exists(); got != tc.wantNames {
				t.Fatalf("allowedFunctionNames present = %v, want %v", got, tc.wantNames)
			}
			if tc.wantMode == tc.mode && string(out) != string(payload) {
				t.Fatalf("payload must be untouched when no downgrade applies:\n%s\n%s", payload, out)
			}
		})
	}
}

// The Anthropic Messages repro from issue #783 must reach the upstream with a
// lowercase input schema and, for a Claude 5.5 backing model, a non-forced mode.
func TestAntigravityIssue783MessagesToolRequestEnvelope(t *testing.T) {
	in := map[string]any{
		"model": "claude-sonnet-5-5-low",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": "Please call test_tool with message hello"}},
		"tools": []any{map[string]any{
			"type":        "function",
			"name":        "test_tool",
			"description": "Test tool calling",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"message": map[string]any{"type": "string"}},
				"required":   []any{"message"},
			},
		}},
		"tool_choice": map[string]any{"type": "function", "name": "test_tool"},
	}
	raw, _ := json.Marshal(in)
	envelope, err := responsesToGeminiInternal(raw, "project-1", "claude-sonnet-5-5-low")
	if err != nil {
		t.Fatalf("responsesToGeminiInternal: %v", err)
	}
	payload, _ := json.Marshal(envelope)
	payload = sanitizeAntigravityEnvelopeToolSchemas(payload)
	payload = relaxAntigravityClaudeForcedToolChoice(payload)
	schema := gjson.GetBytes(payload, "request.tools.0.functionDeclarations.0.parametersJsonSchema")
	if schema.Get("type").String() != "object" || schema.Get("properties.message.type").String() != "string" {
		t.Fatalf("schema = %s", schema.Raw)
	}
	if mode := gjson.GetBytes(payload, "request.toolConfig.functionCallingConfig.mode").String(); mode != "AUTO" {
		t.Fatalf("mode = %q, want AUTO for Claude 5.5", mode)
	}
}
