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
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const ResponsesURL = "https://bps.openai.com/basispoints/api/responses"

type object = map[string]any

type Bridge struct {
	RequestedEffort string
	Effort          string
	Warnings        []string
	// Parallel mirrors the client's parallel_tool_calls (default true). When
	// false, only the first client tool call of a response is relayed.
	Parallel         bool
	tools            map[string]tool
	unsupportedTools map[string]bool
	replay           *ReplayCache
	scope            string
	// skippedReferences counts item_reference inputs dropped from history.
	skippedReferences int
	// builtins lists declared built-in client tools (shell, apply_patch, ...)
	// in declaration order, registered after the client's own tools.
	builtins []string
	// synthesized is set when a cut-off stream was completed locally.
	synthesized atomic.Bool
	// keepalive overrides the idle interval between response.in_progress
	// frames; zero uses defaultKeepalive.
	keepalive time.Duration
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
		// reasoning.mode has no Excel wire field, so it is dropped like the
		// add-in does rather than failing the request.
		requested = text(reasoning["effort"])
	}
	effort, err := NormalizeEffort(requested)
	if err != nil {
		// Unknown or stale picker values fall back to medium instead of a 4xx.
		effort = "medium"
	}
	parallel, explicit := source["parallel_tool_calls"].(bool)
	b := &Bridge{RequestedEffort: requested, Effort: effort, Parallel: parallel || !explicit, tools: make(map[string]tool), unsupportedTools: make(map[string]bool), replay: replay, scope: scope}
	choice := parseToolChoice(source["tool_choice"])
	var catalog []any
	if !choice.none {
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
	catalog = b.registerBuiltins(catalog)
	catalog = b.restrictCatalog(catalog, choice.allowed)
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
	for _, raw := range input {
		// Codex restamps this private metadata every turn; it must not reach
		// the wire or change task and turn identity.
		if item, ok := raw.(object); ok {
			delete(item, "internal_chat_message_metadata_passthrough")
		}
	}
	// Identity comes from the history as the client sent it, before tool
	// results and images are rewritten, so retries keep the same turn_id.
	cacheKey := text(source["prompt_cache_key"])
	conversation := cacheKey
	if conversation == "" && len(input) > 0 {
		conversation = fingerprint(input[0])
	}
	turnEnd, iteration := agentTurnState(input)
	taskID := fingerprint([]any{scope, conversation})
	turnID := fingerprint([]any{scope, input[:turnEnd]})
	replay.Prefetch(scope, historyCallIDs(input))
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
			"The code field is JSON text, not JavaScript: build the complete inner object first, then place it in code, " +
			"escaping every double quote and backslash inside its string values exactly as JSON requires. " +
			"Unescaped quotes in shell commands are the most common transport failure. " +
			"When the catalog lists a raw transport for a tool, prefer it for commands or other text containing quotes or backslashes: " +
			"set summary to codex2api.raw/CATALOG_NAME/FIELD and put the exact raw field value in code, with no JSON around it. " +
			"For a custom tool, set summary to codex2api.custom/CATALOG_NAME and put the exact raw input directly in code. " +
			parallelGuidance(b.Parallel) + "Never call an undeclared native tool or invent a tool result. " +
			"Client tool catalog:\n" + describeCatalog(catalog) +
			"\nEnd of catalog. The gateway handles run_officejs transport and does not execute Office code.\n" +
			transportReminder
		if requirement := choice.requirement(b.tools); requirement != "" {
			protocol += "\n" + requirement
		}
	}
	if outputContract != "" {
		protocol += "\n" + outputContract
	}

	if b.skippedReferences > 0 {
		warning := fmt.Sprintf("%d history item references were omitted", b.skippedReferences)
		b.Warnings = append(b.Warnings, warning)
		protocol += "\nSome earlier conversation items were sent as references to stored responses, which this gateway cannot resolve, " +
			"so they are missing from the history. Do not assume their content; ask the user if it matters."
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
	metadata := clientMetadata(source["metadata"])
	metadata["task_id"], metadata["turn_id"], metadata["agent_iteration"] = taskID, turnID, fmt.Sprint(iteration)
	output := object{
		"model": model, "model_selection": "explicit", "stream": true, "store": false,
		"input": append(prologue, translated...), "reasoning_effort": effort,
		"context_management": []any{object{"type": "compaction", "compact_threshold": 200000}},
		"metadata":           metadata,
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

// agentTurnState returns the end of the turn prefix (through the last user
// message) and the Excel agent iteration. The add-in keeps turn_id fixed while
// it runs tools for one user message and advances agent_iteration once per
// round of tool results; parallel results arrive together as one round.
func agentTurnState(input []any) (int, int) {
	lastUser := -1
	for i, raw := range input {
		if item, _ := raw.(object); text(item["role"]) == "user" {
			lastUser = i
		}
	}
	turnEnd := lastUser + 1
	if lastUser < 0 {
		turnEnd = min(len(input), 1)
	}
	iteration := 1
	inResults := false
	for _, raw := range input[lastUser+1:] {
		item, _ := raw.(object)
		result := strings.HasSuffix(text(item["type"]), "_call_output")
		if result && !inResults {
			iteration++
		}
		inResults = result
	}
	return turnEnd, iteration
}

// transportReminder restates the transport rules after the catalog, which can
// be long enough to push the opening instructions out of focus. It is fixed
// text inside the cached prefix, so it costs nothing per turn.
const transportReminder = "Transport rules: one catalog tool per run_officejs call; never nest another run_officejs inside code; " +
	"code is JSON text, never JavaScript or OfficeJS; escape quotes and backslashes inside JSON string values; " +
	"use a tool's raw transport (codex2api.raw/CATALOG_NAME/FIELD) when its value contains quotes or backslashes."

func parallelGuidance(parallel bool) string {
	if parallel {
		return "When several client tool calls do not depend on each other, make them as separate run_officejs calls in the same response; " +
			"wait for a result only when the next call needs it. "
	}
	return "Call one client tool at a time and continue after its result. "
}

// SynthesizedCompletion reports whether the stream's response.completed was
// rebuilt locally after the upstream closed early. Such a response carries no
// usage, so callers should record it rather than bill it as zero silently.
func (b *Bridge) SynthesizedCompletion() bool {
	return b != nil && b.synthesized.Load()
}

// historyCallIDs lists the call IDs whose native items translation will look up.
func historyCallIDs(input []any) []string {
	var ids []string
	for _, raw := range input {
		item, _ := raw.(object)
		switch text(item["type"]) {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output",
			"local_shell_call", "shell_call", "apply_patch_call",
			"local_shell_call_output", "shell_call_output", "apply_patch_call_output":
			ids = append(ids, text(item["call_id"]))
		}
	}
	return ids
}

// restrictCatalog applies tool_choice allowed_tools: tools outside the allowed
// set are removed from both the prompt catalog and the relay whitelist.
func (b *Bridge) restrictCatalog(catalog []any, allowed map[string]bool) []any {
	if allowed == nil {
		return catalog
	}
	kept := catalog[:0]
	for _, raw := range catalog {
		entry, _ := raw.(object)
		key := text(entry["name"])
		if allowed[key] || allowed[b.tools[key].Name] {
			kept = append(kept, raw)
			continue
		}
		delete(b.tools, key)
	}
	return kept
}

// toolChoice is the client's tool_choice. The Excel wire has no such field, so
// forced selections degrade to auto plus a prompt requirement, and an
// allowed_tools subset narrows the catalog.
type toolChoice struct {
	none     bool
	required bool
	name     string
	allowed  map[string]bool
}

func parseToolChoice(value any) toolChoice {
	switch v := value.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "none":
			return toolChoice{none: true}
		case "required":
			return toolChoice{required: true}
		}
	case object:
		if kind := text(v["type"]); builtinClientTools[kind].callType != "" {
			return toolChoice{required: true, name: kind}
		}
		switch text(v["type"]) {
		case "function", "custom":
			name := text(v["name"])
			if function, ok := v["function"].(object); ok && name == "" {
				name = text(function["name"])
			}
			return toolChoice{required: true, name: name}
		case "allowed_tools":
			allowed := make(map[string]bool)
			tools, _ := v["tools"].([]any)
			for _, raw := range tools {
				entry, _ := raw.(object)
				if kind := text(entry["type"]); builtinClientTools[kind].callType != "" {
					allowed[kind] = true
				}
				if name := text(entry["name"]); name != "" {
					// A namespaced entry allows only that qualified tool; a bare
					// name also matches tools declared inside a namespace.
					if namespace := text(entry["namespace"]); namespace != "" {
						allowed[namespace+"."+name] = true
					} else {
						allowed[name] = true
					}
				}
			}
			return toolChoice{required: text(v["mode"]) == "required", allowed: allowed}
		}
	}
	return toolChoice{}
}

