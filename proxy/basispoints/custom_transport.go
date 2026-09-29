package basispoints

import (
	"fmt"
	"strings"
	"unicode"
)

const customTransportPrefix = "codex2api.custom/"

// customTransportEnvelope recognizes only the explicitly tagged raw custom-tool
// transport. The caller must still check the exact catalog name, custom kind and
// call identity. Ordinary run_officejs calls continue through the JSON decoder.
func customTransportEnvelope(arguments object) (object, bool, error) {
	summary, ok := arguments["summary"].(string)
	if !ok || !strings.HasPrefix(summary, customTransportPrefix) {
		return nil, false, nil
	}
	name := strings.TrimPrefix(summary, customTransportPrefix)
	if name == "" || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsSpace) >= 0 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, true, fmt.Errorf("basispoints raw custom transport requires an exact nonempty catalog tool name in summary")
	}
	input, ok := arguments["code"].(string)
	if !ok {
		return nil, true, fmt.Errorf("basispoints raw custom transport code must be a string")
	}
	if len(input) > maxEnvelopeBytes {
		return nil, true, fmt.Errorf("basispoints raw custom transport code exceeds the size limit")
	}
	return object{"name": name, "input": input}, true, nil
}

const rawFieldTransportPrefix = "codex2api.raw/"

// catalogRawFieldKey and catalogRawListKey mark a catalog entry's raw-field
// argument for describeCatalog; they are never sent upstream as tool schema.
const (
	catalogRawFieldKey = "_codex2api_raw_field"
	catalogRawListKey  = "_codex2api_raw_list"
)

// rawFieldFor returns the argument a function tool can take through the
// raw-field transport: its only required argument, when that is a string.
// Other arguments keep their defaults on that path.
func rawFieldFor(parameters object) string {
	required, _ := parameters["required"].([]any)
	if text(parameters["type"]) != "object" || len(required) != 1 {
		return ""
	}
	field := text(required[0])
	properties, _ := parameters["properties"].(object)
	property, _ := properties[field].(object)
	if !validTransportToken(field) || text(property["type"]) != "string" {
		return ""
	}
	return field
}

func validTransportToken(value string) bool {
	return value != "" && !strings.ContainsAny(value, "/\\") &&
		strings.IndexFunc(value, unicode.IsSpace) < 0 && strings.IndexFunc(value, unicode.IsControl) < 0
}

// rawFieldEnvelope recognizes summary "codex2api.raw/NAME/FIELD": code carries
// that field's value verbatim, so a command with quotes or backslashes needs
// no nested JSON escaping. Only a tool's declared raw field is accepted.
func (b *Bridge) rawFieldEnvelope(arguments object) (object, bool, error) {
	summary, ok := arguments["summary"].(string)
	if !ok || !strings.HasPrefix(summary, rawFieldTransportPrefix) {
		return nil, false, nil
	}
	spec := strings.TrimPrefix(summary, rawFieldTransportPrefix)
	cut := strings.LastIndexByte(spec, '/')
	if cut <= 0 || !validTransportToken(spec[:cut]) || !validTransportToken(spec[cut+1:]) {
		return nil, true, fmt.Errorf("basispoints raw field transport requires summary codex2api.raw/CATALOG_NAME/FIELD")
	}
	name, field := spec[:cut], spec[cut+1:]
	info, ok := b.tools[name]
	if !ok {
		return nil, true, fmt.Errorf("basispoints returned a tool outside the client's catalog")
	}
	if info.RawField == "" || info.RawField != field {
		return nil, true, fmt.Errorf("basispoints raw field transport does not accept that field for this tool")
	}
	value, ok := arguments["code"].(string)
	if !ok {
		return nil, true, fmt.Errorf("basispoints raw field transport code must be a string")
	}
	if len(value) > maxEnvelopeBytes {
		return nil, true, fmt.Errorf("basispoints raw field transport code exceeds the size limit")
	}
	var argument any = value
	if info.RawAsList {
		argument = []any{value}
	}
	return object{"name": name, "arguments": object{field: argument}}, true, nil
}
