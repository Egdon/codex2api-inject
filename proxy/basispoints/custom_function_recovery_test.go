package basispoints

import (
	"encoding/json"
	"testing"
)

func TestFunctionMistaggedAsCustomTransport(t *testing.T) {
	for _, code := range []string{`{"task_name":"probe","message":"return ok"}`, `{"name":"collaboration.spawn_agent","arguments":{"task_name":"probe","message":"return ok"}}`} {
		b := &Bridge{tools: map[string]tool{"collaboration.spawn_agent": {Name: "spawn_agent", Namespace: "collaboration", Kind: "function"}}, replay: &ReplayCache{}, scope: t.Name()}
		out, err := b.translateCall(object{"type": "function_call", "call_id": "call-probe", "name": "run_officejs", "arguments": object{"summary": "codex2api.custom/collaboration.spawn_agent", "code": code}})
		if err != nil {
			t.Fatal(err)
		}
		if out["type"] != "function_call" || out["name"] != "spawn_agent" || out["namespace"] != "collaboration" {
			t.Fatalf("wrong tool identity: %v", out)
		}
		var args object
		if err := json.Unmarshal([]byte(text(out["arguments"])), &args); err != nil {
			t.Fatal(err)
		}
		if args["task_name"] != "probe" || args["message"] != "return ok" {
			t.Fatalf("arguments changed: %v", args)
		}
	}
}
func TestFunctionMistaggedAsCustomTransportRejectsInvalidInputs(t *testing.T) {
	for _, code := range []string{"await exec()", `[]`, `null`, `{"task_name":"probe"} {"message":"second call"}`, `{"name":"other_tool","arguments":{"message":"wrong target"}}`} {
		b := &Bridge{tools: map[string]tool{"collaboration.spawn_agent": {Name: "spawn_agent", Namespace: "collaboration", Kind: "function"}}, replay: &ReplayCache{}, scope: t.Name()}
		_, err := b.translateCall(object{"type": "function_call", "call_id": "call-probe", "name": "run_officejs", "arguments": object{"summary": "codex2api.custom/collaboration.spawn_agent", "code": code}})
		if err == nil {
			t.Fatal("invalid or ambiguous input accepted")
		}
	}
}
