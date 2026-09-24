package database

import (
	"fmt"
	"strings"
	"testing"
)

func TestGPT6SolAndLunaIndependentPricingKeys(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	for _, tc := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-6-sol", 2, 0.2, 10},
		{"gpt-6-luna", 0.1, 0.01, 0.5},
	} {
		for _, model := range []string{
			tc.model,
			strings.ToUpper(tc.model),
			"models/" + tc.model,
			tc.model + "-high",
			tc.model + "(xhigh)",
			tc.model + "-2026-09-22",
			tc.model + "-openai-compact",
		} {
			t.Run(model, func(t *testing.T) {
				if got := PricingManagementModelKey(model); got != tc.model {
					t.Fatalf("pricing key = %q, want %q", got, tc.model)
				}
				if got := PricingAliasTarget(model); got != "" {
					t.Fatalf("independent model incorrectly aliases %q", got)
				}
				p := GetModelPricing(model)
				assertFloatEqual(t, p.InputPricePerMToken, tc.input)
				assertFloatEqual(t, p.CacheReadPricePerMToken, tc.cached)
				assertFloatEqual(t, p.OutputPricePerMToken, tc.output)
				assertFloatEqual(t, p.LongInputPricePerMToken, tc.input*2)
				assertFloatEqual(t, p.LongCacheReadPricePerMToken, tc.cached*2)
				assertFloatEqual(t, p.LongOutputPricePerMToken, tc.output*1.5)
			})
		}
	}
}

func TestGPT6SolAndLunaCostBreakdown(t *testing.T) {
	previous := currentModelPricingOverrides()
	SetModelPricingOverrides(nil)
	t.Cleanup(func() { SetModelPricingOverrides(previous) })

	for _, tc := range []struct {
		model                 string
		input, cached, output float64
	}{
		{"gpt-6-sol", 2, 0.2, 10},
		{"gpt-6-luna", 0.1, 0.01, 0.5},
	} {
		for _, inputTokens := range []int{271999, 272001} {
			for _, tier := range []struct {
				name       string
				multiplier float64
			}{{"", 1}, {"fast", 2}, {"priority", 2}} {
				t.Run(fmt.Sprintf("%s/%d/%s", tc.model, inputTokens, tier.name), func(t *testing.T) {
					const cachedTokens, outputTokens = 100000, 1000
					input, cached, output := tc.input, tc.cached, tc.output
					long := inputTokens > 272000
					if long {
						input *= 2
						cached *= 2
						output *= 1.5
					}
					got := CalculateCostBreakdown(inputTokens, outputTokens, cachedTokens, tc.model, tier.name)
					if got.LongContext != long {
						t.Fatalf("long context = %v, want %v", got.LongContext, long)
					}
					assertFloatEqual(t, got.InputPricePerMToken, input*tier.multiplier)
					assertFloatEqual(t, got.CacheReadPricePerMToken, cached*tier.multiplier)
					assertFloatEqual(t, got.OutputPricePerMToken, output*tier.multiplier)
					want := (float64(inputTokens-cachedTokens)*input + cachedTokens*cached + outputTokens*output) / 1e6 * tier.multiplier
					assertFloatEqual(t, got.TotalCost, want)
				})
			}
		}
	}
}

func TestGPT6SolAndLunaOverridesStayIndependentOfAstra(t *testing.T) {
	previous := currentModelPricingOverrides()
	t.Cleanup(func() { SetModelPricingOverrides(previous) })
	SetModelPricingOverrides(map[string]ModelPricingOverride{
		"gpt-6-astra": {Source: ModelPricingSourceCustom, Input: 90, Output: 190},
		"gpt-6-sol": {
			Source: ModelPricingSourceSynced, Input: 3, CachedInput: 0.3, Output: 11,
			InputLong: 6, CachedInputLong: 0.6, OutputLong: 16,
		},
		"gpt-6-luna": {
			Source: ModelPricingSourceCustom, Input: 0.15, CachedInput: 0.015, Output: 0.6,
			InputLong: 0.3, CachedInputLong: 0.03, OutputLong: 0.9,
		},
	})

	for _, tc := range []struct {
		model              string
		input, output      float64
		longInput, longOut float64
	}{
		{"gpt-6-sol", 3, 11, 6, 16},
		{"gpt-6-luna", 0.15, 0.6, 0.3, 0.9},
	} {
		t.Run(tc.model, func(t *testing.T) {
			p := GetModelPricing(tc.model + "(xhigh)")
			assertFloatEqual(t, p.InputPricePerMToken, tc.input)
			assertFloatEqual(t, p.OutputPricePerMToken, tc.output)
			assertFloatEqual(t, p.LongInputPricePerMToken, tc.longInput)
			assertFloatEqual(t, p.LongOutputPricePerMToken, tc.longOut)
		})
	}
	astra := GetModelPricing("gpt-6-astra")
	assertFloatEqual(t, astra.InputPricePerMToken, 90)
	assertFloatEqual(t, astra.OutputPricePerMToken, 190)
}
