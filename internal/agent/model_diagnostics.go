package agent

import (
	"encoding/json"
	"io"
	"strings"
	"time"
)

// All durations use one process-local monotonic clock. Missing measurements
// stay nil (including legacy records); persisted wall times are never subtracted.
type ModelDiagnostics struct {
	FirstByteMillis          *int64 `json:"first_byte_ms,omitempty"`
	FirstSemanticDeltaMillis *int64 `json:"first_semantic_delta_ms,omitempty"`
	ResponseMillis           *int64 `json:"response_ms,omitempty"`
	PrepareMillis            *int64 `json:"prepare_ms,omitempty"`
	ArchiveMillis            *int64 `json:"archive_ms,omitempty"`
	HydrateMillis            *int64 `json:"hydrate_ms,omitempty"`
	ManifestStoreMillis      *int64 `json:"manifest_store_ms,omitempty"`
	StreamDrainMillis        *int64 `json:"stream_drain_ms,omitempty"`
	StreamSinkWaitMillis     *int64 `json:"stream_sink_wait_ms,omitempty"`
	StreamStoreMillis        *int64 `json:"stream_store_ms,omitempty"`
	FailureReason            string `json:"failure_reason,omitempty"`
}

// ProviderCost is an unverified provider assertion, separate from our estimate.
// Unit must explicitly be currency (not tokens/credits) before monetary totals.
type ProviderCost struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Source   string `json:"source"`
	Conflict bool   `json:"conflict,omitempty"`
}

func elapsedMillis(start time.Time) *int64 {
	n := time.Since(start).Milliseconds()
	return &n
}

type firstByteReader struct {
	io.Reader
	start      time.Time
	diagnostic *ModelDiagnostics
}

func (r firstByteReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 && r.diagnostic.FirstByteMillis == nil {
		r.diagnostic.FirstByteMillis = elapsedMillis(r.start)
	}
	return n, err
}

func semanticDelta(d StreamDelta) bool {
	return d.Text != "" || d.Name != "" || d.Arguments != ""
}

func hasSemanticResponse(r ModelResponse) bool {
	if r.Content != "" || r.Thinking != "" {
		return true
	}
	for _, call := range r.ToolCalls {
		if call.Name != "" || call.Arguments != "" {
			return true
		}
	}
	return false
}

// Bound metadata at the remote boundary. These strings never affect routing,
// identity validation, retry policy or evidence verification.
func boundedProviderMetadata(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 256 {
		return ""
	}
	return s
}

func (out *ModelResponse) observeMetadata(w wireResponse) {
	for _, pair := range []struct {
		dst   *string
		value json.RawMessage
	}{
		{&out.ReportedModel, w.Model}, {&out.ProviderResponseID, w.ID}, {&out.ReportedServiceTier, w.ServiceTier},
	} {
		value := providerMetadataString(pair.value)
		if value == "" {
			continue
		}
		if *pair.dst == "" {
			*pair.dst = value
		} else if *pair.dst != value {
			out.IdentityConflict = true
		}
	}
	var cost *ProviderCost
	if len(w.ReportedCost) > 0 && string(w.ReportedCost) != "null" {
		var native struct{ Amount, Currency, Unit json.RawMessage }
		if json.Unmarshal(w.ReportedCost, &native) == nil {
			cost = reportedScalarCost(native.Amount, native.Currency, native.Unit, "provider.response.reported_cost")
		} else {
			cost = &ProviderCost{Source: "provider.response.reported_cost"}
		}
	} else if len(w.Cost) > 0 && string(w.Cost) != "null" {
		cost = reportedScalarCost(w.Cost, w.CostCurrency, w.CostUnit, "provider.response.cost")
	} else if w.Usage != nil && len(w.Usage.Cost) > 0 && string(w.Usage.Cost) != "null" {
		cost = reportedScalarCost(w.Usage.Cost, w.Usage.CostCurrency, w.Usage.CostUnit, "provider.usage.cost")
	}
	if cost != nil {
		cost.Amount = boundedProviderMetadata(cost.Amount)
		cost.Currency = boundedProviderMetadata(cost.Currency)
		cost.Unit = boundedProviderMetadata(cost.Unit)
		if previous := out.ReportedCost; previous != nil {
			// Stream cost updates may be cumulative; only the last report counts.
			// A unit/currency change cannot be interpreted as a monetary total.
			cost.Conflict = previous.Conflict || cost.Conflict || previous.Unit != "" && cost.Unit != "" && previous.Unit != cost.Unit || previous.Currency != "" && cost.Currency != "" && previous.Currency != cost.Currency
			if cost.Unit == "" {
				cost.Unit = previous.Unit
			}
			if cost.Currency == "" {
				cost.Currency = previous.Currency
			}
		}
		out.ReportedCost = cost
	}
}

func providerMetadataString(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return boundedProviderMetadata(value)
}

func reportedScalarCost(raw, currency, unit json.RawMessage, source string) *ProviderCost {
	amount := strings.TrimSpace(string(raw))
	if strings.HasPrefix(amount, `"`) {
		if json.Unmarshal(raw, &amount) != nil {
			amount = ""
		}
	}
	return &ProviderCost{Amount: amount, Currency: providerMetadataString(currency), Unit: providerMetadataString(unit), Source: source}
}
