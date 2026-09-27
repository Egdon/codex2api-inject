package basispoints

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestStreamDropsUntranslatableToolCallWithoutFailingResponse(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	converted := bridge.Stream(io.NopCloser(strings.NewReader("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"done\"}]},{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"c\",\"name\":\"unknown\",\"arguments\":\"{}\"}]}}\n\n")))
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	if strings.Count(string(out), `"type":"response.completed"`) != 1 || strings.Contains(string(out), "unknown") || strings.Contains(string(out), "function_call") || !strings.Contains(string(out), "done") {
		t.Fatalf("untranslatable call was not dropped cleanly: %s", out)
	}
}

func TestStreamSanitizesProtocolFailureAndStopsAtOneTerminal(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	stream := "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"c\",\"name\":\"secret_tool\",\"arguments\":\"{}\"}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	converted := bridge.Stream(io.NopCloser(strings.NewReader(stream)))
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err != nil {
		// The bridge reports protocol failures as a terminal response.failed frame.
		t.Fatalf("read converted stream: %v", err)
	}
	if strings.Count(string(out), "event: ") != 1 || !strings.Contains(string(out), `"type":"response.failed"`) || strings.Contains(string(out), "secret_tool") {
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

func TestPrepareAcceptsInlineBase64Images(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo=","detail":"auto"}]}]}`)
	body, _, err := Prepare(raw, "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !strings.Contains(string(body), "data:image/png;base64,iVBORw0KGgo=") {
		t.Fatalf("inline image was not forwarded: %s", body)
	}
	for _, url := range []string{"data:text/plain;base64,aGk=", "data:image/png,raw", "http://example.com/a.png"} {
		bad := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + url + `"}]}]}`)
		if _, _, err := Prepare(bad, "account:1", &ReplayCache{}); err == nil {
			t.Fatalf("Prepare accepted image_url %q", url)
		}
	}
}

func TestRewriteImagesPlacesInlineImagesByMode(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"what is this"},{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]},{"type":"function_call","id":"fc_1","call_id":"call-1","name":"view","arguments":{}},{"type":"function_call_output","call_id":"call-1","output":[{"type":"input_image","image_url":"data:image/jpeg;base64,/9j/4AAQ"}]}]}`)
	prepared, _, err := Prepare(raw, "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	var mediaTypes []string
	upload := func(image InlineImage) (string, error) {
		mediaType, data, err := image.Decode()
		if err != nil || image.Digest == "" {
			t.Fatalf("Decode = %v, digest %q", err, image.Digest)
		}
		mediaTypes = append(mediaTypes, mediaType)
		if mediaType == "image/png" && !strings.HasPrefix(string(data), "\x89PNG") {
			t.Fatalf("uploaded bytes were not decoded: %q", data)
		}
		return "file-" + strings.TrimPrefix(mediaType, "image/"), nil
	}

	out, report, err := RewriteImages(prepared, ImagesDefault, upload)
	if err != nil {
		t.Fatalf("RewriteImages(default): %v", err)
	}
	if strings.Contains(string(out), "data:image/png") || !strings.Contains(string(out), `"file_id":"file-png"`) {
		t.Fatalf("message image was not uploaded: %s", out)
	}
	if !strings.Contains(string(out), "data:image/jpeg") || !report.Inline || len(report.FileIDs) != 1 {
		t.Fatalf("tool result image did not stay inline: report=%+v body=%s", report, out)
	}

	mediaTypes = nil
	out, report, err = RewriteImages(prepared, ImagesUploadAll, upload)
	if err != nil || strings.Contains(string(out), "data:image/") || report.Inline || len(mediaTypes) != 2 {
		t.Fatalf("RewriteImages(upload all) = %+v, %v, uploads=%v body=%s", report, err, mediaTypes, out)
	}

	out, report, err = RewriteImages(prepared, ImagesOmit, upload)
	if err != nil || strings.Contains(string(out), "data:image/") || report.Any() || strings.Count(string(out), "image content omitted") != 2 {
		t.Fatalf("RewriteImages(omit) = %+v, %v, body=%s", report, err, out)
	}

	calls := 0
	failing := func(InlineImage) (string, error) {
		calls++
		return "", io.ErrUnexpectedEOF
	}
	out, report, err = RewriteImages(prepared, ImagesUploadAll, failing)
	if err != nil || report.UploadErr == nil || report.Any() || calls != 1 || strings.Contains(string(out), "data:image/") {
		t.Fatalf("failed upload = %+v, %v, calls=%d body=%s", report, err, calls, out)
	}

	plain := []byte(`{"input":[]}`)
	if out, _, err := RewriteImages(plain, ImagesDefault, nil); err != nil || string(out) != string(plain) {
		t.Fatalf("body without images changed: %s, %v", out, err)
	}
}

func preparedBody(t *testing.T, raw string, replay *ReplayCache) (object, *Bridge) {
	t.Helper()
	body, bridge, err := Prepare([]byte(raw), "account:1", replay)
	if err != nil {
		t.Fatalf("Prepare(%s): %v", raw, err)
	}
	var prepared object
	if err := decode(body, &prepared); err != nil {
		t.Fatalf("decode prepared body: %v", err)
	}
	return prepared, bridge
}

func TestPrepareDegradesUnsupportedRequestFields(t *testing.T) {
	tools := `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"web_search"},{"type":"file_search"}]`
	prepared, bridge := preparedBody(t, `{"model":"gpt-5.5","input":[{"type":"item_reference","id":"msg_1"},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],`+tools+`,"tool_choice":{"type":"function","name":"lookup"},"reasoning":{"effort":"turbo","mode":"pro"}}`, &ReplayCache{})
	raw, _ := json.Marshal(prepared)
	if prepared["reasoning_effort"] != "medium" || !strings.Contains(string(raw), `call client tool \"lookup\"`) {
		t.Fatalf("effort or tool_choice was not degraded: %s", raw)
	}
	if !strings.Contains(string(raw), "file_search, web_search") || len(bridge.Warnings) != 2 || !strings.Contains(string(raw), "missing from the history") {
		t.Fatalf("unknown tool kinds were not reported as unavailable: %v %s", bridge.Warnings, raw)
	}
	if strings.Contains(string(raw), "item_reference") {
		t.Fatalf("item_reference was forwarded: %s", raw)
	}
	required, _ := preparedBody(t, `{"model":"gpt-5.5","input":"hi",`+tools+`,"tool_choice":"required"}`, &ReplayCache{})
	if raw, _ := json.Marshal(required); !strings.Contains(string(raw), "at least one client tool") {
		t.Fatalf("tool_choice required was not turned into a requirement: %s", raw)
	}
}

