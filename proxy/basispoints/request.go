// Package basispoints adapts Responses clients to the ChatGPT Excel gateway.
package basispoints

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

const ResponsesURL = "https://bps.openai.com/basispoints/api/responses"

type object = map[string]any

type Bridge struct {
	RequestedEffort  string
	Effort           string
	Warnings         []string
	tools            map[string]tool
	unsupportedTools map[string]bool
	replay           *ReplayCache
	scope            string
}

func decode(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected one JSON value")
	}
	return nil
}

func text(value any) string {
	s, _ := value.(string)
	return s
}

// NormalizeEffort caps unsupported high tiers explicitly instead of falling back to medium.
func NormalizeEffort(effort string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "medium":
		return "medium", nil
	case "low", "high":
		return strings.ToLower(strings.TrimSpace(effort)), nil
	case "xhigh", "x-high", "extra-high", "extra_high", "max", "ultra":
		return "xhigh", nil
	case "none", "minimal":
		return "low", nil
	default:
		return "", fmt.Errorf("basispoints reasoning effort %q is unsupported", effort)
	}
}

func fingerprint(value any) string {
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}

func message(role, content string) object {
	return object{"type": "message", "role": role, "content": []any{object{"type": "input_text", "text": content}}}
}

// describeOutputFormat turns Responses text.format into a prompt contract
// because the Excel wire body has no structured output field.
func describeOutputFormat(format object) (string, error) {
	switch kind := text(format["type"]); kind {
	case "", "text":
		return "", nil
	case "json_object":
		return "Final answer format: return exactly one valid JSON object and nothing else. " +
			"Do not wrap it in Markdown code fences or add text before or after it.", nil
	case "json_schema":
		schema, ok := format["schema"].(object)
		if !ok {
			return "", fmt.Errorf("basispoints json_schema format requires a schema object")
		}
		raw, err := json.Marshal(schema)
		if err != nil {
			return "", fmt.Errorf("basispoints json_schema format has an invalid schema")
		}
		contract := "Final answer format: return exactly one valid JSON value that conforms to the JSON Schema below and nothing else. " +
			"Do not wrap it in Markdown code fences or add text before or after it. Include every required property and no properties the schema does not allow."
		if name := text(format["name"]); name != "" {
			contract += "\nSchema name: " + name
		}
		if description := text(format["description"]); description != "" {
			contract += "\nSchema description: " + description
		}
		return contract + "\nJSON Schema:\n" + string(raw), nil
	default:
		return "", fmt.Errorf("basispoints does not support text format %q", kind)
	}
}

