package basispoints

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

// allCacheWriteUsage carries every cache-creation alias plus an integer beyond
// float64 precision, which must survive the rewrite byte for byte.
const allCacheWriteUsage = `{"input_tokens":1000,"output_tokens":50,"total_tokens":1050,"input_tokens_details":{"cached_tokens":100,"cache_write_tokens":200,"cache_creation_tokens":200},"prompt_tokens_details":{"cached_tokens":100,"cache_write_tokens":200,"cache_creation_tokens":200},"cache_creation_input_tokens":200,"cache_write_input_tokens":200,"cache_creation_tokens":200,"cache_write_tokens":200,"cache_creation":{"ephemeral_5m_input_tokens":150,"ephemeral_1h_input_tokens":50},"output_tokens_details":{"reasoning_tokens":3},"extension":9007199254740993}`

func TestStreamReportsCacheWritesAsInput(t *testing.T) {
	for _, asInput := range []bool{false, true} {
		bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}, CacheWritesAsInput: asInput}
		var upstream strings.Builder
		for _, kind := range []string{"response.created", "response.in_progress", "response.completed"} {
			upstream.WriteString("event: " + kind + "\ndata: {\"type\":\"" + kind + "\",\"usage\":" + allCacheWriteUsage + ",\"response\":{\"id\":\"r1\",\"status\":\"" + map[bool]string{false: "in_progress", true: "completed"}[kind == "response.completed"] + "\",\"output\":[],\"usage\":" + allCacheWriteUsage + "}}\n\n")
		}
		out := readStream(t, bridge, io.NopCloser(strings.NewReader(upstream.String())))
		events := 0
		for _, frame := range strings.Split(out, "\n") {
			data, ok := strings.CutPrefix(frame, "data: ")
			if !ok {
				continue
			}
			events++
			for _, path := range []string{"usage", "response.usage"} {
				usage := gjson.Get(data, path)
				if !asInput {
					assertJSONEqual(t, allCacheWriteUsage, usage.Raw)
					continue
				}
				for _, field := range []string{
					"input_tokens_details.cache_write_tokens", "prompt_tokens_details.cache_write_tokens",
					"input_tokens_details.cache_creation_tokens", "prompt_tokens_details.cache_creation_tokens",
					"cache_write_tokens", "cache_creation_input_tokens", "cache_write_input_tokens", "cache_creation_tokens",
					"cache_creation.ephemeral_5m_input_tokens", "cache_creation.ephemeral_1h_input_tokens",
				} {
					if value := usage.Get(field); !value.Exists() || value.Raw != "0" {
						t.Fatalf("%s %s = %s, want 0", path, field, value.Raw)
					}
				}
				if usage.Get("input_tokens").Int() != 1000 || usage.Get("input_tokens_details.cached_tokens").Int() != 100 ||
					usage.Get("prompt_tokens_details.cached_tokens").Int() != 100 || usage.Get("total_tokens").Int() != 1050 ||
					usage.Get("output_tokens").Int() != 50 || usage.Get("output_tokens_details.reasoning_tokens").Int() != 3 {
					t.Fatalf("%s totals changed: %s", path, usage.Raw)
				}
				if raw := usage.Get("extension").Raw; raw != "9007199254740993" {
					t.Fatalf("%s extension = %s", path, raw)
				}
			}
		}
		if events != 3 {
			t.Fatalf("events = %d, want 3: %s", events, out)
		}
	}
}

func TestReportCacheWritesAsInputKeepsNativeShape(t *testing.T) {
	for _, raw := range []string{
		`{"type":"response.output_text.delta","delta":"cache_write_tokens"}`,
		`{"type":"response.completed","response":{"usage":null}}`,
		`{"response":{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":2}}}}`,
		`{"response":{"usage":{"input_tokens":10,"input_tokens_details":{"cached_tokens":2,"cache_write_tokens":0}}}}`,
		`{"usage":{"input_tokens":10,"cache_creation":null}}`,
	} {
		var payload object
		if err := decode([]byte(raw), &payload); err != nil {
			t.Fatal(err)
		}
		if reportCacheWritesAsInput(payload) {
			t.Fatalf("payload without cache writes was changed: %s", raw)
		}
		got, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		assertJSONEqual(t, raw, string(got))
	}
}

func assertJSONEqual(t *testing.T, want, got string) {
	t.Helper()
	var left, right any
	if err := decode([]byte(want), &left); err != nil {
		t.Fatalf("decode want: %v", err)
	}
	if err := decode([]byte(got), &right); err != nil {
		t.Fatalf("decode got %q: %v", got, err)
	}
	a, _ := json.Marshal(left)
	b, _ := json.Marshal(right)
	if string(a) != string(b) {
		t.Fatalf("JSON differs\nwant %s\n got %s", a, b)
	}
}