func TestPrepareParallelGuidanceIterationsAndMetadata(t *testing.T) {
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}],"internal_chat_message_metadata_passthrough":{"turn_id":"a"}},` +
		`{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"one"},{"type":"function_call_output","call_id":"c2","output":""},` +
		`{"type":"function_call","call_id":"c3","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c3","output":"three"}]`
	tools := `"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`
	metadata := `"metadata":{"trace":"t-1","task_id":"spoofed","n":3}`
	prepared, bridge := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,`+tools+`,`+metadata+`}`, &ReplayCache{})
	meta, _ := prepared["metadata"].(object)
	if meta["agent_iteration"] != "3" || meta["trace"] != "t-1" || meta["n"] != "3" || meta["task_id"] == "spoofed" {
		t.Fatalf("metadata = %v", meta)
	}
	raw, _ := json.Marshal(prepared)
	if !bridge.Parallel || !strings.Contains(string(raw), "separate run_officejs calls in the same response") {
		t.Fatalf("parallel guidance missing: %s", raw)
	}
	if !strings.Contains(string(raw), "(tool call succeeded with no output)") || strings.Contains(string(raw), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("empty output or passthrough metadata not normalized: %s", raw)
	}

	// Codex restamps passthrough metadata each turn; identity must not move.
	restamped := strings.Replace(history, `"turn_id":"a"`, `"turn_id":"b"`, 1)
	again, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+restamped+`,`+tools+`}`, &ReplayCache{})
	if again["metadata"].(object)["turn_id"] != meta["turn_id"] {
		t.Fatal("turn_id changed with Codex passthrough metadata")
	}

	serial, bridge := preparedBody(t, `{"model":"gpt-5.5","input":"hi",`+tools+`,"parallel_tool_calls":false}`, &ReplayCache{})
	if raw, _ := json.Marshal(serial); bridge.Parallel || !strings.Contains(string(raw), "one client tool at a time") {
		t.Fatalf("serial guidance missing: %s", raw)
	}
}

func TestTranslateHistoryAnswersNativePlanWithExcelResult(t *testing.T) {
	replay := &ReplayCache{}
	native := object{"type": "function_call", "id": "fc_plan", "call_id": "plan-1", "name": "update_plan", "arguments": `{"summary":"s","plan":[]}`}
	client := object{"type": "function_call", "call_id": "plan-1", "name": "update_plan", "arguments": object{"plan": []any{}}}
	replay.put("account:1", "plan-1", native, client)
	raw := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},{"type":"function_call","call_id":"plan-1","name":"update_plan","arguments":{"plan":[]}},{"type":"function_call_output","call_id":"plan-1","output":"Plan updated"}]}`
	prepared, _ := preparedBody(t, raw, replay)
	body, _ := json.Marshal(prepared)
	if !strings.Contains(string(body), `"output":"{\"status\":\"ok\"}"`) || strings.Contains(string(body), "Plan updated") {
		t.Fatalf("native plan result was not normalized: %s", body)
	}
}

