package agent

import (
	"encoding/json"
	"math/big"
	"reflect"
	"testing"
)

func cachePriceFixture() ModelPrice {
	return ModelPrice{Model: "fixture", InputPerMillion: "2", OutputPerMillion: "4", CacheReadPerMillion: "0.2", CacheWritePerMillion: "3", BillingBasis: "disjoint_cache_v1", Currency: "USD", Source: "fixture only", EffectiveAt: "2026-11-10"}
}

func TestCacheCostDisjointBucketsMissingZeroAndConflict(t *testing.T) {
	price := cachePriceFixture()
	request := usageRequest{ProviderModel: "fixture", Price: &price}
	for _, tc := range []struct{ name, raw, want, reason string }{
		{"cache", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":10}}`, "0.000142000000000000", ""},
		{"miss", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, "0.000240000000000000", ""},
		{"all_zero", `{"prompt_tokens":0,"completion_tokens":0,"prompt_tokens_details":{"cached_tokens":0,"cache_write_tokens":0}}`, "0.000000000000000000", ""},
		{"read_missing", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cache_write_tokens":0}}`, "", "missing_cache_tokens"},
		{"write_missing", `{"prompt_tokens":100,"completion_tokens":10,"cached_tokens":0}`, "", "missing_cache_tokens"},
		{"conflict", `{"prompt_tokens":100,"completion_tokens":10,"cached_tokens":10,"prompt_tokens_details":{"cached_tokens":20,"cache_write_tokens":0}}`, "", "conflicting_cache_tokens"},
		{"overlap", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":80,"cache_write_tokens":30}}`, "", "conflicting_cache_tokens"},
		{"negative", `{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":-1,"cache_write_tokens":0}}`, "", "conflicting_cache_tokens"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokens TokenUsage
			if err := json.Unmarshal([]byte(tc.raw), &tokens); err != nil {
				t.Fatal(err)
			}
			before, _ := json.Marshal(tokens)
			estimate := estimateRequestCost(request, &tokens, "USD")
			if estimate.Reason != tc.reason || (estimate.Amount != nil) != (tc.want != "") || estimate.Amount != nil && *estimate.Amount != tc.want {
				t.Fatalf("incorrect estimate: %+v", estimate)
			}
			if estimate.Buckets != nil && estimate.Buckets.Input+estimate.Buckets.CacheRead+estimate.Buckets.CacheWrite != *tokens.InputTokens {
				t.Fatalf("cache double counted: %+v", estimate.Buckets)
			}
			after, _ := json.Marshal(tokens)
			if string(before) != string(after) {
				t.Fatal("estimation changed inclusive budget usage")
			}
		})
	}
	price.BillingBasis, price.CacheReadPerMillion, price.CacheWritePerMillion = "", "", ""
	estimate := estimateRequestCost(request, &TokenUsage{InputTokens: int64Pointer(100), OutputTokens: int64Pointer(10)}, "USD")
	if estimate.Amount == nil || *estimate.Amount != "0.000240000000000000" || estimate.Basis != "legacy_inclusive_input" || estimate.Buckets != nil {
		t.Fatalf("legacy price changed: %+v", estimate)
	}
}

func TestNativeCostReportedZeroMissingNegativeUnitAndCurrency(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		cost                                       *ProviderCost
		reported, missing, invalid, unit, currency int
	}{
		{"zero", &ProviderCost{Amount: "0", Currency: "USD", Unit: "currency", Source: "provider.usage.cost"}, 1, 0, 0, 0, 0},
		{"exponent", &ProviderCost{Amount: "8.52e-5", Currency: "USD", Unit: "currency", Source: "provider.usage.cost"}, 1, 0, 0, 0, 0},
		{"unbounded_exponent", &ProviderCost{Amount: "1e999999999", Currency: "USD", Unit: "currency", Source: "provider.usage.cost"}, 0, 0, 1, 0, 0},
		{"missing", nil, 0, 1, 0, 0, 0},
		{"negative", &ProviderCost{Amount: "-1", Currency: "USD", Unit: "currency", Source: "provider.usage.cost"}, 0, 0, 1, 0, 0},
		{"unknown_unit", &ProviderCost{Amount: "0", Currency: "USD", Source: "provider.usage.cost"}, 0, 0, 0, 1, 0},
		{"mismatch", &ProviderCost{Amount: "0", Currency: "EUR", Unit: "currency", Source: "provider.usage.cost"}, 0, 0, 0, 0, 1},
		{"conflict", &ProviderCost{Amount: "0", Currency: "USD", Unit: "currency", Source: "provider.usage.cost", Conflict: true}, 0, 0, 1, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, total := ReportedCostSummary{Currency: "USD", Complete: true}, new(big.Rat)
			summary.add(tc.cost, total)
			if summary.ReportedResponses != tc.reported || summary.MissingResponses != tc.missing || summary.InvalidResponses != tc.invalid || summary.UnknownUnitResponses != tc.unit || summary.CurrencyMismatchResponses != tc.currency || summary.Complete != (tc.reported == 1) {
				t.Fatalf("native cost misclassified: %+v", summary)
			}
		})
	}
}

