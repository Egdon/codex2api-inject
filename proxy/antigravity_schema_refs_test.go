package proxy

import (
	"testing"
)

func TestAntigravityInlineLocalSchemaRefsExpandsDefs(t *testing.T) {
	root := map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"query": map[string]any{"type": "string"},
		},
		"properties": map[string]any{
			"query": map[string]any{"$ref": "#/$defs/query"},
		},
	}
	got := antigravityInlineLocalSchemaRefs(root)
	props, _ := got["properties"].(map[string]any)
	query, _ := props["query"].(map[string]any)
	if query["type"] != "string" {
		t.Fatalf("query = %#v", query)
	}
	if _, ok := query["$ref"]; ok {
		t.Fatalf("$ref must be inlined: %#v", query)
	}
}

func TestAntigravityInlineLocalSchemaRefsExpandsDefinitionsPrefix(t *testing.T) {
	root := map[string]any{
		"type": "object",
		"definitions": map[string]any{
			"A": map[string]any{"type": "integer"},
		},
		"properties": map[string]any{
			"a": map[string]any{"$ref": "#/definitions/A"},
		},
	}
	got := antigravityInlineLocalSchemaRefs(root)
	props, _ := got["properties"].(map[string]any)
	a, _ := props["a"].(map[string]any)
	if a["type"] != "integer" {
		t.Fatalf("a = %#v", a)
	}
}

func TestAntigravityInlineLocalSchemaRefsSiblingOverridesRef(t *testing.T) {
	root := map[string]any{
		"$defs": map[string]any{
			"mode": map[string]any{"type": "string", "description": "base"},
		},
		"properties": map[string]any{
			"mode": map[string]any{
				"$ref":        "#/$defs/mode",
				"description": "override",
			},
		},
	}
	got := antigravityInlineLocalSchemaRefs(root)
	mode, _ := got["properties"].(map[string]any)["mode"].(map[string]any)
	if mode["description"] != "override" || mode["type"] != "string" {
		t.Fatalf("mode = %#v", mode)
	}
}

func TestAntigravityInlineLocalSchemaRefsExpandsNestedPointer(t *testing.T) {
	root := map[string]any{
		"$defs": map[string]any{
			"Task": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
			},
		},
		"properties": map[string]any{
			"name": map[string]any{"$ref": "#/$defs/Task/properties/name"},
		},
	}
	got := antigravityInlineLocalSchemaRefs(root)
	name, _ := got["properties"].(map[string]any)["name"].(map[string]any)
	if name["type"] != "string" {
		t.Fatalf("name = %#v", name)
	}
}

func TestAntigravityInlineLocalSchemaRefsExpandsAnyOfRefBranch(t *testing.T) {
	root := map[string]any{
		"$defs": map[string]any{
			"Task": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name": map[string]any{"type": "string"},
				},
				"required": []any{"name"},
			},
		},
		"anyOf": []any{
			map[string]any{"$ref": "#/$defs/Task"},
			map[string]any{"type": "null"},
		},
	}
	inline := antigravityInlineLocalSchemaRefs(root)
	got, ok := antigravityFlattenSchemaUnions(inline).(map[string]any)
	if !ok {
		t.Fatal("expected object schema")
	}
	if _, ok := got["anyOf"]; ok {
		t.Fatalf("anyOf must flatten after ref inline: %#v", got)
	}
	props, _ := got["properties"].(map[string]any)
	if props["name"] == nil {
		t.Fatalf("Task properties missing: %#v", got)
	}
}

func TestAntigravityInlineLocalSchemaRefsHandlesCycles(t *testing.T) {
	root := map[string]any{
		"$defs": map[string]any{
			"A": map[string]any{"$ref": "#/$defs/B", "type": "object"},
			"B": map[string]any{"$ref": "#/$defs/A", "type": "object"},
		},
		"properties": map[string]any{
			"node": map[string]any{"$ref": "#/$defs/A"},
		},
	}
	got := antigravityInlineLocalSchemaRefs(root)
	node, _ := got["properties"].(map[string]any)["node"].(map[string]any)
	if _, ok := node["$ref"]; ok {
		t.Fatalf("cycle must not leak $ref: %#v", node)
	}
	if node["type"] != "object" {
		t.Fatalf("node = %#v", node)
	}
	if desc, _ := node["description"].(string); desc == "" {
		t.Fatalf("cycle should retain hint description: %#v", node)
	}
}

func TestAntigravityGeminiParametersInlinesRefBeforeCleanup(t *testing.T) {
	got := antigravityGeminiParameters(map[string]any{
		"type": "object",
		"$defs": map[string]any{
			"query": map[string]any{"type": "string", "format": "uri"},
		},
		"properties": map[string]any{
			"query": map[string]any{"$ref": "#/$defs/query"},
		},
		"required": []any{"query"},
	})
	query, _ := got["properties"].(map[string]any)["query"].(map[string]any)
	if query["type"] != "string" {
		t.Fatalf("query = %#v", query)
	}
	if antigravitySchemaHasRef(got) {
		t.Fatalf("final schema still contains $ref: %#v", got)
	}
}