type mapReplayBacking struct {
	mu      sync.Mutex
	entries map[string][]byte
	loads   int
	fail    bool
}

func (m *mapReplayBacking) Load(key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loads++
	if m.fail {
		return nil, false, io.ErrUnexpectedEOF
	}
	raw, ok := m.entries[key]
	return raw, ok, nil
}

// counts reads the fields workers write, under the same lock, so tests stay
// race-free even when Prefetch returns at its deadline with stragglers.
func (m *mapReplayBacking) counts() (loads, entries int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loads, len(m.entries)
}

func (m *mapReplayBacking) Store(key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return io.ErrUnexpectedEOF
	}
	if m.entries == nil {
		m.entries = make(map[string][]byte)
	}
	m.entries[key] = value
	return nil
}

func TestReplayCacheHydratesFromBackingAfterRestart(t *testing.T) {
	backing := &mapReplayBacking{}
	first := &ReplayCache{}
	first.SetBacking(backing)
	native := object{"type": "function_call", "id": "fc_native", "call_id": "call-1", "name": "run_officejs", "arguments": `{"code":"x"}`}
	client := object{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": object{"value": 1}}
	first.putNative("account:1", "call-1", native, client)
	first.put("account:1", "call-2", object{"type": "function_call", "id": "fc_rebuilt"}, client)
	first.pendingWrites.Wait()
	if _, entries := backing.counts(); entries != 1 {
		t.Fatalf("only the exact native item may be persisted, got %d entries", entries)
	}

	restarted := &ReplayCache{}
	restarted.SetBacking(backing)
	if got := restarted.getForCall("account:1", "call-1", client); got != nil {
		t.Fatal("lookup reached the backing store outside Prefetch")
	}
	restarted.Prefetch("account:1", []string{"call-1", "call-1", "call-2", ""})
	if got := restarted.getForCall("account:1", "call-1", client); text(got["id"]) != "fc_native" {
		t.Fatalf("replay was not hydrated from backing: %v", got)
	}
	if loads, _ := backing.counts(); loads != 2 {
		t.Fatalf("Prefetch loads = %d, want one per distinct missing id", loads)
	}
	changed := object{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": object{"value": 2}}
	if got := restarted.getForCall("account:1", "call-1", changed); got != nil {
		t.Fatal("hydrated replay ignored the call fingerprint")
	}
	restarted.Prefetch("account:2", []string{"call-1"})
	if got := restarted.get("account:2", "call-1"); got != nil {
		t.Fatal("hydrated replay crossed account scope")
	}
}

func TestReplayCachePausesFailingBacking(t *testing.T) {
	backing := &mapReplayBacking{fail: true}
	cache := &ReplayCache{}
	cache.SetBacking(backing)
	cache.Prefetch("account:1", []string{"a", "b", "c"})
	loads, _ := backing.counts()
	if loads == 0 || loads > replayPrefetchWorkers {
		t.Fatalf("first failing prefetch loads = %d", loads)
	}
	cache.Prefetch("account:1", []string{"d", "e"})
	cache.putNative("account:1", "f", object{"type": "function_call"})
	cache.pendingWrites.Wait()
	if after, entries := backing.counts(); after != loads || entries != 0 {
		t.Fatalf("backing was used during its cooldown: loads=%d entries=%d", after, entries)
	}
}

func readStream(t *testing.T, bridge *Bridge, upstream io.ReadCloser) string {
	t.Helper()
	converted := bridge.Stream(upstream)
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	return string(out)
}

func TestStreamCompletesResponseCutOffAfterLastItem(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	if !strings.Contains(out, `"type":"response.completed"`) || !strings.Contains(out, `"id":"r1"`) || strings.Count(out, `"text":"answer"`) != 2 {
		t.Fatalf("cut-off stream was not completed: %s", out)
	}

	commentary := strings.Replace(stream, `"phase":"final_answer"`, `"phase":"commentary"`, 1)
	converted := bridge.Stream(io.NopCloser(strings.NewReader(commentary)))
	defer converted.Close()
	if _, err := io.ReadAll(converted); err == nil {
		t.Fatal("stream cut after commentary was completed instead of left to the client retry")
	}
}

func TestStreamSendsKeepaliveWhileUpstreamIsSilent(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}, keepalive: 5 * time.Millisecond}
	reader, writer := io.Pipe()
	released := make(chan struct{})
	go func() {
		_, _ = io.WriteString(writer, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
		// The upstream stays silent until the test has seen a keepalive.
		<-released
		_, _ = io.WriteString(writer, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[]}}\n\n")
		_ = writer.Close()
	}()
	converted := bridge.Stream(reader)
	defer converted.Close()
	guard := time.AfterFunc(10*time.Second, func() { _ = converted.Close() })
	defer guard.Stop()
	lines := bufio.NewReader(converted)
	for {
		line, err := lines.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before a keepalive: %v", err)
		}
		if strings.TrimSpace(line) == "event: response.in_progress" {
			break
		}
	}
	close(released)
	rest, err := io.ReadAll(lines)
	if err != nil || !strings.Contains(string(rest), `"type":"response.completed"`) {
		t.Fatalf("stream did not complete after keepalive: %v %s", err, rest)
	}
}

