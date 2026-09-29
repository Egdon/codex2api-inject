package database

import "strings"

// NormalizeCodexBasispointsModels canonicalizes the global Basispoints model
// list. Names are trimmed, lowercased and de-duplicated in their first order;
// commas, whitespace and newlines all separate entries. An empty result means
// the list does not restrict which models use Basispoints.
func NormalizeCodexBasispointsModels(raw string) string {
	return strings.Join(ParseCodexBasispointsModels(raw), ",")
}

// ParseCodexBasispointsModels returns the normalized model names of raw.
func ParseCodexBasispointsModels(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(fields) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(fields))
	models := make([]string, 0, len(fields))
	for _, field := range fields {
		model := strings.ToLower(strings.TrimSpace(field))
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	return models
}
