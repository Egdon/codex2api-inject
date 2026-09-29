package basispoints

import (
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestPrepareAcceptsRedeclaredToolsThatDifferOnlyInDescription(t *testing.T) {
	request := `{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"},{"type":"additional_tools","tools":[{"type":"function","name":"lookup","description":"newer text","defer_loading":true,"parameters":{"type":"object"}}]}],"tools":[{"type":"function","name":"lookup","description":"older text","parameters":{"type":"object"}}]}`
	if _, _, err := Prepare([]byte(request), "account:1", &ReplayCache{}); err != nil {
		t.Fatalf("annotation-only redeclaration was rejected: %v", err)
	}
	conflicting := `{"model":"gpt-6-astra","input":[{"role":"user","content":"hi"},{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","required":["q"]}}]}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`
	if _, _, err := Prepare([]byte(conflicting), "account:1", &ReplayCache{}); err == nil {
		t.Fatal("a changed call contract must still be rejected")
	}
}

func TestPrepareRebuildsUncachedHistoryInTheCatalogTransport(t *testing.T) {
	request := `{"model":"gpt-6-astra","tools":[
		{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
		{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"},"workdir":{"type":"string"}},"required":["cmd"]}}
	],"input":[
		{"role":"user","content":"edit and run"},
		{"type":"custom_tool_call","call_id":"call_patch","name":"apply_patch","input":"*** Begin Patch\n*** End Patch"},
		{"type":"custom_tool_call_output","call_id":"call_patch","output":"ok"},
		{"type":"function_call","call_id":"call_raw","name":"exec_command","arguments":"{\"cmd\":\"echo \\\"hi\\\"\"}"},
		{"type":"function_call_output","call_id":"call_raw","output":"hi"},
		{"type":"function_call","call_id":"call_json","name":"exec_command","arguments":"{\"cmd\":\"ls\",\"workdir\":\"/tmp\"}"},
		{"type":"function_call_output","call_id":"call_json","output":"a"}
	]}`
	body, _, err := Prepare([]byte(request), "account:1", &ReplayCache{})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	transport := func(callID string) gjson.Result {
		t.Helper()
		for _, item := range gjson.GetBytes(body, "input").Array() {
			if item.Get("type").String() == "function_call" && item.Get("call_id").String() == callID {
				if item.Get("name").String() != "run_officejs" {
					t.Fatalf("%s was not rebuilt as run_officejs: %s", callID, item.Raw)
				}
				return gjson.Parse(item.Get("arguments").String())
			}
		}
		t.Fatalf("rebuilt call %s missing", callID)
		return gjson.Result{}
	}
	if args := transport("call_patch"); args.Get("summary").String() != "codex2api.custom/apply_patch" || args.Get("code").String() != "*** Begin Patch\n*** End Patch" {
		t.Fatalf("custom call history = %s", args.Raw)
	}
	if args := transport("call_raw"); args.Get("summary").String() != "codex2api.raw/exec_command/cmd" || args.Get("code").String() != `echo "hi"` {
		t.Fatalf("raw-field call history = %s", args.Raw)
	}
	// Optional arguments cannot travel in the raw form; keep the JSON envelope.
	if args := transport("call_json"); strings.HasPrefix(args.Get("summary").String(), "codex2api.") || gjson.Get(args.Get("code").String(), "arguments.workdir").String() != "/tmp" {
		t.Fatalf("multi-argument call history = %s", args.Raw)
	}
}

func TestStripReasoningEncryptedContent(t *testing.T) {
	prepared := `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","summary":[],"encrypted_content":"opaque"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`
	stripped, ok := StripReasoningEncryptedContent([]byte(prepared))
	if !ok || strings.Contains(string(stripped), "opaque") || len(gjson.GetBytes(stripped, "input").Array()) != 2 {
		t.Fatalf("strip = %s, %t", stripped, ok)
	}
	if _, ok := StripReasoningEncryptedContent([]byte(`{"input":[{"type":"message","role":"user","content":"hi"}]}`)); ok {
		t.Fatal("nothing to strip must not produce a retry")
	}
	other := `{"input":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"message","role":"user","content":[{"type":"encrypted_content","encrypted_content":"other"}]}]}`
	if _, ok := StripReasoningEncryptedContent([]byte(other)); ok {
		t.Fatal("other opaque content must block the retry")
	}
}