func TestStreamHonoursDisabledParallelCallsAndShowsReasoning(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, unsupportedTools: map[string]bool{}, tools: map[string]tool{
		"lookup": {Name: "lookup", Kind: "function"},
	}}
	call := func(id string) string {
		return `{"type":"function_call","id":"fc_` + id + `","call_id":"` + id + `","name":"lookup","arguments":"{}"}`
	}
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		`{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"why"}]},` + call("a") + "," + call("b") + "]}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	if !strings.Contains(out, `"call_id":"a"`) || strings.Contains(out, `"call_id":"b"`) || !strings.Contains(out, `**Thinking**\n\nwhy`) {
		t.Fatalf("serial mode or reasoning summary not applied: %s", out)
	}
	bridge.Parallel = true
	out = readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	if !strings.Contains(out, `"call_id":"a"`) || !strings.Contains(out, `"call_id":"b"`) || !strings.Contains(out, `"parallel_tool_calls":true`) {
		t.Fatalf("parallel calls were not all relayed: %s", out)
	}
}

func TestStreamDoesNotCompleteStreamCutAfterReasoning(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]}}\n\n" +
		"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"summary\":[]}}\n\n" +
		"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"summary\":[],\"encrypted_content\":\"e\"}}\n\n"
	converted := bridge.Stream(io.NopCloser(strings.NewReader(stream)))
	defer converted.Close()
	out, err := io.ReadAll(converted)
	if err == nil || strings.Contains(string(out), "response.completed") || bridge.SynthesizedCompletion() {
		t.Fatalf("stream cut after reasoning was reported complete: %v %s", err, out)
	}
}

func TestStreamFailsWhenEveryToolCallIsDroppedAndNothingElseRemains(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"reasoning\",\"summary\":[]},{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"c\",\"name\":\"excel_native\",\"arguments\":\"{}\"}]}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	if !strings.Contains(out, `"type":"response.failed"`) || strings.Contains(out, "response.completed") || strings.Contains(out, "excel_native") {
		t.Fatalf("empty turn was not failed: %s", out)
	}
}

func TestTerminalErrorShapeOmitsProviderMessages(t *testing.T) {
	shape := TerminalErrorShape(object{"response": object{"error": object{"code": "invalid_prompt", "type": "invalid_request_error", "message": "echo of the user's secret prompt"}}})
	if strings.Contains(shape, "secret") || !strings.Contains(shape, "invalid_prompt") {
		t.Fatalf("TerminalErrorShape = %s", shape)
	}
}