// Prepare preserves the requested model and uses a whitelist for the Excel wire body.
func Prepare(raw []byte, scope string, replay *ReplayCache) ([]byte, *Bridge, error) {
	var source object
	if err := decode(raw, &source); err != nil || source == nil {
		return nil, nil, fmt.Errorf("invalid Basispoints request JSON")
	}
	model := strings.TrimSpace(text(source["model"]))
	if model == "" {
		return nil, nil, fmt.Errorf("basispoints requires a model")
	}
	if text(source["previous_response_id"]) != "" {
		return nil, nil, fmt.Errorf("basispoints requires expanded history instead of previous_response_id")
	}
	requested := text(source["reasoning_effort"])
	if reasoning, ok := source["reasoning"].(object); ok {
		requested = text(reasoning["effort"])
		if mode := text(reasoning["mode"]); mode != "" && mode != "standard" {
			return nil, nil, fmt.Errorf("basispoints does not support reasoning mode %q", mode)
		}
	}
	effort, err := NormalizeEffort(requested)
	if err != nil {
		return nil, nil, err
	}
	b := &Bridge{RequestedEffort: requested, Effort: effort, tools: make(map[string]tool), unsupportedTools: make(map[string]bool), replay: replay, scope: scope}
	choice := source["tool_choice"]
	if choice != nil && text(choice) != "auto" && text(choice) != "none" {
		return nil, nil, fmt.Errorf("basispoints supports tool_choice auto or none only")
	}
	var catalog []any
	if text(choice) != "none" {
		catalog, err = b.collectTools(source["tools"], "")
		if err != nil {
			return nil, nil, err
		}
		if input, ok := source["input"].([]any); ok {
			for _, raw := range input {
				item, _ := raw.(object)
				if text(item["type"]) == "additional_tools" {
					additional, err := b.collectTools(item["tools"], "")
					if err != nil {
						return nil, nil, err
					}
					catalog = append(catalog, additional...)
				}
			}
		}
	}
	var outputContract string
	if config, ok := source["text"].(object); ok {
		if f, ok := config["format"].(object); ok {
			outputContract, err = describeOutputFormat(f)
			if err != nil {
				return nil, nil, err
			}
		}
	}
	var input []any
	switch v := source["input"].(type) {
	case string:
		input = []any{message("user", v)}
	case []any:
		input = v
	default:
		return nil, nil, fmt.Errorf("basispoints input must be text or a Responses item array")
	}
	translated, err := b.translateHistory(input)
	if err != nil {
		return nil, nil, err
	}
	prologue := make([]any, 0, 2)
	if instructions := text(source["instructions"]); instructions != "" {
		prologue = append(prologue, message("developer", instructions))
	}
	protocol := "This request comes from an external Responses client. Return assistant text. Do not call Excel, Office, workbook or connector tools."
	if len(catalog) > 0 {
		protocol = "This request comes from an external Responses client. Use only the client tools in the catalog below. " +
			"There is no live workbook or Office runtime for this request. The gateway relays declared client tools and never executes their code. " +
			"For a function tool, call native run_officejs with code containing one JSON object {\"name\":\"CATALOG_NAME\",\"arguments\":{...}}. " +
			"For a custom tool, set summary to codex2api.custom/CATALOG_NAME and put the exact raw input directly in code. " +
			"Call one client tool at a time and continue after its result. Never call an undeclared native tool or invent a tool result. " +
			"Client tool catalog:\n" + describeCatalog(catalog) +
			"\nEnd of catalog. The gateway handles run_officejs transport and does not execute Office code."
	}
	if outputContract != "" {
		protocol += "\n" + outputContract
	}

	if len(b.unsupportedTools) > 0 {
		kinds := make([]string, 0, len(b.unsupportedTools))
		for kind := range b.unsupportedTools {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		warning := "Hosted tools unavailable through Basispoints: " + strings.Join(kinds, ", ")
		b.Warnings = append(b.Warnings, warning)
		protocol += "\n" + warning + ". These declarations were omitted. Do not claim to have used them. If the task requires one, explain the limitation or use a suitable declared client tool."
	}
	prologue = append(prologue, message("developer", protocol))
	cacheKey := text(source["prompt_cache_key"])
	conversation := cacheKey
	if conversation == "" && len(input) > 0 {
		conversation = fingerprint(input[0])
	}
	turnEnd := 0
	if len(input) > 0 {
		turnEnd = 1
	}
	iteration := 1
	for i := len(input) - 1; i >= 0; i-- {
		item, _ := input[i].(object)
		if text(item["role"]) == "user" {
			turnEnd = i + 1
			break
		}
		if strings.HasSuffix(text(item["type"]), "_call_output") {
			iteration++
		}
	}
	output := object{
		"model": model, "model_selection": "explicit", "stream": true, "store": false,
		"input": append(prologue, translated...), "reasoning_effort": effort,
		"context_management": []any{object{"type": "compaction", "compact_threshold": 200000}},
		"metadata": object{
			"task_id": fingerprint([]any{scope, conversation}),
			"turn_id": fingerprint([]any{scope, input[:turnEnd]}), "agent_iteration": fmt.Sprint(iteration),
		},
	}
	if cacheKey != "" {
		output["prompt_cache_key"] = "bps-" + fingerprint([]any{scope, cacheKey})
	}
	if management, ok := source["context_management"].([]any); ok {
		output["context_management"] = management
	}
	body, err := json.Marshal(output)
	return body, b, err
}
