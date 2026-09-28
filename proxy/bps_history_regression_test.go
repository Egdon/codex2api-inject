package proxy

import (
	"encoding/json"
	"testing"
)

func TestBPSReasoningReplayOmitsDisplayContent(t *testing.T) {
	var input any
	if err := json.Unmarshal([]byte(`[{"type":"reasoning","id":"rs_probe","encrypted_content":"gAAAAnative","summary":[{"type":"summary_text","text":"visible summary"}],"content":[{"type":"reasoning_text","text":"visible summary"}],"status":"completed"},{"role":"user","content":[{"type":"input_text","text":"keep user content"}]}]`), &input); err != nil {
		t.Fatal(err)
	}
	cleaned, changed, keep := dropBareReasoningInputValue(input)
	if !changed || !keep {
		t.Fatal("expected cleaned reasoning history")
	}
	items := cleaned.([]any)
	reasoning := items[0].(map[string]any)
	if _, exists := reasoning["content"]; exists {
		t.Fatal("display-only reasoning.content is still replayed")
	}
	if _, exists := reasoning["status"]; exists {
		t.Fatal("status is still replayed")
	}
	if reasoning["encrypted_content"] != "gAAAAnative" || len(reasoning["summary"].([]any)) != 1 {
		t.Fatal("reasoning context lost")
	}
	if len(items[1].(map[string]any)["content"].([]any)) != 1 {
		t.Fatal("user content lost")
	}
}

func TestBPSPlaintextAgentContextStaysOnBPS(t *testing.T) {
	for _, raw := range []string{`{"input":[{"type":"agent_message","content":[{"type":"input_text","text":"plain task"}]}]}`, `{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"Reply exactly BPS_AGENT_PROBE_OK."}]}]}`} {
		if reason := excelBPSNativeRequestReason([]byte(raw)); reason != "" {
			t.Fatalf("plaintext context unnecessarily switched upstream: %s", reason)
		}
	}
}

func TestPrepareResponsesBodyNormalizesPlaintextAgentContext(t *testing.T) {
	raw := []byte(`{"model":"gpt-6-astra","input":[{"type":"agent_message","id":"amsg_probe","author":"parent","recipient":"child","content":[{"type":"encrypted_content","encrypted_content":"Return OK"}]}]}`)
	got, _ := PrepareResponsesBody(raw)
	var body map[string]any
	json.Unmarshal(got, &body)
	agent := body["input"].([]any)[0].(map[string]any)
	part := agent["content"].([]any)[0].(map[string]any)
	if part["type"] != "input_text" || part["text"] != "Return OK" {
		t.Fatal("native fallback still receives plaintext as encrypted")
	}
}
