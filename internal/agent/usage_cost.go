package agent

import (
	"math/big"
	"strconv"
	"strings"
)

type BillingTokenBuckets struct {
	Input      int64 `json:"input"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
}

type CostEstimate struct {
	exactAmount *big.Rat
	Status      string               `json:"status"`
	Basis       string               `json:"basis"`
	Amount      *string              `json:"amount"`
	Currency    string               `json:"currency"`
	Source      string               `json:"source,omitempty"`
	Reason      string               `json:"reason,omitempty"`
	Buckets     *BillingTokenBuckets `json:"buckets,omitempty"`
}

func estimateRequestCost(request usageRequest, tokens *TokenUsage, currency string) CostEstimate {
	out := CostEstimate{Status: "unknown", Currency: currency, Basis: "unknown", Reason: "missing_price"}
	price := request.Price
	if price == nil {
		return out
	}
	if price.Model != request.ProviderModel || price.ProviderID != "" && price.ProviderID != request.ProviderID || price.Currency != currency {
		out.Reason = "price_identity_mismatch"
		return out
	}
	if ValidateCostConfig(CostConfig{Rates: []ModelPrice{*price}, Currency: price.Currency}) != nil {
		out.Reason = "invalid_price"
		return out
	}
	out.Source, out.Basis = price.Source, price.BillingBasis
	if out.Basis == "" {
		out.Basis = "legacy_inclusive_input"
	}
	if tokens == nil || tokens.InputTokens == nil || tokens.OutputTokens == nil || *tokens.InputTokens < 0 || *tokens.OutputTokens < 0 {
		out.Reason = "missing_or_invalid_tokens"
		return out
	}
	input, output := *tokens.InputTokens, *tokens.OutputTokens
	total := new(big.Rat)
	add := func(count int64, rate string) {
		n, _ := decimal(rate)
		total.Add(total, n.Mul(n, new(big.Rat).SetInt64(count)))
	}
	if out.Basis == "disjoint_cache_v1" {
		buckets, reason := billingTokenBuckets(tokens)
		if buckets == nil {
			out.Reason = reason
			return out
		}
		out.Buckets = buckets
		input = buckets.Input
		add(buckets.CacheRead, price.CacheReadPerMillion)
		add(buckets.CacheWrite, price.CacheWritePerMillion)
	}
	add(input, price.InputPerMillion)
	add(output, price.OutputPerMillion)
	amount := total.Quo(total, big.NewRat(1000000, 1)).FloatString(18)
	out.Status, out.Amount, out.Reason = "estimated", &amount, ""
	out.exactAmount = total
	return out
}

type ReportedCostSummary struct {
	Currency                  string  `json:"currency"`
	KnownAmount               *string `json:"known_amount"`
	Complete                  bool    `json:"complete"`
	ReportedResponses         int     `json:"reported_responses"`
	MissingResponses          int     `json:"missing_responses"`
	InvalidResponses          int     `json:"invalid_responses"`
	UnknownUnitResponses      int     `json:"unknown_unit_responses"`
	CurrencyMismatchResponses int     `json:"currency_mismatch_responses"`
}

func (s *ReportedCostSummary) add(cost *ProviderCost, total *big.Rat) {
	if cost == nil {
		s.MissingResponses++
		s.Complete = false
		return
	}
	amount, err := reportedCostDecimal(cost.Amount)
	if err != nil || cost.Conflict || cost.Source == "" {
		s.InvalidResponses++
		s.Complete = false
		return
	}
	if cost.Unit != "currency" || cost.Currency == "" {
		s.UnknownUnitResponses++
		s.Complete = false
		return
	}
	if cost.Currency != s.Currency {
		s.CurrencyMismatchResponses++
		s.Complete = false
		return
	}
	s.ReportedResponses++
	total.Add(total, amount)
}

// JSON numeric costs may use scientific notation. Preserve exact decimal
// arithmetic while bounding the exponent before allocating a big integer.
func reportedCostDecimal(value string) (*big.Rat, error) {
	plain, plainErr := decimal(value)
	if plainErr == nil {
		return plain, nil
	}
	index := strings.IndexAny(value, "eE")
	if index < 1 || len(value) > 40 {
		return nil, plainErr
	}
	mantissa, err := decimal(value[:index])
	if err != nil {
		return nil, plainErr
	}
	exponent, err := strconv.Atoi(value[index+1:])
	if err != nil || exponent < -100 || exponent > 100 {
		return nil, plainErr
	}
	power := exponent
	if power < 0 {
		power = -power
	}
	scale := new(big.Rat).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(power)), nil))
	if exponent < 0 {
		return mantissa.Quo(mantissa, scale), nil
	}
	return mantissa.Mul(mantissa, scale), nil
}
