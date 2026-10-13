package proxy

import (
	"encoding/json"
	"strings"
)

func antigravityFlattenSchemaUnions(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		if allOf, ok := typed["allOf"].([]any); ok {
			for _, branch := range allOf {
				branchMap, ok := antigravityFlattenSchemaUnions(branch).(map[string]any)
				if !ok {
					continue
				}
				antigravityMergeSchemaObjects(typed, branchMap)
			}
			delete(typed, "allOf")
		}
		for _, unionKey := range []string{"anyOf", "oneOf"} {
			branches, ok := typed[unionKey].([]any)
			if !ok || len(branches) == 0 {
				continue
			}
			hasNull := false
			best := map[string]any(nil)
			bestScore := -1
			for _, branch := range branches {
				branchMap, ok := antigravityFlattenSchemaUnions(branch).(map[string]any)
				if !ok {
					continue
				}
				if branchType, _ := branchMap["type"].(string); strings.EqualFold(branchType, "null") {
					hasNull = true
					continue
				}
				score := antigravitySchemaBranchScore(branchMap)
				if score > bestScore {
					bestScore = score
					best = branchMap
				}
			}
			if parentProps, ok := typed["properties"].(map[string]any); ok && len(parentProps) > 0 {
				for _, branch := range branches {
					branchMap, ok := antigravityFlattenSchemaUnions(branch).(map[string]any)
					if !ok {
						continue
					}
					if branchType, _ := branchMap["type"].(string); strings.EqualFold(branchType, "null") {
						hasNull = true
					}
					antigravityMergeSchemaObjects(typed, branchMap)
				}
			} else if best != nil {
				antigravityMergeSchemaObjects(typed, best)
			}
			if hasNull {
				typed["nullable"] = true
			}
			delete(typed, unionKey)
		}
		for key, item := range typed {
			typed[key] = antigravityFlattenSchemaUnions(item)
		}
		return typed
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, antigravityFlattenSchemaUnions(item))
		}
		return out
	default:
		return value
	}
}

func antigravitySchemaBranchScore(schema map[string]any) int {
	if props, ok := schema["properties"].(map[string]any); ok && len(props) > 0 {
		return 3
	}
	if schemaType, _ := schema["type"].(string); strings.EqualFold(schemaType, "object") {
		return 3
	}
	if _, ok := schema["items"]; ok {
		return 2
	}
	if schemaType, _ := schema["type"].(string); schemaType != "" && !strings.EqualFold(schemaType, "null") {
		return 1
	}
	return 0
}

func antigravityMergeSchemaObjects(dst, src map[string]any) {
	if dst == nil || src == nil {
		return
	}
	if srcProps, ok := src["properties"].(map[string]any); ok {
		dstProps, _ := dst["properties"].(map[string]any)
		if dstProps == nil {
			dstProps = map[string]any{}
			dst["properties"] = dstProps
		}
		for key, value := range srcProps {
			if _, exists := dstProps[key]; !exists {
				dstProps[key] = antigravityFlattenSchemaUnions(value)
			}
		}
	}
	if required, ok := src["required"].([]any); ok {
		dstRequired, _ := dst["required"].([]any)
		seen := map[string]struct{}{}
		for _, item := range dstRequired {
			if name, ok := item.(string); ok {
				seen[name] = struct{}{}
			}
		}
		for _, item := range required {
			name, ok := item.(string)
			if !ok || name == "" {
				continue
			}
			if _, exists := seen[name]; exists {
				continue
			}
			dstRequired = append(dstRequired, name)
			seen[name] = struct{}{}
		}
		if len(dstRequired) > 0 {
			dst["required"] = dstRequired
		}
	}
	if srcType, ok := src["type"].(string); ok {
		if _, exists := dst["type"]; !exists {
			dst["type"] = strings.ToLower(strings.TrimSpace(srcType))
		}
	}
}

// antigravityApplyGeminiDeclarationSchema normalizes tool parameter schemas for
// Cloud Code v1internal, which expects parametersJsonSchema instead of parameters.
func antigravityApplyGeminiDeclarationSchema(declaration map[string]any) {
	if declaration == nil {
		return
	}
	raw := declaration["parameters"]
	if raw == nil {
		raw = declaration["parametersJsonSchema"]
	}
	if raw == nil {
		raw = declaration["parameters_json_schema"]
	}
	schema := antigravityGeminiParameters(raw)
	delete(declaration, "parameters")
	delete(declaration, "parameters_json_schema")
	declaration["parametersJsonSchema"] = schema
}

func sanitizeAntigravityEnvelopeToolSchemas(payload []byte) []byte {
	var envelope map[string]any
	if json.Unmarshal(payload, &envelope) != nil {
		return payload
	}
	request, ok := envelope["request"].(map[string]any)
	if !ok {
		return payload
	}
	tools, ok := request["tools"].([]any)
	if !ok || len(tools) == 0 {
		return payload
	}
	for toolIndex, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"functionDeclarations", "function_declarations"} {
			declarations, ok := tool[key].([]any)
			if !ok {
				continue
			}
			for declIndex, rawDecl := range declarations {
				decl, ok := rawDecl.(map[string]any)
				if !ok {
					continue
				}
				antigravityApplyGeminiDeclarationSchema(decl)
				declarations[declIndex] = decl
			}
			tool[key] = declarations
		}
		tools[toolIndex] = tool
	}
	request["tools"] = tools
	envelope["request"] = request
	out, err := json.Marshal(envelope)
	if err != nil {
		return payload
	}
	return out
}
