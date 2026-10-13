package proxy

import (
	"fmt"
	"sort"
	"strings"
)

const antigravityToolSchemaReasonDescription = "Brief explanation of why you are calling this tool"

var antigravityKnownSchemaKeywords = map[string]struct{}{
	"properties": {}, "patternProperties": {}, "additionalProperties": {}, "items": {}, "prefixItems": {},
	"$defs": {}, "definitions": {}, "dependentSchemas": {}, "dependentRequired": {}, "dependencies": {},
	"if": {}, "then": {}, "else": {}, "not": {}, "contains": {}, "propertyNames": {},
	"unevaluatedProperties": {}, "unevaluatedItems": {}, "contentSchema": {}, "additionalItems": {},
	"default": {}, "const": {}, "example": {}, "examples": {}, "discriminator": {}, "xml": {},
	"enumDescriptions": {}, "enumTitles": {},
}

// antigravityNormalizeMalformedToolSchema repairs MCP-style schemas that omit object
// wrappers, promote boolean required flags, and add missing array item schemas.
func antigravityNormalizeMalformedToolSchema(root map[string]any) map[string]any {
	if root == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	repaired, _ := antigravityRepairToolSchemaNode(root, true)
	if repaired == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return repaired
}

func antigravityFinalizeToolSchema(schema map[string]any) map[string]any {
	if schema == nil {
		schema = map[string]any{}
	}
	antigravityDropToolSchemaEnums(schema)
	antigravityEnsureArrayItemSchemas(schema)
	antigravityEnsureEmptyToolSchemaPlaceholder(schema)
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
	if _, ok := schema["properties"]; !ok {
		schema["properties"] = map[string]any{}
	}
	return schema
}

func antigravityDropToolSchemaEnums(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if enum, ok := typed["enum"].([]any); ok && len(enum) > 0 {
			hint := "Allowed: " + antigravityEnumValuesText(enum)
			typed["description"] = antigravityMergeSchemaHint(asString(typed["description"]), hint)
			delete(typed, "enum")
		}
		for key, item := range typed {
			if key == "enum" {
				continue
			}
			antigravityDropToolSchemaEnums(item)
		}
	case []any:
		for _, item := range typed {
			antigravityDropToolSchemaEnums(item)
		}
	}
}

func antigravityEnsureArrayItemSchemas(value any) {
	switch typed := value.(type) {
	case map[string]any:
		if antigravitySchemaTypeIsArray(typed["type"]) {
			if _, ok := typed["items"]; !ok {
				typed["items"] = map[string]any{"type": "string"}
			}
		}
		for _, item := range typed {
			antigravityEnsureArrayItemSchemas(item)
		}
	case []any:
		for _, item := range typed {
			antigravityEnsureArrayItemSchemas(item)
		}
	}
}

func antigravityEnsureEmptyToolSchemaPlaceholder(schema map[string]any) {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	}
	if len(props) > 0 {
		return
	}
	if schemaType, _ := schema["type"].(string); schemaType != "" && !antigravitySchemaTypeIsObject(schema["type"]) {
		return
	}
	props["reason"] = map[string]any{
		"type":        "string",
		"description": antigravityToolSchemaReasonDescription,
	}
	schema["properties"] = props
	schema["required"] = []any{"reason"}
	if _, ok := schema["type"]; !ok {
		schema["type"] = "object"
	}
}

func antigravityRepairToolSchemaNode(node map[string]any, addMissingArrayItems bool) (map[string]any, bool) {
	if node == nil {
		return nil, false
	}
	modified := false
	clone := make(map[string]any, len(node))
	for key, value := range node {
		clone[key] = value
	}

	if !antigravitySchemaTypeIsNonObject(clone["type"]) {
		bareProps := map[string]any{}
		for key, value := range clone {
			childMap, isMap := value.(map[string]any)
			if !isMap || antigravityIsKnownSchemaKeyword(key) || strings.HasPrefix(key, "x-") {
				continue
			}
			bareProps[key] = childMap
		}
		if len(bareProps) > 0 {
			repairedProps, promotedReqs, _ := antigravityRepairToolPropertyMap(bareProps, addMissingArrayItems)
			for key := range bareProps {
				delete(clone, key)
			}
			existingProps, _ := clone["properties"].(map[string]any)
			if existingProps == nil {
				existingProps = map[string]any{}
			}
			for key, value := range repairedProps {
				existingProps[key] = value
			}
			clone["properties"] = existingProps
			if _, hasType := clone["type"]; !hasType {
				clone["type"] = "object"
			}
			if len(promotedReqs) > 0 {
				clone["required"] = antigravityMergeRequired(antigravityStringSlice(clone["required"]), promotedReqs)
			}
			modified = true
		}
	}

	if props, ok := clone["properties"].(map[string]any); ok {
		repairedProps, promotedReqs, propsMod := antigravityRepairToolPropertyMap(props, addMissingArrayItems)
		if propsMod {
			clone["properties"] = repairedProps
			modified = true
		}
		if len(promotedReqs) > 0 {
			clone["required"] = antigravityMergeRequired(antigravityStringSlice(clone["required"]), promotedReqs)
			modified = true
		}
	}

	if addMissingArrayItems && antigravitySchemaTypeIsArray(clone["type"]) {
		if _, ok := clone["items"]; !ok {
			clone["items"] = map[string]any{"type": "string"}
			modified = true
		}
	}

	for _, key := range []string{"items", "additionalProperties", "if", "then", "else", "not", "contains", "propertyNames"} {
		if sub, ok := clone[key].(map[string]any); ok {
			repaired, subMod := antigravityRepairToolSchemaNode(sub, addMissingArrayItems)
			if subMod {
				clone[key] = repaired
				modified = true
			}
		}
	}
	for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
		if list, ok := clone[key].([]any); ok {
			repaired, listMod := antigravityRepairToolSchemaList(list, addMissingArrayItems)
			if listMod {
				clone[key] = repaired
				modified = true
			}
		}
	}
	for _, key := range []string{"$defs", "definitions", "dependentSchemas"} {
		if defs, ok := clone[key].(map[string]any); ok {
			repairedDefs := map[string]any{}
			defsModified := false
			for defKey, defValue := range defs {
				if defMap, ok := defValue.(map[string]any); ok {
					repairedDef, defMod := antigravityRepairToolSchemaNode(defMap, addMissingArrayItems)
					repairedDefs[defKey] = repairedDef
					if defMod {
						defsModified = true
						modified = true
					}
				} else {
					repairedDefs[defKey] = defValue
				}
			}
			if defsModified {
				clone[key] = repairedDefs
			}
		}
	}
	return clone, modified
}

