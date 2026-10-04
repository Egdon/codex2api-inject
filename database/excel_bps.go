package database

import "strings"

// NormalizeUpstreamSource describes the observed request path, never today's
// account configuration. Empty/unknown remains unknown; unrecognized sources
// are other, not native Codex. The retired "bps" source remains recognizable for
// historical usage and quality-test records; it is not a routing capability.
func NormalizeUpstreamSource(source string) string {
	switch source = strings.ToLower(strings.TrimSpace(source)); source {
	case "", "unknown":
		return ""
	case "bps", "codex", "other":
		return source
	default:
		return "other"
	}
}

func nullableUpstreamSource(source string) any {
	if source = NormalizeUpstreamSource(source); source != "" {
		return source
	}
	return nil
}
