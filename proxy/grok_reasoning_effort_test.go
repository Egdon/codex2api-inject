package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestGrokSupportsXHighReasoningEffort(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"grok-4.7", true},
		{"grok-4.7-beta", true},
		{"grok-4.6", true},
		{"GROK-4.6", true},
		{"grok-4.6-beta", true},
		{"grok-4.6-build", true},
		{"grok-4.20-multi-agent", true},
		{"grok-5", true},
		{"grok-5.0", true},
		{"grok-4.5", false},
		{"grok-4.5-build", false},
		{"grok-4", false},
		{"grok-4-fast", false},
		{"grok-3", false},
		{"gpt-5.6-sol", false},
		{"", false},
	}
	for _, c := range cases {
		if got := grokSupportsXHighReasoningEffort(c.model); got != c.want {
			t.Fatalf("%q = %v, want %v", c.model, got, c.want)
		}
	}
}

// 旧 Grok build 只有 low/medium/high：xhigh/max 降到 high、minimal 降到 low。
// grok-4.6 起放行 xhigh；Codex 的 max 落到 xhigh。无模型时按旧 build 处理。
func TestClampGrokReasoningEffort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		path string
		want string
	}{
		{"no model xhigh→high", `{"reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "high"},
		{"no model max→high", `{"reasoning":{"effort":"max"}}`, "reasoning.effort", "high"},
		{"4.5 xhigh→high", `{"model":"grok-4.5","reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "high"},
		{"4.5 max→high", `{"model":"grok-4.5","reasoning":{"effort":"max"}}`, "reasoning.effort", "high"},
		{"4.6 xhigh stays", `{"model":"grok-4.6","reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "xhigh"},
		{"4.7 xhigh stays", `{"model":"grok-4.7","reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "xhigh"},
		{"4.7 max→xhigh", `{"model":"grok-4.7","reasoning":{"effort":"max"}}`, "reasoning.effort", "xhigh"},
		{"4.6-beta xhigh stays", `{"model":"grok-4.6-beta","reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "xhigh"},
		{"4.6-build xhigh stays", `{"model":"grok-4.6-build","reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "xhigh"},
		{"4.20-multi-agent xhigh stays", `{"model":"grok-4.20-multi-agent","reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "xhigh"},
		{"4.6 max→xhigh", `{"model":"grok-4.6","reasoning":{"effort":"max"}}`, "reasoning.effort", "xhigh"},
		{"4.6 minimal→low", `{"model":"grok-4.6","reasoning":{"effort":"minimal"}}`, "reasoning.effort", "low"},
		{"4.6 high stays", `{"model":"grok-4.6","reasoning":{"effort":"high"}}`, "reasoning.effort", "high"},
		{"4.6 medium stays", `{"model":"grok-4.6","reasoning":{"effort":"medium"}}`, "reasoning.effort", "medium"},
		{"chat 4.5 xhigh→high", `{"model":"grok-4.5","reasoning_effort":"xhigh"}`, "reasoning_effort", "high"},
		{"chat 4.6 xhigh stays", `{"model":"grok-4.6","reasoning_effort":"xhigh"}`, "reasoning_effort", "xhigh"},
		{"chat 4.6 max→xhigh", `{"model":"grok-4.6","reasoning_effort":"max"}`, "reasoning_effort", "xhigh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := clampGrokReasoningEffort([]byte(c.in))
			if got := gjson.GetBytes(out, c.path).String(); got != c.want {
				t.Fatalf("%s = %q, want %q (out=%s)", c.path, got, c.want, out)
			}
		})
	}

	// 无 effort 字段不崩、不误增。
	out := clampGrokReasoningEffort([]byte(`{"model":"grok-4.5"}`))
	if gjson.GetBytes(out, "reasoning.effort").Exists() || gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("不应凭空注入 effort 字段: %s", out)
	}

	// sanitize 主入口：旧模型仍降级，4.6 放行 xhigh。
	sane := sanitizeGrokRequestBody([]byte(`{"model":"grok-4.5","reasoning":{"effort":"xhigh"},"service_tier":"priority"}`))
	if got := gjson.GetBytes(sane, "reasoning.effort").String(); got != "high" {
		t.Fatalf("sanitize 4.5 effort = %q, want high", got)
	}
	if gjson.GetBytes(sane, "service_tier").Exists() {
		t.Fatalf("sanitize 应仍剥离 service_tier")
	}
	sane46 := sanitizeGrokRequestBody([]byte(`{"model":"grok-4.6","reasoning":{"effort":"xhigh"},"service_tier":"priority"}`))
	if got := gjson.GetBytes(sane46, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("sanitize 4.6 effort = %q, want xhigh", got)
	}
}

func TestPrepareGrokUpstreamBodyPassesXHighForGrok46(t *testing.T) {
	// reasoning 写在 model 前面，确认先窥探 model 再钳位，而不是按键序误降级。
	body := []byte(`{"reasoning":{"effort":"xhigh","summary":"detailed"},"model":"grok-4.6"}`)
	got := prepareGrokUpstreamBody(body)
	if effort := gjson.GetBytes(got.Body, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("effort = %q, want xhigh; body=%s", effort, got.Body)
	}
	if got.Model != "grok-4.6" {
		t.Fatalf("model = %q, want grok-4.6", got.Model)
	}

	old := prepareGrokUpstreamBody([]byte(`{"reasoning":{"effort":"xhigh"},"model":"grok-4.5"}`))
	if effort := gjson.GetBytes(old.Body, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("4.5 effort = %q, want high; body=%s", effort, old.Body)
	}
}