func TestCacheCostPinnedPricesAndDiagnosticsReadOnlyBackends(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store *Store
			var err error
			if backend == "sqlite" {
				store = sqliteTestStore(t)
			} else {
				store, err = NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			a, err := New(store, &OpenAIProvider{Model: "fixture", BaseURL: "https://fixture.invalid/v1"}, "")
			if err != nil {
				t.Fatal(err)
			}
			price := cachePriceFixture()
			config := CostConfig{Currency: "USD", Rates: []ModelPrice{price}}
			if err = a.ConfigureCosts(config); err != nil {
				t.Fatal(err)
			}
			id := createTestRun(t, a)
			_, err = store.Update(id, func(s *Snapshot) error {
				for _, requestID := range []string{"old", "new"} {
					r := ModelRequest{ID: requestID}
					a.stampRequest(&r)
					s.Requests = append(s.Requests, r)
					s.Responses = append(s.Responses, ResponseRecord{RequestID: requestID, Response: ModelResponse{ReportedModel: "alias", Usage: &TokenUsage{InputTokens: int64Pointer(100), OutputTokens: int64Pointer(10), TotalTokens: int64Pointer(110), PromptTokensDetails: &PromptTokenDetails{CachedTokens: int64Pointer(60), CacheWriteTokens: int64Pointer(10)}}, ReportedCost: &ProviderCost{Amount: "0", Currency: "USD", Unit: "currency", Source: "provider.usage.cost"}}})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before, err := store.readUsageSnapshot(id)
			if err != nil {
				t.Fatal(err)
			}
			config.Rates[0].CacheReadPerMillion = "999"
			report, err := store.UsageReport(id, config)
			if err != nil || report.Cost.KnownAmount == nil || *report.Cost.KnownAmount != "0.000284000000000000" || report.Cost.CachePricedResponses != 2 || report.ReportedCost.KnownAmount == nil || *report.ReportedCost.KnownAmount != "0.000000000000000000" || report.ReportedCost.ReportedResponses != 2 {
				t.Fatalf("repriced, double-counted or merged actual/estimate: %+v %v", report, err)
			}
			diagnostic, err := store.Diagnostics(id, "USD", 1)
			if err != nil || !diagnostic.Truncated || len(diagnostic.Requests) != 1 || diagnostic.Requests[0].RequestID != "new" || diagnostic.Requests[0].Timing != nil {
				t.Fatalf("bounded/legacy diagnostic: %+v %v", diagnostic, err)
			}
			after, err := store.readUsageSnapshot(id)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("read projection modified state")
			}
			if _, err := store.Diagnostics(id, "USD", 101); err == nil {
				t.Fatal("unbounded diagnostics accepted")
			}
		})
	}
}

func TestCachePriceRequiresExplicitVersionAndBothRates(t *testing.T) {
	base := cachePriceFixture()
	for _, change := range []func(*ModelPrice){
		func(p *ModelPrice) { p.BillingBasis = "" },
		func(p *ModelPrice) { p.CacheReadPerMillion = "" },
		func(p *ModelPrice) { p.CacheWritePerMillion = "" },
		func(p *ModelPrice) { p.CacheWritePerMillion = "-1" },
		func(p *ModelPrice) { p.BillingBasis = "unknown_v9" },
	} {
		price := base
		change(&price)
		if ValidateCostConfig(CostConfig{Currency: "USD", Rates: []ModelPrice{price}}) == nil {
			t.Fatalf("ambiguous billing accepted: %+v", price)
		}
	}
}
