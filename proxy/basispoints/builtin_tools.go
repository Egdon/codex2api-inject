package basispoints

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// builtinTool describes a Responses built-in tool that the client executes:
// the model only generates the call, so BPS can relay it through run_officejs
// like a function tool, and the client receives its native call item.
type builtinTool struct {
	callType    string
	idPrefix    string
	description string
	parameters  object
}

func stringArraySchema(description string) object {
	return object{"type": "array", "items": object{"type": "string"}, "minItems": 1, "description": description}
}

var builtinClientTools = map[string]builtinTool{
	"local_shell": {
		callType:    "local_shell_call",
		idPrefix:    "lsh_",
		description: "Run one program on the client machine (built-in local_shell tool).",
		parameters: object{
			"type": "object",
			"properties": object{
				"command":           stringArraySchema(`Program and arguments, for example ["bash","-lc","ls -la"].`),
				"working_directory": object{"type": "string"},
				"timeout_ms":        object{"type": "integer"},
				"env":               object{"type": "object", "additionalProperties": object{"type": "string"}},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	},
	"shell": {
		callType:    "shell_call",
		idPrefix:    "sh_",
		description: "Run shell commands in order on the client machine (built-in shell tool).",
		parameters: object{
			"type": "object",
			"properties": object{
				"commands":          stringArraySchema("Shell command lines to run in order."),
				"timeout_ms":        object{"type": "integer"},
				"max_output_length": object{"type": "integer"},
			},
			"required":             []any{"commands"},
			"additionalProperties": false,
		},
	},
	"apply_patch": {
		callType:    "apply_patch_call",
		idPrefix:    "apc_",
		description: "Create, update or delete one file on the client machine (built-in apply_patch tool). Use a V4A diff for create_file and update_file.",
		parameters: object{
			"type": "object",
			"properties": object{
				"type": object{"type": "string", "enum": []any{"create_file", "update_file", "delete_file"}},
				"path": object{"type": "string"},
				"diff": object{"type": "string", "description": "V4A diff; omit for delete_file."},
			},
			"required":             []any{"type", "path"},
			"additionalProperties": false,
		},
	},
}

// builtinKindForCall maps a call or call-output item type to its tool kind.
func builtinKindForCall(itemType string) (string, bool) {
	itemType = strings.TrimSuffix(itemType, "_output")
	for kind, spec := range builtinClientTools {
		if spec.callType == itemType {
			return kind, true
		}
	}
	return "", false
}

func isBuiltinCall(item object) bool {
	_, ok := builtinKindForCall(text(item["type"]))
	return ok && !strings.HasSuffix(text(item["type"]), "_output")
}

// isClientCall reports whether a translated item is a call the client runs.
func isClientCall(item object) bool {
	return isTool(item) || isBuiltinCall(item)
}

// registerBuiltins adds the built-in client tools the request declared. A
// client tool already declared under the same name keeps it; the model can
// reach the same capability through that tool.
func (b *Bridge) registerBuiltins(catalog []any) []any {
	for _, kind := range b.builtins {
		if _, taken := b.tools[kind]; taken {
			continue
		}
		spec := builtinClientTools[kind]
		info := tool{Name: kind, Kind: kind, Definition: "builtin:" + kind, Parameters: spec.parameters}
		entry := object{"type": "function", "name": kind, "description": spec.description, "parameters": spec.parameters}
		if kind == "shell" {
			// One command line needs no JSON escaping on the raw path.
			info.RawField, info.RawAsList = "commands", true
			entry[catalogRawFieldKey], entry[catalogRawListKey] = "commands", true
		}
		b.tools[kind] = info
		catalog = append(catalog, entry)
	}
	return catalog
}

func stringList(value any, field string) ([]any, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, fmt.Errorf("basispoints %s must be a nonempty string array", field)
	}
	for _, item := range items {
		if s, ok := item.(string); !ok || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("basispoints %s must contain only nonempty strings", field)
		}
	}
	return items, nil
}

func copyOptional(target, source object, fields ...string) {
	for _, field := range fields {
		if value, ok := source[field]; ok && value != nil {
			target[field] = value
		}
	}
}

// optionalInteger keeps a non-negative integer field. The model writes these
// values inside a JSON string, so a quoted number is accepted and anything
// else is dropped: a wrongly typed optional field would make strictly typed
// clients reject the whole call item.
func optionalInteger(target, source object, field string) {
	var number json.Number
	switch value := source[field].(type) {
	case json.Number:
		number = value
	case string:
		number = json.Number(strings.TrimSpace(value))
	default:
		return
	}
	if parsed, err := number.Int64(); err == nil && parsed >= 0 {
		target[field] = json.Number(strconv.FormatInt(parsed, 10))
	}
}

func optionalString(target, source object, field string) {
	if value, ok := source[field].(string); ok && strings.TrimSpace(value) != "" {
		target[field] = value
	}
}

// stringMap keeps scalar entries as strings, the only env value type the
// client accepts; nested values are dropped.
func stringMap(value any) object {
	env := object{}
	source, _ := value.(object)
	for key, raw := range source {
		switch v := raw.(type) {
		case string:
			env[key] = v
		case json.Number, bool:
			env[key] = fmt.Sprint(v)
		}
	}
	return env
}

