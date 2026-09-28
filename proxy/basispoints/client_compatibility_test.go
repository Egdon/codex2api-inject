package basispoints

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestClientFunctionEncryptionContract(t *testing.T) {
	for _, direct := range []bool{false, true} {
		for _, task := range []string{"OK", "检查", "Preserve quotes: \"text\"\n\tand whitespace"} {
			b := &Bridge{tools: map[string]tool{"collaboration.spawn_agent": {Name: "spawn_agent", Namespace: "collaboration", Kind: "function"}}, replay: &ReplayCache{}, scope: t.Name()}
			args := object{"message": task, "task_name": "probe"}
			native := object{"type": "function_call", "id": "fc_probe", "call_id": "call_probe", "name": "collaboration.spawn_agent", "arguments": args}
			if !direct {
				code, _ := json.Marshal(object{"name": "collaboration.spawn_agent", "arguments": args})
				native["name"] = "run_officejs"
				native["arguments"] = object{"code": string(code)}
				native["encrypted_function_args"] = []any{"code"}
			}
			var wire strings.Builder
			for _, event := range []object{
				{"type": "response.created", "response": object{"id": "resp_probe", "output": []any{}}},
				{"type": "response.output_item.done", "output_index": 0, "item": native},
				{"type": "response.completed", "response": object{"id": "resp_probe", "status": "completed", "output": []any{native}}},
			} {
				raw, _ := json.Marshal(event)
				wire.WriteString("data: " + string(raw) + "\n\n")
			}
			stream := b.Stream(io.NopCloser(strings.NewReader(wire.String())))
			out, err := io.ReadAll(stream)
			stream.Close()
			if err != nil {
				t.Fatal(err)
			}
			checked := 0
			verify := func(item object) {
				metadata, err := json.Marshal(item["encrypted_function_args"])
				if err != nil || string(metadata) != "[]" {
					t.Errorf("missing explicit plaintext metadata: %s", metadata)
				}
				checked++
				if text(item["arguments"]) != "" {
					var got object
					if json.Unmarshal([]byte(text(item["arguments"])), &got) != nil || got["message"] != task {
						t.Error("task contents changed")
					}
				}
			}
			err = readEvents(strings.NewReader(string(out)), func(_ string, data []byte) error {
				var event object
				if err := decode(data, &event); err != nil {
					return err
				}
				if item, ok := event["item"].(object); ok && text(item["type"]) == "function_call" {
					verify(item)
				}
				if text(event["type"]) == "response.completed" {
					response := event["response"].(object)
					for _, item := range response["output"].([]any) {
						verify(item.(object))
					}
				}
				return nil
			})
			if err != nil || checked != 3 {
				t.Fatalf("incomplete stream contract: checked=%d err=%v", checked, err)
			}
		}
	}
}

func TestDirectFunctionRetainsDeclaredEncryption(t *testing.T) {
	b := &Bridge{tools: map[string]tool{"collaboration.spawn_agent": {Name: "spawn_agent", Namespace: "collaboration", Kind: "function"}}, replay: &ReplayCache{}}
	original := object{"type": "function_call", "name": "collaboration.spawn_agent", "call_id": "call_opaque", "arguments": object{"message": "gAAAAopaque"}, "encrypted_function_args": []any{"message"}}
	out, err := b.translateCall(original)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := json.Marshal(out["encrypted_function_args"])
	if string(metadata) != "[\"message\"]" {
		t.Fatal("native encryption declaration lost")
	}
	var args object
	if json.Unmarshal([]byte(text(out["arguments"])), &args) != nil || args["message"] != "gAAAAopaque" {
		t.Fatal("opaque argument changed")
	}
}

func TestClientImageDetailOriginalNormalizesWithoutChangingImage(t *testing.T) {
	const imageURL = "data:image/png;base64,iVBORw0KGgo="
	for _, toolOutput := range []bool{false, true} {
		for _, detail := range []string{"original", "auto", "low", "high"} {
			part := object{"type": "input_image", "image_url": imageURL, "detail": detail}
			input := []any{object{"type": "message", "role": "user", "content": []any{part}}}
			if toolOutput {
				input = []any{object{"type": "function_call", "call_id": "call_image", "name": "view", "arguments": object{}}, object{"type": "function_call_output", "call_id": "call_image", "output": []any{part}}}
			}
			raw, _ := json.Marshal(object{"model": "gpt-6-astra", "input": input})
			prepared, _, err := Prepare(raw, t.Name(), &ReplayCache{})
			if err != nil {
				t.Errorf("tool=%v detail=%s: %v", toolOutput, detail, err)
				continue
			}
			var body object
			if decode(prepared, &body) != nil {
				t.Fatal("invalid prepared request")
			}
			items := body["input"].([]any)
			item := items[len(items)-1].(object)
			field := "content"
			if toolOutput {
				field = "output"
			}
			result := item[field].([]any)[0].(object)
			want := detail
			if detail == "original" {
				want = "high"
			}
			if result["detail"] != want || result["image_url"] != imageURL {
				t.Fatal("image detail or bytes changed incorrectly")
			}
		}
	}
	for _, invalid := range []any{"ultra", "HIGH", true, 42} {
		if validateImage(object{"type": "input_image", "image_url": imageURL, "detail": invalid}) == nil {
			t.Fatal("unknown detail accepted")
		}
	}
}
