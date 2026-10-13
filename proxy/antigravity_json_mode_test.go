package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// antigravityJSONModeAnswer is the generateContent body Cloud Code returns for
// claude-sonnet-4-6 with a responseSchema (captured 2026-10-13).
const antigravityJSONModeAnswer = `{"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"json_mode","args":{"name":"Bob","age":3},"id":"toolu_vrtx_016mbm6gz5jkPqHrr2TJsfwK"}}]},"finishReason":"OTHER"}],"usageMetadata":{"promptTokenCount":680,"candidatesTokenCount":50,"totalTokenCount":730},"modelVersion":"claude-sonnet-4-6"},"traceId":"327ba94bf7b2817d","metadata":{}}`

const antigravityJSONModeResponsesBody = `{"input":"Make up a person named Bob aged 3.","text":{"format":{"type":"json_schema","name":"person","schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}}}}`

func antigravityJSONModeTestServer(t *testing.T, status int, generateBody, streamBody string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, streamBody)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, generateBody)
	}))
	t.Cleanup(server.Close)
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
}

func antigravityJSONModeTestAccount() *auth.Account {
	return &auth.Account{DBID: 783, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "google-project"}
}

func TestAntigravityRewriteJSONModeFrameUnwrapsStructuredAnswer(t *testing.T) {
	out, toolCalled := antigravityRewriteJSONModeFrame([]byte(antigravityJSONModeAnswer), false)
	if !toolCalled {
		t.Fatal("json_mode call was not reported as a tool call")
	}
	candidate := gjson.GetBytes(out, "response.candidates.0")
	if got := candidate.Get("finishReason").String(); got != "STOP" {
		t.Fatalf("finishReason = %q, want STOP; frame=%s", got, out)
	}
	parts := candidate.Get("content.parts").Array()
	if len(parts) != 1 || parts[0].Get("functionCall").Exists() {
		t.Fatalf("parts = %s", candidate.Get("content.parts").Raw)
	}
	// The model's key order survives: decoding the args into a map would sort them.
	if got := parts[0].Get("text").String(); got != `{"name":"Bob","age":3}` {
		t.Fatalf("text = %q", got)
	}
	if got := gjson.GetBytes(out, "response.usageMetadata.totalTokenCount").Int(); got != 730 {
		t.Fatalf("usage lost: %s", out)
	}
}

func TestAntigravityRewriteJSONModeFrameDropsPreambleKeepsThoughts(t *testing.T) {
	frame := `{"candidates":[{"content":{"parts":[{"text":"weighing it","thought":true},{"text":"Here is the JSON:"},{"functionCall":{"name":"json_mode","args":"{\"ok\":true}"}}]},"finishReason":"OTHER"}]}`
	out, _ := antigravityRewriteJSONModeFrame([]byte(frame), false)
	parts := gjson.GetBytes(out, "candidates.0.content.parts").Array()
	if len(parts) != 2 {
		t.Fatalf("parts = %s", gjson.GetBytes(out, "candidates.0.content.parts").Raw)
	}
	if !parts[0].Get("thought").Bool() || parts[0].Get("text").String() != "weighing it" {
		t.Fatalf("thought part changed: %s", parts[0].Raw)
	}
	if got := parts[1].Get("text").String(); got != `{"ok":true}` {
		t.Fatalf("string args text = %q", got)
	}
}

func TestAntigravityRewriteJSONModeFrameCompletesClientToolCall(t *testing.T) {
	// With a responseSchema the backend reports a call to the client's own
	// tool as OTHER as well (captured 2026-10-13).
	frame := `{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"I'll start by fetching the weather for Paris right away!"},{"functionCall":{"name":"get_weather","args":{"city":"Paris"},"id":"toolu_1"}}]},"finishReason":"OTHER"}]}}`
	out, toolCalled := antigravityRewriteJSONModeFrame([]byte(frame), false)
	if !toolCalled {
		t.Fatal("client tool call was not reported")
	}
	candidate := gjson.GetBytes(out, "response.candidates.0")
	if got := candidate.Get("finishReason").String(); got != "STOP" {
		t.Fatalf("finishReason = %q, want STOP", got)
	}
	if got := candidate.Get("content.parts.1.functionCall.name").String(); got != "get_weather" {
		t.Fatalf("client tool call changed: %s", candidate.Raw)
	}
	if got := candidate.Get("content.parts.0.text").String(); !strings.HasPrefix(got, "I'll start") {
		t.Fatalf("text next to a client tool call must stay: %s", candidate.Raw)
	}
}