func TestPrepareAppliesAllowedToolsAndNotesSkippedReferences(t *testing.T) {
	raw := `{"model":"gpt-5.5","input":[{"type":"item_reference","id":"msg_1"},{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]}],` +
		`"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}},{"type":"function","name":"exec_command","parameters":{"type":"object"}}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"read_file"}]}}`
	prepared, bridge := preparedBody(t, raw, &ReplayCache{})
	body, _ := json.Marshal(prepared)
	if strings.Contains(string(body), "exec_command") || !strings.Contains(string(body), "read_file") || bridge.tools["exec_command"].Name != "" {
		t.Fatalf("allowed_tools did not narrow the catalog: %s", body)
	}
	if !strings.Contains(string(body), "at least one client tool") || !strings.Contains(string(body), "missing from the history") {
		t.Fatalf("required mode or skipped-reference note missing: %s", body)
	}
}

func TestInlineImageRejectsHeaderInjectingMediaType(t *testing.T) {
	image := InlineImage{url: "data:image/png\r\nx-evil: 1;base64,iVBORw0KGgo="}
	if _, _, err := image.Decode(); err == nil {
		t.Fatal("media type with CR/LF was accepted")
	}
	if mediaType, _, err := (InlineImage{url: "data:image/svg+xml;base64,PHN2Zz4="}).Decode(); err != nil || mediaType != "image/svg+xml" {
		t.Fatalf("plain media type rejected: %q %v", mediaType, err)
	}
}

func transportCall(callID, name string, arguments string) string {
	code, _ := json.Marshal(`{"name":"` + name + `","arguments":` + arguments + `}`)
	args, _ := json.Marshal(`{"code":` + string(code) + `,"summary":"s","extended_summary":"e","destructive":false,"references":[]}`)
	return `{"type":"function_call","id":"fc_` + callID + `","call_id":"` + callID + `","name":"run_officejs","arguments":` + string(args) + `}`
}

func TestBuiltinClientToolsAreDeclaredAndRelayedAsNativeItems(t *testing.T) {
	replay := &ReplayCache{}
	request := `{"model":"gpt-5.5","input":"list files","tools":[{"type":"shell"},{"type":"local_shell"},{"type":"apply_patch"},{"type":"web_search"}]}`
	body, bridge, err := Prepare([]byte(request), "account:1", replay)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, name := range []string{`Client tool \"shell\"`, `Client tool \"local_shell\"`, `Client tool \"apply_patch\"`} {
		if !strings.Contains(string(body), name) {
			t.Fatalf("catalog is missing %s: %s", name, body)
		}
	}
	if len(bridge.Warnings) != 1 || !strings.Contains(bridge.Warnings[0], "web_search") {
		t.Fatalf("only hosted tools should be reported unavailable: %v", bridge.Warnings)
	}

	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		transportCall("c-sh", "shell", `{"commands":["ls -la"],"timeout_ms":1000}`) + "," +
		transportCall("c-lsh", "local_shell", `{"command":["bash","-lc","pwd"]}`) + "," +
		transportCall("c-apc", "apply_patch", `{"type":"update_file","path":"a.go","diff":"@@\\n-x\\n+y"}`) + "]}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	for _, want := range []string{
		`"type":"shell_call"`, `"commands":["ls -la"]`,
		`"type":"local_shell_call"`, `"action":{"command":["bash","-lc","pwd"],"env":{},"type":"exec"}`,
		`"type":"apply_patch_call"`, `"operation":{"diff":"@@\\n-x\\n+y","path":"a.go","type":"update_file"}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stream is missing %s: %s", want, out)
		}
	}
	if strings.Contains(out, "run_officejs") || strings.Contains(out, "function_call_arguments") {
		t.Fatalf("built-in calls leaked transport or argument deltas: %s", out)
	}

	// The next turn replays the exact run_officejs items and turns the
	// structured results into text BPS understands.
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"list files"}]},` +
		`{"type":"shell_call","id":"sh_x","call_id":"c-sh","status":"completed","action":{"commands":["ls -la"],"timeout_ms":1000}},` +
		`{"type":"shell_call_output","call_id":"c-sh","max_output_length":100,"output":[{"stdout":"a.go\n","stderr":"","outcome":{"type":"exit","exit_code":0}}]},` +
		`{"type":"apply_patch_call","call_id":"c-apc","status":"completed","operation":{"type":"update_file","path":"a.go","diff":"@@\\n-x\\n+y"}},` +
		`{"type":"apply_patch_call_output","call_id":"c-apc","status":"failed","output":"context mismatch"}]`
	next, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,"tools":[{"type":"shell"},{"type":"apply_patch"}]}`, replay)
	raw, _ := json.Marshal(next)
	if !strings.Contains(string(raw), `"id":"fc_c-sh"`) || strings.Contains(string(raw), `"shell_call`) || strings.Contains(string(raw), "max_output_length\":100") {
		t.Fatalf("shell history was not replayed as transport items: %s", raw)
	}
	if !strings.Contains(string(raw), `Command 1 exited with code 0\nstdout:\na.go`) || !strings.Contains(string(raw), `Patch failed:\ncontext mismatch`) {
		t.Fatalf("built-in results were not rendered: %s", raw)
	}
}

