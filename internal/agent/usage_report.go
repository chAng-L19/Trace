package agent

// References: Muteki 144df766 core/cost.py (explicit per-model rates and
// unknown prices), pi 8a7b0c03 harness/utils/usage.ts (additive usage).
// Trace aggregates durable response identities, not inherited messages.
import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type usageRequest struct {
	ID              string      `json:"id"`
	ProviderModel   string      `json:"provider_model"`
	ProviderID      string      `json:"provider_id"`
	Price           *ModelPrice `json:"price"`
	ReasoningEffort string      `json:"reasoning_effort"`
	Purpose         string      `json:"purpose"`
	CreatedAt       time.Time   `json:"created_at"`
}

type usageResponse struct {
	RequestID string    `json:"request_id"`
	CreatedAt time.Time `json:"created_at"`
	Error     string    `json:"error"`
	Response  struct {
		Usage               *TokenUsage       `json:"usage"`
		ReportedModel       string            `json:"reported_model"`
		ProviderResponseID  string            `json:"provider_response_id"`
		ReportedServiceTier string            `json:"reported_service_tier"`
		IdentityConflict    bool              `json:"identity_conflict"`
		ReportedCost        *ProviderCost     `json:"reported_cost"`
		Diagnostics         *ModelDiagnostics `json:"diagnostics"`
	} `json:"response"`
}

type usageSnapshot struct {
	Run       Run             `json:"run"`
	Requests  []usageRequest  `json:"requests"`
	Responses []usageResponse `json:"responses"`
	Actions   []struct {
		RequestID  string    `json:"request_id"`
		StartedAt  time.Time `json:"started_at"`
		FinishedAt time.Time `json:"finished_at"`
		Status     string    `json:"status"`
	} `json:"actions"`
}