// builtinCallItem builds the client's native call item from envelope
// arguments already decoded as an object.
func builtinCallItem(kind, callID string, args object) (object, error) {
	spec := builtinClientTools[kind]
	result := object{"type": spec.callType, "id": spec.idPrefix + fingerprint(callID), "call_id": callID, "status": "completed"}
	switch kind {
	case "local_shell":
		command, err := stringList(args["command"], "local_shell command")
		if err != nil {
			return nil, err
		}
		action := object{"type": "exec", "command": command, "env": stringMap(args["env"])}
		optionalString(action, args, "working_directory")
		optionalInteger(action, args, "timeout_ms")
		result["action"] = action
	case "shell":
		commands, err := stringList(args["commands"], "shell commands")
		if err != nil {
			return nil, err
		}
		action := object{"commands": commands}
		optionalInteger(action, args, "timeout_ms")
		optionalInteger(action, args, "max_output_length")
		result["action"] = action
	case "apply_patch":
		operation := object{"type": text(args["type"]), "path": text(args["path"])}
		switch operation["type"] {
		case "create_file", "update_file":
			diff, ok := args["diff"].(string)
			// An empty create_file diff makes an empty file; an empty update
			// changes nothing and is treated as a malformed call.
			if !ok || operation["type"] == "update_file" && strings.TrimSpace(diff) == "" {
				return nil, fmt.Errorf("basispoints apply_patch %s requires a diff", operation["type"])
			}
			operation["diff"] = diff
		case "delete_file":
		default:
			return nil, fmt.Errorf("basispoints apply_patch type must be create_file, update_file or delete_file")
		}
		if strings.TrimSpace(text(operation["path"])) == "" {
			return nil, fmt.Errorf("basispoints apply_patch requires a path")
		}
		result["operation"] = operation
	default:
		return nil, fmt.Errorf("basispoints built-in tool %q is not relayable", kind)
	}
	return result, nil
}

// builtinEnvelope reverses builtinCallItem for history recovery: it returns
// the tool name and arguments the model would have sent through run_officejs.
func builtinEnvelope(item object) (string, object, error) {
	kind, _ := builtinKindForCall(text(item["type"]))
	switch kind {
	case "local_shell", "shell":
		action, ok := item["action"].(object)
		if !ok {
			return "", nil, fmt.Errorf("basispoints history %s call requires an action", kind)
		}
		args := object{}
		if kind == "local_shell" {
			copyOptional(args, action, "command", "working_directory", "timeout_ms", "env")
		} else {
			copyOptional(args, action, "commands", "timeout_ms", "max_output_length")
		}
		return kind, args, nil
	case "apply_patch":
		operation, ok := item["operation"].(object)
		if !ok {
			return "", nil, fmt.Errorf("basispoints history apply_patch call requires an operation")
		}
		args := object{}
		copyOptional(args, operation, "type", "path", "diff")
		return kind, args, nil
	}
	return "", nil, fmt.Errorf("basispoints history item is not a built-in tool call")
}

// builtinCallFingerprint identifies a built-in call by its kind, call ID and
// structured action, mirroring historyCallFingerprint for function calls.
func builtinCallFingerprint(item object) string {
	kind, id := text(item["type"]), text(item["call_id"])
	if id == "" || strings.TrimSpace(id) != id {
		return ""
	}
	canonical := object{"type": kind, "call_id": id}
	switch kind {
	case "local_shell_call", "shell_call":
		action, ok := item["action"].(object)
		if !ok {
			return ""
		}
		canonical["action"] = withoutEmptyFields(action)
	case "apply_patch_call":
		operation, ok := item["operation"].(object)
		if !ok {
			return ""
		}
		canonical["operation"] = withoutEmptyFields(operation)
	default:
		return ""
	}
	return fingerprint(canonical)
}

// withoutEmptyFields drops null and empty-object fields, which clients add or
// omit when they echo a call back (optional fields as null, an empty env), so
// the replay fingerprint depends only on what the call actually does.
func withoutEmptyFields(fields object) object {
	out := make(object, len(fields))
	for key, value := range fields {
		if value == nil {
			continue
		}
		if nested, ok := value.(object); ok && len(nested) == 0 {
			continue
		}
		out[key] = value
	}
	return out
}

// builtinOutputText renders a built-in tool result as the text BPS receives in
// a function_call_output, since BPS only knows run_officejs results.
func builtinOutputText(item object) any {
	switch text(item["type"]) {
	case "shell_call_output":
		chunks, ok := item["output"].([]any)
		if !ok {
			return item["output"]
		}
		var out strings.Builder
		for i, raw := range chunks {
			chunk, _ := raw.(object)
			if i > 0 {
				out.WriteString("\n\n")
			}
			fmt.Fprintf(&out, "Command %d", i+1)
			if outcome, ok := chunk["outcome"].(object); ok {
				switch text(outcome["type"]) {
				case "exit":
					if code, ok := outcome["exit_code"].(json.Number); ok {
						fmt.Fprintf(&out, " exited with code %s", code)
					} else {
						out.WriteString(" exited without an exit code")
					}
				case "timeout":
					out.WriteString(" timed out")
				}
			}
			if stdout := text(chunk["stdout"]); stdout != "" {
				out.WriteString("\nstdout:\n" + stdout)
			}
			if stderr := text(chunk["stderr"]); stderr != "" {
				out.WriteString("\nstderr:\n" + stderr)
			}
		}
		return out.String()
	case "apply_patch_call_output":
		status, body := text(item["status"]), text(item["output"])
		switch {
		case status == "":
			return body
		case body == "":
			return "Patch " + status
		default:
			return "Patch " + status + ":\n" + body
		}
	case "local_shell_call_output":
		if status := text(item["status"]); status != "" && status != "completed" {
			return "(command " + status + ")\n" + text(item["output"])
		}
		return item["output"]
	default:
		return item["output"]
	}
}