func TestBuiltinClientToolsYieldToDeclaredToolsAndRejectBadArguments(t *testing.T) {
	request := `{"model":"gpt-5.5","input":"go","tools":[{"type":"shell"},{"type":"function","name":"shell","parameters":{"type":"object"}}]}`
	body, bridge, err := Prepare([]byte(request), "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if bridge.tools["shell"].Kind != "function" || strings.Contains(string(body), "built-in shell tool") || len(bridge.Warnings) != 0 {
		t.Fatalf("declared shell function did not win: %+v %s", bridge.tools["shell"], body)
	}

	_, bridge, _ = Prepare([]byte(`{"model":"gpt-5.5","input":"go","tools":[{"type":"shell"}]}`), "account:1", &ReplayCache{})
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		transportCall("c-bad", "shell", `{"commands":[]}`) + "]}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	if !strings.Contains(out, `"type":"response.failed"`) || strings.Contains(out, "shell_call") {
		t.Fatalf("invalid shell arguments were relayed: %s", out)
	}

	choice, _ := preparedBody(t, `{"model":"gpt-5.5","input":"go","tools":[{"type":"shell"},{"type":"apply_patch"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"shell"}]}}`, &ReplayCache{})
	if raw, _ := json.Marshal(choice); strings.Contains(string(raw), "apply_patch") || !strings.Contains(string(raw), "built-in shell tool") {
		t.Fatalf("allowed_tools did not keep only the built-in shell tool: %s", raw)
	}
}

func builtinItem(t *testing.T, kind, arguments string) (object, error) {
	t.Helper()
	var args object
	if err := decode([]byte(arguments), &args); err != nil {
		t.Fatalf("decode %s: %v", arguments, err)
	}
	return builtinCallItem(kind, "call-1", args)
}

func TestBuiltinCallItemNormalizesOptionalArguments(t *testing.T) {
	item, err := builtinItem(t, "shell", `{"commands":["ls"],"timeout_ms":"30000","max_output_length":"lots"}`)
	if err != nil {
		t.Fatalf("builtinCallItem: %v", err)
	}
	action, _ := item["action"].(object)
	if action["timeout_ms"] != json.Number("30000") || action["max_output_length"] != nil {
		t.Fatalf("shell optional fields not normalized: %v", action)
	}
	item, err = builtinItem(t, "local_shell", `{"command":["env"],"working_directory":7,"timeout_ms":-5,"env":{"A":"1","B":2,"C":true,"D":{"x":1}}}`)
	if err != nil {
		t.Fatalf("builtinCallItem: %v", err)
	}
	action, _ = item["action"].(object)
	env, _ := action["env"].(object)
	if action["working_directory"] != nil || action["timeout_ms"] != nil || env["A"] != "1" || env["B"] != "2" || env["C"] != "true" || env["D"] != nil {
		t.Fatalf("local_shell optional fields not normalized: %v", action)
	}
	if _, err := builtinItem(t, "apply_patch", `{"type":"update_file","path":"a.go","diff":"  "}`); err == nil {
		t.Fatal("empty update_file diff was accepted")
	}
	if _, err := builtinItem(t, "apply_patch", `{"type":"create_file","path":"empty.txt","diff":""}`); err != nil {
		t.Fatalf("empty create_file diff was rejected: %v", err)
	}
}