func TestAntigravityRewriteJSONModeFrameLeavesOtherWithoutToolCall(t *testing.T) {
	frame := []byte(`{"candidates":[{"content":{"parts":[{"text":"partial"}]},"finishReason":"OTHER"}]}`)
	out, toolCalled := antigravityRewriteJSONModeFrame(frame, false)
	if toolCalled || string(out) != string(frame) {
		t.Fatalf("frame without a tool call was rewritten: %s", out)
	}
}

func TestAntigravityRewriteJSONModeFrameCarriesToolCallAcrossFrames(t *testing.T) {
	first, toolCalled := antigravityRewriteJSONModeFrame([]byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"json_mode","args":{"a":1}}}]}}]}}`), false)
	if got := gjson.GetBytes(first, "response.candidates.0.content.parts.0.text").String(); got != `{"a":1}` {
		t.Fatalf("first frame text = %q", got)
	}
	last, _ := antigravityRewriteJSONModeFrame([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":""}]},"finishReason":"OTHER"}]}}`), toolCalled)
	if got := gjson.GetBytes(last, "response.candidates.0.finishReason").String(); got != "STOP" {
		t.Fatalf("finishReason after an earlier call = %q, want STOP", got)
	}
}

func TestAntigravityClaudeJSONModeRequest(t *testing.T) {
	schema := `"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"object"}}`
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"claude with schema", `{"model":"claude-sonnet-4-6","request":{` + schema + `}}`, true},
		{"snake case schema", `{"model":"claude-opus-4-6-thinking","request":{"generation_config":{"response_schema":{"type":"object"}}}}`, true},
		{"claude json_object only", `{"model":"claude-sonnet-4-6","request":{"generationConfig":{"responseMimeType":"application/json"}}}`, false},
		{"gemini with schema", `{"model":"gemini-3-flash","request":{` + schema + `}}`, false},
		{"client declares json_mode", `{"model":"claude-sonnet-4-6","request":{` + schema + `,"tools":[{"functionDeclarations":[{"name":"lookup"},{"name":"json_mode"}]}]}}`, false},
		{"other client tools", `{"model":"claude-sonnet-4-6","request":{` + schema + `,"tools":[{"functionDeclarations":[{"name":"lookup"}]}]}}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := antigravityClaudeJSONModeRequest([]byte(tc.payload)); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAntigravityResponsesClaudeStructuredOutputNonStream(t *testing.T) {
	antigravityJSONModeTestServer(t, http.StatusOK, antigravityJSONModeAnswer, "")
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(antigravityJSONModeResponsesBody), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	result := gjson.ParseBytes(body)
	if got := result.Get("status").String(); got != "completed" {
		t.Fatalf("status = %q; body=%s", got, body)
	}
	if got := result.Get("output_text").String(); got != `{"name":"Bob","age":3}` {
		t.Fatalf("output_text = %q", got)
	}
	output := result.Get("output").Array()
	if len(output) != 1 || output[0].Get("type").String() != "message" || output[0].Get("content.0.text").String() != `{"name":"Bob","age":3}` {
		t.Fatalf("output = %s", result.Get("output").Raw)
	}
	if result.Get("error").Exists() {
		t.Fatalf("unexpected error: %s", body)
	}
}

func TestAntigravityResponsesClaudeWithoutStructuredOutputKeepsOtherAsFailure(t *testing.T) {
	antigravityJSONModeTestServer(t, http.StatusOK, antigravityJSONModeAnswer, "")
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(`{"input":"hello"}`), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got := gjson.GetBytes(body, "status").String(); got != "failed" {
		t.Fatalf("status = %q, want failed; body=%s", got, body)
	}
}

func TestAntigravityResponsesClaudeStructuredOutputStreams(t *testing.T) {
	cases := []struct {
		name     string
		buffered bool
	}{
		{"stream", false},
		{"buffered", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The streaming frames split the call and the finish reason, the
			// buffered path replays the whole generateContent answer at once.
			antigravityJSONModeTestServer(t, http.StatusOK, antigravityJSONModeAnswer,
				`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"json_mode","args":{"name":"Bob","age":3}}}]}}]}}`+"\n\n"+
					`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"OTHER"}],"usageMetadata":{"promptTokenCount":680,"candidatesTokenCount":50,"totalTokenCount":730}}}`+"\n\n")
			var resp *http.Response
			var err error
			if tc.buffered {
				resp, err = ExecuteAntigravityResponsesRequestBuffered(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(antigravityJSONModeResponsesBody), "")
			} else {
				resp, err = ExecuteAntigravityResponsesRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(antigravityJSONModeResponsesBody), true, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			events := readAntigravityTestEvents(t, resp.Body)
			if len(events) == 0 {
				t.Fatal("no events")
			}
			terminal := events[len(events)-1]
			if got := terminal.Get("type").String(); got != "response.completed" {
				t.Fatalf("terminal event = %s", terminal.Raw)
			}
			if got := terminal.Get("response.output_text").String(); got != `{"name":"Bob","age":3}` {
				t.Fatalf("output_text = %q", got)
			}
			for _, event := range events {
				if strings.HasPrefix(event.Get("type").String(), "response.function_call") {
					t.Fatalf("json_mode leaked as a function call: %s", event.Raw)
				}
			}
		})
	}
}

func TestAntigravityResponsesClaudeStructuredOutputRejectionIsClear(t *testing.T) {
	upstream := `{"error":{"code":400,"message":"{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"tool_choice: type \\\"tool\\\" and \\\"any\\\" are not supported for this model.\"},\"request_id\":\"req_vrtx_1\"}","status":"INVALID_ARGUMENT"}}`
	antigravityJSONModeTestServer(t, http.StatusBadRequest, upstream, upstream)
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-5-5-low", []byte(antigravityJSONModeResponsesBody), true, "")
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("got a response, want an error: status %d", resp.StatusCode)
	}
	var gatewayErr *Error
	if !errors.As(err, &gatewayErr) {
		t.Fatalf("err = %v", err)
	}
	if gatewayErr.HTTPStatus != http.StatusBadRequest || gatewayErr.Type != ErrorTypeInvalidRequest || gatewayErr.Retryable {
		t.Fatalf("error = %#v", gatewayErr)
	}
	if !strings.Contains(gatewayErr.Message, "claude-sonnet-5-5-low does not support json_schema structured output") ||
		!strings.Contains(gatewayErr.Message, `tool_choice: type "tool" and "any" are not supported`) {
		t.Fatalf("message = %q", gatewayErr.Message)
	}
}

func TestAntigravityResponsesClaudeStructuredOutputOtherBadRequestPassesThrough(t *testing.T) {
	upstream := `{"error":{"code":400,"message":"prompt is too long","status":"INVALID_ARGUMENT"}}`
	antigravityJSONModeTestServer(t, http.StatusBadRequest, upstream, upstream)
	resp, err := ExecuteAntigravityResponsesRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(antigravityJSONModeResponsesBody), false, "")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest || string(body) != upstream {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
}

func TestExecuteAntigravityGeminiRequestClaudeStructuredOutput(t *testing.T) {
	request := `{"contents":[{"role":"user","parts":[{"text":"Make up Bob"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"OBJECT","properties":{"name":{"type":"STRING"},"age":{"type":"INTEGER"}}}}}`
	t.Run("non-stream", func(t *testing.T) {
		antigravityJSONModeTestServer(t, http.StatusOK, antigravityJSONModeAnswer, "")
		resp, err := ExecuteAntigravityGeminiRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(request), false, "")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		candidate := gjson.GetBytes(body, "candidates.0")
		if candidate.Get("finishReason").String() != "STOP" || candidate.Get("content.parts.0.text").String() != `{"name":"Bob","age":3}` || candidate.Get("content.parts.0.functionCall").Exists() {
			t.Fatalf("body = %s", body)
		}
	})
	t.Run("stream", func(t *testing.T) {
		antigravityJSONModeTestServer(t, http.StatusOK, "",
			`data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"json_mode","args":{"name":"Bob","age":3}}}]},"finishReason":"OTHER"}]}}`+"\n\n")
		resp, err := ExecuteAntigravityGeminiRequest(context.Background(), antigravityJSONModeTestAccount(), "claude-sonnet-4-6", []byte(request), true, "")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("stream ended with %v; body=%s", err, body)
		}
		if !strings.Contains(string(body), `"finishReason":"STOP"`) || !strings.Contains(string(body), `"text":"{\"name\":\"Bob\",\"age\":3}"`) || strings.Contains(string(body), "json_mode") {
			t.Fatalf("body = %s", body)
		}
	})
}

// newAntigravityJSONModeHandler serves a Claude-capable Antigravity account
// behind the downstream handlers, so the Chat bridge is exercised end to end.
func newAntigravityJSONModeHandler(t *testing.T) *Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	antigravityJSONModeTestServer(t, http.StatusOK, antigravityJSONModeAnswer,
		`data: `+antigravityJSONModeAnswer+"\n\n")
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, MaxRetries: 1})
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{
		DBID:                 783,
		UpstreamType:         auth.UpstreamAntigravity,
		AccessToken:          "google-access-token",
		RefreshToken:         "google-refresh-token",
		AntigravityProjectID: "google-project",
		Models:               append(auth.AntigravityDefaultModelIDs(), "claude-sonnet-4-6", "claude-opus-4-6-thinking"),
	})
	return NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
}

func TestChatCompletionsAntigravityClaudeStructuredOutput(t *testing.T) {
	for _, buffered := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			if buffered && stream {
				continue
			}
			name := map[bool]string{false: "sync", true: "stream"}[stream]
			if buffered {
				name = "buffered"
			}
			t.Run(name, func(t *testing.T) {
				withAntigravityExposeThoughts(t, buffered)
				handler := newAntigravityJSONModeHandler(t)
				request := map[string]any{
					"model":    "claude-sonnet-4-6",
					"stream":   stream,
					"messages": []any{map[string]any{"role": "user", "content": "Make up Bob"}},
					"response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{
						"name":   "person",
						"schema": map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string"}, "age": map[string]any{"type": "integer"}}},
					}},
				}
				raw, _ := json.Marshal(request)
				recorder := invokeAntigravityTransport(t, handler, "/v1/chat/completions", string(raw), (*Handler).ChatCompletions)
				body := recorder.Body.String()
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d; body=%s", recorder.Code, body)
				}
				if stream {
					if !strings.Contains(body, `"finish_reason":"stop"`) || strings.Contains(body, "tool_calls") || strings.Contains(body, "json_mode") {
						t.Fatalf("stream = %s", body)
					}
					var content strings.Builder
					for line := range strings.SplitSeq(body, "\n") {
						if data, ok := strings.CutPrefix(line, "data: "); ok && data != "[DONE]" {
							content.WriteString(gjson.Get(data, "choices.0.delta.content").String())
						}
					}
					if content.String() != `{"name":"Bob","age":3}` {
						t.Fatalf("streamed content = %q; body=%s", content.String(), body)
					}
					return
				}
				choice := gjson.Get(body, "choices.0")
				if choice.Get("finish_reason").String() != "stop" || choice.Get("message.content").String() != `{"name":"Bob","age":3}` || choice.Get("message.tool_calls").Exists() {
					t.Fatalf("body = %s", body)
				}
			})
		}
	}
}
