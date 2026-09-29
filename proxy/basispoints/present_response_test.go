package basispoints

import (
	"io"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestStreamReportsClientToolsInsteadOfExcelConfiguration(t *testing.T) {
	request := `{"model":"gpt-6-astra","input":"hi","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`
	_, bridge, err := Prepare([]byte(request), "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	excel := `"instructions":"excel server prompt","input":[{"role":"developer","content":"protocol"}],"tools":[{"type":"function","name":"run_officejs"}]`
	upstream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"status\":\"in_progress\",\"output\":[]," + excel + "}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}]," + excel + "}}\n\n"
	out := readStream(t, bridge, io.NopCloser(strings.NewReader(upstream)))
	if strings.Contains(out, "excel server prompt") || strings.Contains(out, "run_officejs") || strings.Contains(out, `"protocol"`) {
		t.Fatalf("Excel server configuration reached the client: %s", out)
	}
	for _, frame := range strings.Split(out, "\n") {
		data, ok := strings.CutPrefix(frame, "data: ")
		if !ok || !gjson.Get(data, "response").Exists() {
			continue
		}
		if name := gjson.Get(data, "response.tools.0.name").String(); name != "get_weather" {
			t.Fatalf("response tools = %s", gjson.Get(data, "response.tools").Raw)
		}
	}
}