func TestBuiltinHistoryPairsByIDAndToleratesEchoedNulls(t *testing.T) {
	replay := &ReplayCache{}
	_, bridge, err := Prepare([]byte(`{"model":"gpt-5.5","input":"go","tools":[{"type":"local_shell"}]}`), "account:1", replay)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		transportCall("c-l", "local_shell", `{"command":["pwd"]}`) + "]}}\n\n"
	if out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream))); !strings.Contains(out, `"type":"local_shell_call"`) {
		t.Fatalf("local_shell call not emitted: %s", out)
	}
	// The client echoes optional fields as null, drops the empty env and
	// answers with a spec-shaped output that names the call item by id only.
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},` +
		`{"type":"local_shell_call","id":"lsh_item","call_id":"c-l","status":"completed","action":{"type":"exec","command":["pwd"],"timeout_ms":null,"working_directory":null,"user":null}},` +
		`{"type":"local_shell_call_output","id":"lsh_item","status":"incomplete","output":"/repo"}]`
	next, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,"tools":[{"type":"local_shell"}]}`, replay)
	raw, _ := json.Marshal(next)
	if !strings.Contains(string(raw), `"id":"fc_c-l"`) {
		t.Fatalf("echoed call with nulls did not replay the exact item: %s", raw)
	}
	// preparedBody fails on an unpaired output, so reaching here proves the
	// id-only output found its call; check the status survived rendering.
	if !strings.Contains(string(raw), `(command incomplete)\n/repo`) {
		t.Fatalf("id-only output status was not rendered: %s", raw)
	}
	if text(builtinOutputText(object{"type": "shell_call_output", "output": []any{object{"outcome": object{"type": "exit"}}}})) != "Command 1 exited without an exit code" {
		t.Fatal("missing exit_code rendered badly")
	}
}

func TestAllowedToolsNamespacedEntryDoesNotAllowOtherNamespaces(t *testing.T) {
	raw := `{"model":"gpt-5.5","input":"go","tools":[` +
		`{"type":"namespace","name":"github","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]},` +
		`{"type":"namespace","name":"jira","tools":[{"type":"function","name":"search","parameters":{"type":"object"}}]}],` +
		`"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","namespace":"github","name":"search"}]}}`
	_, bridge := preparedBody(t, raw, &ReplayCache{})
	if _, ok := bridge.tools["github.search"]; !ok {
		t.Fatal("allowed namespaced tool was removed")
	}
	if _, ok := bridge.tools["jira.search"]; ok {
		t.Fatal("same-named tool in another namespace was allowed")
	}
}

func rawTransportCall(callID, summary, code string) string {
	args, _ := json.Marshal(map[string]any{"code": code, "summary": summary, "extended_summary": "e", "destructive": false, "references": []any{}})
	quotedArgs, _ := json.Marshal(string(args))
	return `{"type":"function_call","id":"fc_` + callID + `","call_id":"` + callID + `","name":"run_officejs","arguments":` + string(quotedArgs) + `}`
}

const execCommandTools = `"tools":[` +
	`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"},"workdir":{"type":"string"}},"required":["cmd"]}},` +
	`{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}},` +
	`{"type":"shell"}]`

func TestRawFieldTransportRelaysCommandsWithoutNestedEscaping(t *testing.T) {
	replay := &ReplayCache{}
	body, bridge, err := Prepare([]byte(`{"model":"gpt-5.5","input":"search",`+execCommandTools+`}`), "account:1", replay)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, want := range []string{`codex2api.raw/exec_command/cmd`, `codex2api.raw/shell/commands`, "escaping every double quote and backslash", "Transport rules:"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("prompt is missing %q: %s", want, body)
		}
	}
	if strings.Contains(string(body), "codex2api.raw/lookup") || strings.Contains(string(body), catalogRawFieldKey) {
		t.Fatalf("ineligible tool got a raw transport or the marker leaked: %s", body)
	}

	command := `rg "foo bar" src\path`
	stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
		rawTransportCall("c-raw", "codex2api.raw/exec_command/cmd", command) + "," +
		rawTransportCall("c-sh", "codex2api.raw/shell/commands", `echo "hi"`) + "]}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream)))
	wantArgs, _ := json.Marshal(map[string]string{"cmd": command})
	quotedWant, _ := json.Marshal(string(wantArgs))
	if !strings.Contains(out, `"arguments":`+string(quotedWant)) {
		t.Fatalf("raw command was not relayed verbatim (want %s): %s", quotedWant, out)
	}
	if !strings.Contains(out, `"type":"shell_call"`) || !strings.Contains(out, `"commands":["echo \"hi\""]`) {
		t.Fatalf("raw shell command was not relayed as one commands entry: %s", out)
	}

	// The exact raw-transport item replays on the next turn.
	history := `[{"type":"message","role":"user","content":[{"type":"input_text","text":"search"}]},` +
		`{"type":"function_call","call_id":"c-raw","name":"exec_command","arguments":` + string(quotedWant) + `}]`
	next, _ := preparedBody(t, `{"model":"gpt-5.5","input":`+history+`,`+execCommandTools+`}`, replay)
	if raw, _ := json.Marshal(next); !strings.Contains(string(raw), `"id":"fc_c-raw"`) || !strings.Contains(string(raw), "codex2api.raw/exec_command/cmd") {
		t.Fatalf("raw transport call was not replayed exactly: %s", raw)
	}
}

