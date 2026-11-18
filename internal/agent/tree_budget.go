package agent

// Reference: Muteki c60c8fee coordinator_race.py:2272-2299 (AGPL-3.0),
// cumulative cost/spawn admission. Trace derives totals from its original
// Store identities under the same cross-process transaction as each claim.
import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"strconv"
	"time"
)

// All zero means unlimited. MaxSeconds is cumulative active run-seconds, not
// the parent's wall clock. Token/cost limits are admission ceilings; archived
// in-flight context/output reservations are estimates, not billing guarantees.
type TreeBudget struct {
	MaxDepth         int    `json:"max_depth"`
	MaxWorkers       int    `json:"max_workers"`
	MaxModelRequests int    `json:"max_model_requests"`
	MaxTokens        int64  `json:"max_tokens"`
	MaxSeconds       int64  `json:"max_seconds"`
	MaxCost          string `json:"max_cost,omitempty"`
	Currency         string `json:"currency,omitempty"`
}
type TreeBudgetError struct{ RootRunID, Reason string }

func (e *TreeBudgetError) Error() string { return "tree budget " + e.RootRunID + ": " + e.Reason }

func ValidateTreeBudget(b TreeBudget) error {
	if b.MaxDepth < 0 || b.MaxWorkers < 0 || b.MaxModelRequests < 0 || b.MaxTokens < 0 || b.MaxSeconds < 0 || b.MaxSeconds > maxBudgetSeconds {
		return fmt.Errorf("tree budgets must be non-negative and seconds within duration range")
	}
	if b.MaxCost != "" {
		if _, err := decimal(b.MaxCost); err != nil {
			return err
		}
		if b.Currency == "" {
			return fmt.Errorf("tree cost budget requires currency")
		}
	}
	return nil
}
func TreeBudgetFromEnv() (*TreeBudget, error) {
	b := TreeBudget{}
	present := false
	for _, f := range []struct {
		name   string
		target *int
	}{{"TRACE_TREE_MAX_DEPTH", &b.MaxDepth}, {"TRACE_TREE_MAX_WORKERS", &b.MaxWorkers}, {"TRACE_TREE_MAX_MODEL_REQUESTS", &b.MaxModelRequests}} {
		if text, ok := os.LookupEnv(f.name); ok {
			present = true
			n, err := strconv.Atoi(text)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.name, err)
			}
			*f.target = n
		}
	}
	for _, f := range []struct {
		name   string
		target *int64
	}{{"TRACE_TREE_MAX_TOKENS", &b.MaxTokens}, {"TRACE_TREE_MAX_SECONDS", &b.MaxSeconds}} {
		if text, ok := os.LookupEnv(f.name); ok {
			present = true
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.name, err)
			}
			*f.target = n
		}
	}
	if v, ok := os.LookupEnv("TRACE_TREE_MAX_COST"); ok {
		present = true
		b.MaxCost = v
	}
	if v, ok := os.LookupEnv("TRACE_TREE_CURRENCY"); ok {
		present = true
		b.Currency = v
	}
	if err := ValidateTreeBudget(b); err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	return &b, nil
}
func (a *Agent) ConfigureTreeBudget(b *TreeBudget) error {
	if b == nil {
		a.treeBudget = nil
		return nil
	}
	if err := ValidateTreeBudget(*b); err != nil {
		return err
	}
	copy := *b
	a.treeBudget = &copy
	return nil
}
func (a *Agent) SetTreeBudget(id string, budget TreeBudget) (Snapshot, error) {
	if err := ValidateTreeBudget(budget); err != nil {
		return Snapshot{}, err
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	return a.store.Update(id, func(s *Snapshot) error {
		if s.Run.ParentTaskID != "" {
			return fmt.Errorf("tree budget belongs to the root run")
		}
		if s.Run.Status == StatusRunning || s.Run.Status == StatusWaitingTool {
			return fmt.Errorf("pause root before changing tree budget")
		}
		if s.Run.TreeBudget == budget {
			return nil
		}
		s.Run.TreeBudget = budget
		addEvent(s, "tree_budget_updated", "整树准入预算已更新；累计使用不重置", budget)
		return nil
	})
}

// resolveTreeRootLocked does not acquire another lock. Ordinary conversation
// branches are independent, even though they retain a ParentRunID.
func (a *Agent) resolveTreeRootLocked(current *Snapshot) (*Snapshot, int, error) {
	root, depth := current, 0
	seen := map[string]bool{current.Run.ID: true}
	for root.Run.ParentTaskID != "" && root.Run.ParentRunID != "" {
		id := root.Run.ParentRunID
		if seen[id] {
			return nil, 0, fmt.Errorf("worker budget ancestry contains a cycle")
		}
		seen[id] = true
		parent, err := a.store.loadRawLocked(id)
		if err != nil {
			return nil, 0, err
		}
		if parent.Run.Deleted {
			return nil, 0, fmt.Errorf("worker budget ancestor is deleted")
		}
		registered := false
		for _, w := range parent.Workers {
			registered = registered || w.RunID == root.Run.ID && w.TaskID == root.Run.ParentTaskID
		}
		if !registered {
			return nil, 0, fmt.Errorf("worker budget ancestry is unregistered")
		}
		root, depth = &parent, depth+1
	}
	return root, depth, nil
}

func (a *Agent) workerTreeLocked(root, current *Snapshot) ([]*Snapshot, error) {
	queue := []*Snapshot{root}
	result := []*Snapshot{}
	seen := map[string]bool{}
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if s.Run.ID == current.Run.ID {
			s = current
		}
		if seen[s.Run.ID] {
			return nil, fmt.Errorf("duplicate worker ancestry %s", s.Run.ID)
		}
		seen[s.Run.ID] = true
		result = append(result, s)
		children := map[string]bool{}
		for _, w := range s.Workers {
			if children[w.RunID] {
				continue
			}
			children[w.RunID] = true
			var child Snapshot
			if w.RunID == current.Run.ID {
				child = *current
			} else {
				var err error
				child, err = a.store.loadRawLocked(w.RunID)
				if err != nil {
					return nil, err
				}
			}
			if child.Run.ParentRunID != s.Run.ID || child.Run.ParentTaskID != w.TaskID {
				return nil, fmt.Errorf("worker tree identity mismatch: %s", w.RunID)
			}
			queue = append(queue, &child)
		}
	}
	return result, nil
}