func (c toolChoice) requirement(tools map[string]tool) string {
	if c.name != "" {
		match := ""
		if _, ok := tools[c.name]; ok {
			match = c.name
		} else {
			// Sorted so the prompt stays byte-identical for the upstream cache.
			keys := make([]string, 0, len(tools))
			for key := range tools {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if tools[key].Name == c.name {
					match = key
					break
				}
			}
		}
		if match != "" {
			return "The client requires this response to call client tool " + quoted(match) + " through run_officejs."
		}
	}
	if c.required {
		return "The client requires this response to call at least one client tool through run_officejs."
	}
	return ""
}

// clientMetadata keeps scalar client metadata within the Responses limits
// (16 keys, 64-character keys, 512-character values), leaving room for the
// three gateway identity keys, which are set afterwards and always win.
func clientMetadata(value any) object {
	metadata := make(object)
	source, _ := value.(object)
	keys := make([]string, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if len(metadata) >= 13 {
			break
		}
		var rendered string
		switch v := source[key].(type) {
		case string:
			rendered = v
		case json.Number, bool:
			rendered = fmt.Sprint(v)
		default:
			continue
		}
		if key = truncateRunes(key, 64); key != "" {
			metadata[key] = truncateRunes(rendered, 512)
		}
	}
	return metadata
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	return string([]rune(value)[:limit])
}
