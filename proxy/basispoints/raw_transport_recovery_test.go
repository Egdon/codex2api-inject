package basispoints

import (
	"encoding/json"
	"testing"
)

func TestRawTransportRecoversExactUnambiguousTool(t *testing.T) {
	for _, kind := range []string{"function", "custom"} {
		b := &Bridge{tools: map[string]tool{"client.run": {Name: "run", Namespace: "client", Kind: kind, RawField: map[string]string{"function": "code"}[kind]}}, replay: &ReplayCache{}, scope: t.Name()}
		out, err := b.translateCall(object{"type": "function_call", "call_id": "raw-probe", "name": "run_officejs", "arguments": object{"summary": "codex2api.raw/client.run", "code": "return 'quoted raw text'"}})
		if err != nil {
			t.Fatal(err)
		}
		if out["name"] != "run" || out["namespace"] != "client" || out["call_id"] != "raw-probe" {
			t.Fatal("call identity changed")
		}
		if kind == "custom" {
			if out["input"] != "return 'quoted raw text'" {
				t.Fatal("raw custom text changed")
			}
		} else {
			var args object
			json.Unmarshal([]byte(text(out["arguments"])), &args)
			if len(args) != 1 || args["code"] != "return 'quoted raw text'" {
				t.Fatal("raw function text changed")
			}
		}
	}
}

func TestRawTransportRejectsAmbiguousOrUndeclaredTargets(t *testing.T) {
	b := &Bridge{tools: map[string]tool{"client.run": {Name: "run", Namespace: "client", Kind: "function", RawField: "code"}, "client.multi": {Name: "multi", Kind: "function"}}, replay: &ReplayCache{}}
	for _, summary := range []string{"codex2api.raw/client.missing", "codex2api.raw/client.multi", "codex2api.raw/client.run/", "codex2api.raw/client.run/wrong", "codex2api.raw/client.run/extra/code", "codex2api.raw/client.run more"} {
		if _, err := b.translateCall(object{"type": "function_call", "call_id": "reject-probe", "name": "run_officejs", "arguments": object{"summary": summary, "code": "content"}}); err == nil {
			t.Fatalf("accepted ambiguous marker %s", summary)
		}
	}
}

func TestAgentNormalizationPreservesNonAgentAndConflictingParts(t *testing.T) {
	for _, kind := range []string{"message", "function_call_output", "agent_message"} {
		part := object{"type": "encrypted_content", "encrypted_content": "Return OK"}
		if kind == "agent_message" {
			part["text"] = "conflicting text"
		}
		item := object{"type": kind, "content": []any{part}}
		if NormalizeAgentMessage(item) || part["type"] != "encrypted_content" {
			t.Fatal("unrelated or conflicting content changed")
		}
	}
}
