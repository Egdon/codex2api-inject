package basispoints

import (
	"encoding/json"
	"testing"
)

func TestPreparePlaintextAgentEncryptedField(t *testing.T) {
	for _, textValue := range []string{"Reply exactly BPS_AGENT_PROBE_OK.", "只做检查并返回结果。", "Return OK"} {
		source := object{"model": "gpt-6-astra", "input": []any{object{"type": "agent_message", "id": "amsg_test", "author": "parent", "recipient": "child", "content": []any{object{"type": "input_text", "text": "task:"}, object{"type": "encrypted_content", "encrypted_content": textValue}}, "internal_chat_message_metadata_passthrough": object{"turn_id": "private"}}}}
		raw, _ := json.Marshal(source)
		wire, _, err := Prepare(raw, t.Name(), &ReplayCache{})
		if err != nil {
			t.Fatal(err)
		}
		var output object
		json.Unmarshal(wire, &output)
		items := output["input"].([]any)
		agent := items[len(items)-1].(object)
		if agent["type"] != "agent_message" || agent["author"] != "parent" || agent["recipient"] != "child" || agent["id"] != "amsg_test" {
			t.Fatal("agent identity changed")
		}
		part := agent["content"].([]any)[1].(object)
		if part["type"] != "input_text" || part["text"] != textValue {
			t.Fatal("plaintext task lost or not normalized")
		}
		if _, has := part["encrypted_content"]; has {
			t.Fatal("plaintext still marked encrypted")
		}
	}
}

func TestPreparePreservesOpaqueAgentContext(t *testing.T) {
	for _, value := range []string{"gAAAAnative", "YWJjZA==", "opaque-synthetic", "YWJj\r\nZA=="} {
		raw, _ := json.Marshal(object{"model": "gpt-6-astra", "input": []any{object{"type": "agent_message", "id": "amsg_test", "author": "parent", "recipient": "child", "content": []any{object{"type": "encrypted_content", "encrypted_content": value}}}}})
		_, _, err := Prepare(raw, t.Name(), &ReplayCache{})
		if err == nil {
			t.Fatal("opaque context must stay unsupported by BPS adapter")
		}
	}
}