type treeUsage struct {
	workers, requests          int
	tokens, millis             int64
	unknownTokens, unknownCost bool
	cost                       *big.Rat
}

func addBudgetInt(total *int64, amount int64) error {
	if amount < 0 || *total > math.MaxInt64-amount {
		return fmt.Errorf("invalid or overflowing tree usage")
	}
	*total += amount
	return nil
}
func reservedRequestUsage(r ModelRequest) *TokenUsage {
	if r.ContextTokens <= 0 || r.MaxOutputTokens <= 0 {
		return nil
	}
	in, out := int64(r.ContextTokens), int64(r.MaxOutputTokens)
	if in > math.MaxInt64-out {
		return nil
	}
	total := in + out
	return &TokenUsage{InputTokens: &in, OutputTokens: &out, TotalTokens: &total}
}
func (u *treeUsage) addRequest(r ModelRequest, usage *TokenUsage, currency string) error {
	u.requests++
	if usage == nil || usage.TotalTokens == nil {
		u.unknownTokens = true
	} else if err := addBudgetInt(&u.tokens, *usage.TotalTokens); err != nil {
		return err
	}
	p := r.Price
	if usage == nil || usage.InputTokens == nil || usage.OutputTokens == nil || p == nil || p.Currency != currency || p.Model != r.ProviderModel || p.ProviderID != "" && p.ProviderID != r.ProviderID {
		u.unknownCost = true
		return nil
	}
	if err := ValidateCostConfig(CostConfig{Currency: currency, Rates: []ModelPrice{*p}}); err != nil {
		return err
	}
	if *usage.InputTokens < 0 || *usage.OutputTokens < 0 {
		return fmt.Errorf("negative reported tree usage")
	}
	in, _ := decimal(p.InputPerMillion)
	out, _ := decimal(p.OutputPerMillion)
	in.Mul(in, new(big.Rat).SetInt64(*usage.InputTokens))
	out.Mul(out, new(big.Rat).SetInt64(*usage.OutputTokens))
	in.Add(in, out)
	in.Quo(in, big.NewRat(1000000, 1))
	u.cost.Add(u.cost, in)
	return nil
}
func collectTreeUsage(nodes []*Snapshot, currency string) (treeUsage, error) {
	u := treeUsage{cost: new(big.Rat)}
	for _, s := range nodes {
		identities := map[string]bool{}
		for _, e := range s.Events {
			if e.Type != "task_claimed" {
				continue
			}
			raw, err := json.Marshal(e.Data)
			if err != nil {
				return u, err
			}
			var task PlanTask
			if err = json.Unmarshal(raw, &task); err != nil {
				return u, err
			}
			if task.WorkerID != "" && !task.LocalExecution {
				identity := task.ID + "/" + task.WorkerID
				// Old ledgers emitted task_claimed again on lease renewal. One
				// task/worker identity remains one admission across recovery.
				if !identities[identity] {
					u.workers++
					identities[identity] = true
				}
			}
		}
		for _, w := range s.Workers {
			identity := w.TaskID + "/" + w.ID
			if !identities[identity] {
				u.workers++
				identities[identity] = true
			}
		}
		for _, t := range s.Plan.Tasks {
			identity := t.ID + "/" + t.WorkerID
			if t.WorkerID != "" && !t.LocalExecution && !identities[identity] {
				u.workers++
				identities[identity] = true
			}
		}
		elapsed := s.Run.ElapsedMillis
		if !s.Run.BudgetActiveSince.IsZero() {
			if err := addBudgetInt(&elapsed, max(int64(0), time.Since(s.Run.BudgetActiveSince).Milliseconds())); err != nil {
				return u, err
			}
		}
		if err := addBudgetInt(&u.millis, elapsed); err != nil {
			return u, err
		}
		responses := map[string]ResponseRecord{}
		for _, record := range s.Responses {
			if _, exists := responses[record.RequestID]; exists {
				return u, fmt.Errorf("duplicate tree response identity")
			}
			responses[record.RequestID] = record
		}
		requests := map[string]bool{}
		for _, r := range s.Requests {
			if r.ID == "" || requests[r.ID] {
				return u, fmt.Errorf("ambiguous tree request identity")
			}
			requests[r.ID] = true
			usage := reservedRequestUsage(r)
			if response, ok := responses[r.ID]; ok {
				usage = response.Response.Usage
			}
			if err := u.addRequest(r, usage, currency); err != nil {
				return u, err
			}
		}
		for id := range responses {
			if !requests[id] {
				return u, fmt.Errorf("tree response without request")
			}
		}
	}
	return u, nil
}

