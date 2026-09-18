package version

// Release builds inject these strings using -X github.com/codex2api/internal/version.<Name>.
var (
	Version      = "dev"
	Source       = "patched"
	UpstreamBase = "unknown"
	Revision     = "unknown"
)

func Current() string { return Version }
