package basispoints

import (
	"io"
	"strings"
	"testing"
)

func TestNormalizeEffortRejectsUnknownAndCapsAliases(t *testing.T) {
	if got, err := NormalizeEffort("max"); err != nil || got != "xhigh" {
		t.Fatalf("NormalizeEffort(max) = %q, %v", got, err)
	}
	if got, err := NormalizeEffort("minimal"); err != nil || got != "low" {
		t.Fatalf("NormalizeEffort(minimal) = %q, %v", got, err)
	}
	if _, err := NormalizeEffort("unsupported-tier"); err == nil {
		t.Fatal("NormalizeEffort accepted an unknown effort")
	}
}

func TestReplayCacheScopesAndFingerprintsCompleteCalls(t *testing.T) {
	var cache ReplayCache
	native := object{"type": "function_call", "id": "native-1", "call_id": "call-1", "name": "lookup", "arguments": `{"value":1}`}
	client := object{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": object{"value": 1}}
	cache.put("account:1", "call-1", native, client)
	if got := cache.getForCall("account:1", "call-1", client); got == nil {
		t.Fatal("matching call was not replayed")
	}
	changed := object{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": object{"value": 2}}
	if got := cache.getForCall("account:1", "call-1", changed); got != nil {
		t.Fatal("call ID was replayed after its arguments changed")
	}
	if got := cache.get("account:2", "call-1"); got != nil {
		t.Fatal("replay crossed account scope")
	}
}

func TestPrepareNormalizesToolResultIDAndDoesNotEmbedPrivateToolNames(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"function_call","id":"fc_original","call_id":"call-1","name":"lookup","arguments":{"value":1}},{"type":"function_call_output","id":"ctco_original","call_id":"call-1","output":"ok"}]}`)
	body, _, err := Prepare(raw, "account:1/key:2/thread:3", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.Contains(string(body), `"store":false`) || !strings.Contains(string(body), `"stream":true`) {
		t.Fatalf("prepared body did not force stream/store policy: %s", body)
	}
	if strings.Contains(string(body), "private_tool") || strings.Contains(string(body), "local/path/") {
		t.Fatalf("prepared body contains non-public tool or path data")
	}
	var prepared object
	if err := decode(body, &prepared); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	items, _ := prepared["input"].([]any)
	found := false
	for _, value := range items {
		item, _ := value.(object)
		if text(item["type"]) == "function_call_output" {
			found = strings.HasPrefix(text(item["id"]), "fc_")
		}
	}
	if !found {
		t.Fatal("function_call_output did not receive an fc_ id")
	}
}

func TestStreamEmitsOneTerminalEvent(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	converted := bridge.Stream(io.NopCloser(strings.NewReader("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\n")))
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	if strings.Count(string(out), `"type":"response.completed"`) != 1 {
		t.Fatalf("terminal event count = %d, stream=%s", strings.Count(string(out), `"type":"response.completed"`), out)
	}
}

func TestStreamSanitizesProtocolFailureAndStopsAtOneTerminal(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	converted := bridge.Stream(io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"c\",\"name\":\"unknown\",\"arguments\":\"{}\"}]}}\n\n")))
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err != nil {
		// The bridge reports protocol failures as a terminal response.failed frame.
		t.Fatalf("read converted stream: %v", err)
	}
	if !strings.Contains(string(out), `"type":"response.failed"`) || strings.Contains(string(out), "unknown") {
		t.Fatalf("protocol failure was not sanitized: %s", out)
	}
}

func TestStreamSanitizesProviderTerminalError(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	converted := bridge.Stream(io.NopCloser(strings.NewReader("event: response.failed\ndata: {\"type\":\"response.failed\",\"message\":\"top-level detail\",\"response\":{\"status\":\"failed\",\"status_details\":{\"error\":{\"message\":\"nested detail\"}},\"error\":{\"message\":\"sensitive upstream detail\"}}}\n\n")))
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	if strings.Contains(string(out), "top-level detail") || strings.Contains(string(out), "nested detail") || strings.Contains(string(out), "sensitive upstream detail") || !strings.Contains(string(out), "basispoints_upstream_error") {
		t.Fatalf("provider error was not sanitized: %s", out)
	}
}

func TestPrepareTranslatesStructuredOutputToPromptContract(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","input":"hi","text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}}`)
	body, _, err := Prepare(raw, "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if strings.Contains(string(body), `"text":{`) {
		t.Fatalf("prepared body forwarded text.format to the Excel wire: %s", body)
	}
	if !strings.Contains(string(body), "Schema name: answer") || !strings.Contains(string(body), `\"required\":[\"answer\"]`) {
		t.Fatalf("prepared body is missing the structured output contract: %s", body)
	}

	jsonObject := []byte(`{"model":"gpt-5.5","input":"hi","text":{"format":{"type":"json_object"}}}`)
	if body, _, err := Prepare(jsonObject, "account:1", &ReplayCache{}); err != nil || !strings.Contains(string(body), "one valid JSON object") {
		t.Fatalf("Prepare(json_object) = %s, %v", body, err)
	}

	if _, _, err := Prepare([]byte(`{"model":"gpt-5.5","input":"hi","text":{"format":{"type":"json_schema"}}}`), "account:1", &ReplayCache{}); err == nil {
		t.Fatal("Prepare accepted json_schema without a schema")
	}
	if _, _, err := Prepare([]byte(`{"model":"gpt-5.5","input":"hi","text":{"format":{"type":"grammar"}}}`), "account:1", &ReplayCache{}); err == nil {
		t.Fatal("Prepare accepted an unknown text format")
	}
}