func antigravityRepairToolSchemaList(list []any, addMissingArrayItems bool) ([]any, bool) {
	out := make([]any, 0, len(list))
	modified := false
	for _, item := range list {
		if itemMap, ok := item.(map[string]any); ok {
			repaired, itemMod := antigravityRepairToolSchemaNode(itemMap, addMissingArrayItems)
			out = append(out, repaired)
			if itemMod {
				modified = true
			}
			continue
		}
		out = append(out, item)
	}
	return out, modified
}

func antigravityRepairToolPropertyMap(props map[string]any, addMissingArrayItems bool) (map[string]any, []string, bool) {
	out := make(map[string]any, len(props))
	var promotedReqs []string
	modified := false
	for key, value := range props {
		childMap, isMap := value.(map[string]any)
		if !isMap {
			out[key] = value
			continue
		}
		childClone := make(map[string]any, len(childMap))
		for ck, cv := range childMap {
			childClone[ck] = cv
		}
		if reqBool, isBool := childClone["required"].(bool); isBool {
			delete(childClone, "required")
			modified = true
			if reqBool {
				promotedReqs = append(promotedReqs, key)
			}
		}
		repairedChild, childMod := antigravityRepairToolSchemaNode(childClone, addMissingArrayItems)
		if childMod {
			modified = true
		}
		out[key] = repairedChild
	}
	sort.Strings(promotedReqs)
	return out, promotedReqs, modified
}

func antigravityIsKnownSchemaKeyword(key string) bool {
	if strings.HasPrefix(key, "x-") {
		return true
	}
	_, ok := antigravityKnownSchemaKeywords[key]
	return ok
}

func antigravitySchemaTypeIsObject(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "object") || strings.EqualFold(strings.TrimSpace(typed), "OBJECT")
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.EqualFold(strings.TrimSpace(s), "object") {
				return true
			}
		}
	}
	return false
}

func antigravitySchemaTypeIsArray(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(typed), "array") || strings.EqualFold(strings.TrimSpace(typed), "ARRAY")
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.EqualFold(strings.TrimSpace(s), "array") {
				return true
			}
		}
	}
	return false
}

func antigravitySchemaTypeIsNonObject(value any) bool {
	if antigravitySchemaTypeIsObject(value) {
		return false
	}
	switch typed := value.(type) {
	case string:
		t := strings.ToLower(strings.TrimSpace(typed))
		return t != "" && t != "object"
	case []any:
		for _, item := range typed {
			if s, ok := item.(string); ok {
				t := strings.ToLower(strings.TrimSpace(s))
				if t == "object" {
					return false
				}
				if t != "" {
					return true
				}
			}
		}
	}
	return false
}

func antigravityStringSlice(value any) []string {
	switch typed := value.(type) {
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return typed
	default:
		return nil
	}
}

func antigravityMergeRequired(existing, promoted []string) []any {
	seen := map[string]struct{}{}
	out := make([]any, 0, len(existing)+len(promoted))
	for _, name := range append(existing, promoted...) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func antigravityEnumValuesText(values []any) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprint(value))
	}
	return strings.Join(parts, ", ")
}

func antigravityMergeSchemaHint(existing, hint string) string {
	existing = strings.TrimSpace(existing)
	if existing == "" {
		return hint
	}
	if existing == hint || strings.Contains(existing, hint) {
		return existing
	}
	return existing + " (" + hint + ")"
}

func asString(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}