func TestRawFieldTransportRejectsUndeclaredFieldsAndTools(t *testing.T) {
	for _, summary := range []string{"codex2api.raw/exec_command/workdir", "codex2api.raw/lookup/a", "codex2api.raw/missing/cmd", "codex2api.raw/exec_command"} {
		_, bridge, err := Prepare([]byte(`{"model":"gpt-5.5","input":"go",`+execCommandTools+`}`), "account:1", &ReplayCache{})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		stream := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" +
			rawTransportCall("c-bad", summary, "pwd") + "]}}\n\n"
		if out := readStream(t, bridge, io.NopCloser(strings.NewReader(stream))); !strings.Contains(out, `"type":"response.failed"`) {
			t.Fatalf("raw transport %q was relayed: %s", summary, out)
		}
	}
}

func TestStreamFailsWhenOnlyCommentaryRemainsAfterDroppingCalls(t *testing.T) {
	bridge := &Bridge{Effort: "medium", replay: &ReplayCache{}, tools: map[string]tool{}, unsupportedTools: map[string]bool{}}
	preamble := `{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"I'll run the tests now."}]}`
	call := `{"type":"function_call","id":"fc_1","call_id":"c","name":"excel_native","arguments":"{}"}`
	completed := func(output string) string {
		return "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[" + output + "]}}\n\n"
	}
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(completed(preamble+","+call))))
	if !strings.Contains(out, `"type":"response.failed"`) || strings.Contains(out, `"type":"response.completed"`) {
		t.Fatalf("commentary alone ended the turn after its call was dropped: %s", out)
	}
	answer := strings.Replace(preamble, `"phase":"commentary"`, `"phase":"final_answer"`, 1)
	out = readStream(t, bridge, io.NopCloser(strings.NewReader(completed(answer+","+call))))
	if !strings.Contains(out, `"type":"response.completed"`) {
		t.Fatalf("a final answer with a dropped call should still complete: %s", out)
	}
}

func TestRewriteImagesReportsOnlyLatestTurnUploadFailures(t *testing.T) {
	failing := func(InlineImage) (string, error) { return "", io.ErrUnexpectedEOF }
	image := `{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}`
	history := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[` + image + `]},{"type":"message","role":"user","content":[{"type":"input_text","text":"and now?"}]}]}`
	current := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"user","content":[` + image + `]}]}`
	both := `{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[` + image + `]},{"type":"message","role":"user","content":[` + image + `]}]}`
	for name, tc := range map[string]struct {
		raw     string
		current bool
	}{"history only": {history, false}, "latest turn": {current, true}, "history failure skips latest": {both, true}} {
		prepared, _, err := Prepare([]byte(tc.raw), "account:1", &ReplayCache{})
		if err != nil {
			t.Fatalf("%s: Prepare: %v", name, err)
		}
		_, report, err := RewriteImages(prepared, ImagesDefault, failing)
		if err != nil || report.UploadErr == nil || (report.CurrentUploadErr != nil) != tc.current {
			t.Fatalf("%s: report = %+v, %v", name, report, err)
		}
	}
}

func TestRewriteImagesKeepsUploadingAfterAnInvalidHistoryImage(t *testing.T) {
	raw := `{"model":"gpt-5.5","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/x\u0000y;base64,AAAA"}]},` +
		`{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]}`
	prepared, _, err := Prepare([]byte(raw), "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	uploads := 0
	upload := func(image InlineImage) (string, error) {
		if _, _, err := image.Decode(); err != nil {
			return "", err
		}
		uploads++
		return "file-latest", nil
	}
	out, report, err := RewriteImages(prepared, ImagesDefault, upload)
	if err != nil || uploads != 1 || report.CurrentUploadErr != nil || report.UploadErr != nil || !errors.Is(report.InputErr, ErrInvalidImage) {
		t.Fatalf("report = %+v, uploads=%d, err=%v", report, uploads, err)
	}
	if !strings.Contains(string(out), `"file_id":"file-latest"`) || !strings.Contains(string(out), "the image data is invalid") {
		t.Fatalf("latest image not uploaded or history image not noted: %s", out)
	}
}