// Read one atomic file generation without evidence revalidation, cache writes,
// request message histories, response bodies or action output deserialization.
// A separate process may rename the file while this descriptor remains valid.
func (s *Store) readUsageSnapshot(id string) (usageSnapshot, error) {
	var out usageSnapshot
	if s.sqlite {
		db, err := s.sqliteDB()
		if err != nil {
			return out, err
		}
		var state, run []byte
		err = db.QueryRow("SELECT r.state,h.run FROM runs r JOIN run_headers h ON h.id=r.id WHERE r.id=?", id).Scan(&state, &run)
		if err == sql.ErrNoRows {
			return out, os.ErrNotExist
		}
		if err != nil {
			return out, err
		}
		if err = json.Unmarshal(state, &out); err != nil {
			return out, err
		}
		if err = json.Unmarshal(run, &out.Run); err != nil {
			return out, err
		}
		if out.Run.ID != id {
			return out, fmt.Errorf("snapshot identity mismatch: %s", id)
		}
		return out, nil
	}
	if id == "" || strings.ContainsAny(id, `/\\`) || id == "." || id == ".." {
		return out, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.root, id+".json"))
	if err != nil {
		return out, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	if err = decoder.Decode(&out); err != nil {
		return out, err
	}
	if err = decoder.Decode(new(any)); err != io.EOF {
		return out, fmt.Errorf("invalid trailing snapshot data")
	}
	if out.Run.ID == "" {
		return out, os.ErrNotExist
	}
	if out.Run.ID != id {
		return out, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	return out, nil
}

type ModelPrice struct {
	Model                string `json:"model"`
	ProviderID           string `json:"provider_id,omitempty"`
	InputPerMillion      string `json:"input_per_million"`
	OutputPerMillion     string `json:"output_per_million"`
	Currency             string `json:"currency"`
	Source               string `json:"source"`
	EffectiveAt          string `json:"effective_at"`
	CacheReadPerMillion  string `json:"cache_read_per_million,omitempty"`
	CacheWritePerMillion string `json:"cache_write_per_million,omitempty"`
	BillingBasis         string `json:"billing_basis,omitempty"`
}

type CostConfig struct {
	Rates     []ModelPrice `json:"rates,omitempty"`
	PoolLimit string       `json:"pool_limit,omitempty"`
	Currency  string       `json:"currency,omitempty"`
}

type CostSummary struct {
	Currency             string  `json:"currency"`
	KnownAmount          *string `json:"known_amount"`
	Complete             bool    `json:"complete"`
	PricedResponses      int     `json:"priced_responses"`
	UnpricedResponses    int     `json:"unpriced_responses"`
	Basis                string  `json:"basis"`
	LegacyResponses      int     `json:"legacy_responses"`
	CachePricedResponses int     `json:"cache_priced_responses"`
}

type CostPool struct {
	Limit       string  `json:"limit,omitempty"`
	Currency    string  `json:"currency"`
	KnownAmount *string `json:"known_amount"`
	Remaining   *string `json:"remaining"`
	Complete    bool    `json:"complete"`
	Exhausted   bool    `json:"exhausted"`
	Enforcement string  `json:"enforcement"`
}

type UsageReport struct {
	RootRunID              string              `json:"root_run_id"`
	RunIDs                 []string            `json:"run_ids"`
	Usage                  UsageStats          `json:"usage"`
	MissingInputResponses  int                 `json:"missing_input_responses"`
	MissingOutputResponses int                 `json:"missing_output_responses"`
	MissingTotalResponses  int                 `json:"missing_total_responses"`
	Cost                   CostSummary         `json:"cost"`
	Pool                   CostPool            `json:"pool"`
	Rates                  []ModelPrice        `json:"rates"`
	ReportedCost           ReportedCostSummary `json:"reported_cost"`
}

func decimal(value string) (*big.Rat, error) {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 40 {
		return nil, fmt.Errorf("invalid nonnegative decimal")
	}
	dots, digits := 0, 0
	for _, c := range value {
		if c == '.' {
			dots++
			continue
		}
		if c < '0' || c > '9' {
			return nil, fmt.Errorf("invalid nonnegative decimal")
		}
		digits++
	}
	if dots > 1 || digits == 0 {
		return nil, fmt.Errorf("invalid nonnegative decimal")
	}
	n, ok := new(big.Rat).SetString(value)
	if !ok {
		return nil, fmt.Errorf("invalid nonnegative decimal")
	}
	return n, nil
}

func ValidateCostConfig(c CostConfig) error {
	if c.Currency == "" && (len(c.Rates) > 0 || c.PoolLimit != "") {
		return fmt.Errorf("cost currency is required")
	}
	seen := map[string]bool{}
	for _, rate := range c.Rates {
		key := rate.Model + "\x00" + rate.ProviderID
		if rate.Model == "" || rate.Source == "" || rate.EffectiveAt == "" || rate.Currency != c.Currency || seen[key] {
			return fmt.Errorf("model rates need unique model/provider, matching currency, source and effective_at")
		}
		if _, err := decimal(rate.InputPerMillion); err != nil {
			return err
		}
		if _, err := decimal(rate.OutputPerMillion); err != nil {
			return err
		}
		if rate.BillingBasis != "" && rate.BillingBasis != "legacy_inclusive_input" && rate.BillingBasis != "disjoint_cache_v1" {
			return fmt.Errorf("unknown billing_basis")
		}
		if rate.BillingBasis == "disjoint_cache_v1" {
			for _, value := range []string{rate.CacheReadPerMillion, rate.CacheWritePerMillion} {
				if _, err := decimal(value); err != nil {
					return fmt.Errorf("cache rates require explicit nonnegative values: %w", err)
				}
			}
		} else if rate.CacheReadPerMillion != "" || rate.CacheWritePerMillion != "" {
			return fmt.Errorf("cache rates require disjoint_cache_v1 billing_basis")
		}
		seen[key] = true
	}
	if c.PoolLimit != "" {
		_, err := decimal(c.PoolLimit)
		return err
	}
	return nil
}

func (a *Agent) ConfigureCosts(c CostConfig) error {
	if err := ValidateCostConfig(c); err != nil {
		return err
	}
	c.Rates = append([]ModelPrice(nil), c.Rates...)
	a.costConfig = c
	return nil
}

func (a *Agent) stampRequest(request *ModelRequest) {
	p, ok := a.provider.(*OpenAIProvider)
	if !ok {
		return
	}
	request.ProviderModel = p.Model
	request.ReasoningEffort = p.ReasoningEffort
	// Provider identity is a hash of the normalized endpoint; credentials and
	// query parameters never enter the request ledger.
	if u, err := url.Parse(p.BaseURL); err == nil {
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		request.ProviderID = promptHash(strings.TrimSuffix(u.String(), "/chat/completions"))
	}
	for _, rate := range a.costConfig.Rates {
		if rate.Model == request.ProviderModel && (rate.ProviderID == "" || rate.ProviderID == request.ProviderID) {
			copy := rate
			request.Price = &copy
			if rate.ProviderID != "" {
				break
			}
		}
	}
}

// UsageReport includes the selected Run and its actual Worker descendants.
// Branches carry inherited messages and are deliberately not Worker totals.
// Each response is counted once by (RunID, RequestID); repeated observations
// of a child through Plan and Worker records never add the child twice.
func (s *Store) UsageReport(id string, config CostConfig) (UsageReport, error) {
	if err := ValidateCostConfig(config); err != nil {
		return UsageReport{}, err
	}
	root, err := s.readUsageSnapshot(id)
	if err != nil {
		return UsageReport{}, err
	}
	runs, err := s.ListWithDeleted()
	if err != nil {
		return UsageReport{}, err
	}
	children := map[string][]Run{}
	for _, run := range runs {
		if run.ParentTaskID != "" {
			children[run.ParentRunID] = append(children[run.ParentRunID], run)
		}
	}
	report := UsageReport{RootRunID: id, RunIDs: []string{}, Rates: []ModelPrice{}, Cost: CostSummary{Currency: config.Currency, Complete: true, Basis: "estimated_from_reported_tokens"}, ReportedCost: ReportedCostSummary{Currency: config.Currency, Complete: true}}
	total := new(big.Rat)
	reportedTotal := new(big.Rat)
	quotes := map[string]bool{}
	seenRuns := map[string]bool{}
	queue := []Run{root.Run}
	for len(queue) > 0 {
		run := queue[0]
		queue = queue[1:]
		if seenRuns[run.ID] {
			continue
		}
		seenRuns[run.ID] = true
		snapshot := root
		if run.ID != id {
			var readErr error
			snapshot, readErr = s.readUsageSnapshot(run.ID)
			if readErr != nil {
				return UsageReport{}, readErr
			}
			if snapshot.Run.ParentRunID != run.ParentRunID || snapshot.Run.ParentTaskID != run.ParentTaskID {
				return UsageReport{}, fmt.Errorf("worker ancestry changed during usage read: %s", run.ID)
			}
		}
		requests, responses, actions := snapshot.Requests, snapshot.Responses, snapshot.Actions
		report.RunIDs = append(report.RunIDs, run.ID)
		report.Usage.ModelRequests += len(requests)
		report.Usage.ToolActions += len(actions)
		byID := map[string]usageRequest{}
		for _, request := range requests {
			if request.ID == "" {
				return UsageReport{}, fmt.Errorf("missing request identity in run %s", run.ID)
			}
			if _, exists := byID[request.ID]; exists {
				return UsageReport{}, fmt.Errorf("ambiguous request identity in run %s", run.ID)
			}
			byID[request.ID] = request
		}
		seenResponses := map[string]bool{}
		for _, record := range responses {
			if record.RequestID == "" || seenResponses[record.RequestID] {
				return UsageReport{}, fmt.Errorf("ambiguous response identity in run %s", run.ID)
			}
			request, exists := byID[record.RequestID]
			if !exists {
				return UsageReport{}, fmt.Errorf("response has no matching request in run %s", run.ID)
			}
			seenResponses[record.RequestID] = true
			report.Usage.ModelResponses++
			report.ReportedCost.add(record.Response.ReportedCost, reportedTotal)
			tokens := record.Response.Usage
			report.Usage.PromptCache.add(tokens)
			if tokens == nil {
				tokens = &TokenUsage{}
			}
			if tokens.InputTokens == nil {
				report.MissingInputResponses++
			}
			if tokens.OutputTokens == nil {
				report.MissingOutputResponses++
			}
			if tokens.TotalTokens == nil {
				report.MissingTotalResponses++
			}
			if tokens.InputTokens == nil || tokens.OutputTokens == nil || tokens.TotalTokens == nil {
				report.Usage.MissingUsageResponses++
			}
			for _, n := range []*int64{tokens.InputTokens, tokens.OutputTokens, tokens.TotalTokens} {
				if n != nil && *n < 0 {
					return UsageReport{}, fmt.Errorf("negative token usage in run %s", run.ID)
				}
			}
			sumToken(&report.Usage.KnownInputTokens, tokens.InputTokens)
			sumToken(&report.Usage.KnownOutputTokens, tokens.OutputTokens)
			sumToken(&report.Usage.KnownTotalTokens, tokens.TotalTokens)
			price := request.Price
			estimate := estimateRequestCost(request, tokens, config.Currency)
			if estimate.Amount == nil {
				report.Cost.UnpricedResponses++
				report.Cost.Complete = false
				continue
			}
			total.Add(total, estimate.exactAmount)
			report.Cost.PricedResponses++
			if estimate.Basis == "legacy_inclusive_input" {
				report.Cost.LegacyResponses++
			} else {
				report.Cost.CachePricedResponses++
			}
			key := valueHash(price)
			if !quotes[key] {
				report.Rates = append(report.Rates, *price)
				quotes[key] = true
			}
		}
		// A persisted request without its response may already incur provider cost.
		// Do not report its unknown cost as a complete zero or pool remainder.
		if len(seenResponses) < len(requests) {
			report.Cost.Complete = false
			report.ReportedCost.Complete = false
		}
		queue = append(queue, children[run.ID]...)
	}
	sort.Strings(report.RunIDs)
	sort.Slice(report.Rates, func(i, j int) bool { return valueHash(report.Rates[i]) < valueHash(report.Rates[j]) })
	if report.Cost.PricedResponses > 0 || report.Usage.ModelRequests == 0 {
		v := total.FloatString(18)
		report.Cost.KnownAmount = &v
	}
	if report.ReportedCost.ReportedResponses > 0 || report.Usage.ModelRequests == 0 {
		v := reportedTotal.FloatString(18)
		report.ReportedCost.KnownAmount = &v
	}
	report.Pool = CostPool{Limit: config.PoolLimit, Currency: config.Currency, KnownAmount: report.Cost.KnownAmount, Complete: report.Cost.Complete, Enforcement: "informational; in-flight and missing usage are not a hard spending cap"}
	if config.PoolLimit != "" && report.Cost.KnownAmount != nil {
		limit, _ := decimal(config.PoolLimit)
		report.Pool.Exhausted = total.Cmp(limit) >= 0
		if report.Cost.Complete {
			v := new(big.Rat).Sub(limit, total).FloatString(18)
			report.Pool.Remaining = &v
		}
	}
	return report, nil
}