func (a *Agent) checkTreeBudgetLocked(s *Snapshot, kind string) error {
	return a.admitTreeLocked(s, kind, nil)
}
func (a *Agent) admitTreeRequestLocked(s *Snapshot, request ModelRequest) error {
	return a.admitTreeLocked(s, "model", &request)
}
func (a *Agent) admitTreeLocked(s *Snapshot, kind string, request *ModelRequest) error {
	root, depth, err := a.resolveTreeRootLocked(s)
	if err != nil {
		return err
	}
	b := root.Run.TreeBudget
	if err := ValidateTreeBudget(b); err != nil {
		return err
	}
	if b == (TreeBudget{}) {
		return nil
	}
	blocked := func(reason string) error { return &TreeBudgetError{root.Run.ID, reason} }
	if (kind == "worker" || kind == "resume") && b.MaxDepth > 0 && depth+1 > b.MaxDepth {
		return blocked("maximum worker depth reached")
	}
	nodes, err := a.workerTreeLocked(root, s)
	if err != nil {
		return err
	}
	u, err := collectTreeUsage(nodes, b.Currency)
	if err != nil {
		return err
	}
	if kind == "worker" && b.MaxWorkers > 0 && u.workers >= b.MaxWorkers {
		return blocked("cumulative worker admission exhausted")
	}
	if request != nil {
		if err := u.addRequest(*request, reservedRequestUsage(*request), b.Currency); err != nil {
			return err
		}
	}
	if kind == "model" && b.MaxModelRequests > 0 && u.requests > b.MaxModelRequests {
		return blocked("cumulative model request budget exhausted")
	}
	if kind != "model" && b.MaxModelRequests > 0 && u.requests >= b.MaxModelRequests {
		return blocked("cumulative model request budget exhausted")
	}
	if b.MaxSeconds > 0 && u.millis >= b.MaxSeconds*1000 {
		return blocked("cumulative active time budget exhausted")
	}
	if b.MaxTokens > 0 {
		if u.unknownTokens {
			return blocked("token usage is unknown; reconcile usage before new admission")
		}
		if u.tokens > b.MaxTokens || request == nil && u.tokens >= b.MaxTokens {
			return blocked("token admission budget exhausted")
		}
	}
	if b.MaxCost != "" {
		limit, _ := decimal(b.MaxCost)
		if limit.Sign() > 0 {
			if u.unknownCost {
				return blocked("cost or pricing is unknown; reconcile usage before new admission")
			}
			if u.cost.Cmp(limit) > 0 || request == nil && u.cost.Cmp(limit) >= 0 {
				return blocked("cost admission budget exhausted")
			}
		}
	}
	return nil
}
