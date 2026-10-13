package proxy

import (
	"strings"
	"testing"

	"github.com/codex2api/api"
)

func TestAntigravityFlattenSchemaUnionsAnyOf(t *testing.T) {
	root := map[string]any{
		"type": "object",
		"anyOf": []any{
			map[string]any{"type": "null"},
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"q": map[string]any{"type": "string"},
				},
			},
		},
	}
	flattened, ok := antigravityFlattenSchemaUnions(root).(map[string]any)
	if !ok {
		t.Fatal("expected object schema")
	}
	if _, ok := flattened["anyOf"]; ok {
		t.Fatalf("anyOf must be flattened: %#v", flattened)
	}
	props, ok := flattened["properties"].(map[string]any)
	if !ok || props["q"] == nil {
		t.Fatalf("expected merged properties: %#v", flattened)
	}
	if flattened["nullable"] != true {
		t.Fatalf("expected nullable true: %#v", flattened)
	}
}

func TestAntigravitySanitizeFunctionName(t *testing.T) {
	got := antigravitySanitizeFunctionName("mcp/server/read")
	if got != "mcp_server_read" {
		t.Fatalf("sanitize = %q", got)
	}
	if len(got) > 64 {
		t.Fatalf("name too long: %d", len(got))
	}
}

func TestAntigravityGeminiFunctionNameMapDisambiguatesCollisions(t *testing.T) {
	raw := []byte(`{"tools":[{"functionDeclarations":[{"name":"a/b"},{"name":"a-b"}]}]}`)
	forward := antigravityGeminiFunctionNameMap(raw)
	if forward["a/b"] == "" || forward["a-b"] == "" {
		t.Fatalf("missing mappings: %#v", forward)
	}
	if forward["a/b"] == forward["a-b"] {
		t.Fatalf("colliding names must disambiguate: %#v", forward)
	}
	reverse := antigravityGeminiReverseNameMap(forward)
	if antigravityRestoreGeminiFunctionName(reverse, forward["a/b"]) != "a/b" {
		t.Fatalf("reverse map failed: %#v", reverse)
	}
}

func TestAntigravityRestoreNativeGeminiResponseNames(t *testing.T) {
	forward := map[string]string{"mcp/server/read": "mcp_server_read"}
	reverse := antigravityGeminiReverseNameMap(forward)
	body := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"mcp_server_read","args":{}}}]}}]}`)
	got := antigravityRestoreNativeGeminiResponseNames(body, reverse)
	if !strings.Contains(string(got), `"name":"mcp/server/read"`) {
		t.Fatalf("restored name missing: %s", got)
	}
}

func TestAntigravityApplyGeminiDeclarationSchemaUsesParametersJsonSchema(t *testing.T) {
	decl := map[string]any{
		"name": "lookup",
		"parametersJsonSchema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"q": map[string]any{"type": "string"},
			},
		},
	}
	antigravityApplyGeminiDeclarationSchema(decl)
	if _, ok := decl["parameters"]; ok {
		t.Fatal("parameters must be removed")
	}
	schema, ok := decl["parametersJsonSchema"].(map[string]any)
	if !ok || schema["type"] != "object" {
		t.Fatalf("schema = %#v", decl)
	}
}

func TestGeminiNativeModelEntries(t *testing.T) {
	entries := geminiNativeModelEntries([]api.Model{
		{ID: "gemini-3.7-flash-high"},
		{ID: "models/gemini-3.7-flash-high"},
		{ID: "gemini-3.8-flash-low"},
	})
	if len(entries) != 2 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0]["name"] != "models/gemini-3.7-flash-high" {
		t.Fatalf("name = %#v", entries[0]["name"])
	}
	methods, _ := entries[0]["supportedGenerationMethods"].([]string)
	if len(methods) != 3 || methods[0] != "generateContent" {
		t.Fatalf("methods = %#v", methods)
	}
}

func TestAntigravityCollectFunctionResponsesWithInlineData(t *testing.T) {
	content := map[string]any{
		"parts": []any{
			map[string]any{"inlineData": map[string]any{"mimeType": "image/png", "data": "abc"}},
			map[string]any{"functionResponse": map[string]any{"name": "lookup", "response": map[string]any{"result": "ok"}}},
		},
	}
	responses := antigravityCollectFunctionResponsesWithInlineData(content)
	if len(responses) != 1 {
		t.Fatalf("responses len = %d", len(responses))
	}
	fr, ok := responses[0]["functionResponse"].(map[string]any)
	if !ok {
		t.Fatal("missing functionResponse")
	}
	parts, ok := fr["parts"].([]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("inlineData not attached: %#v", fr)
	}
}
