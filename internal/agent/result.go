package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func newIncomplete(reason string) Result {
	return Result{Status: "incomplete", Answers: []Answer{}, EvidenceIDs: []string{}, ReproductionSteps: []string{}, CleanupStatus: "unknown", TerminalReason: reason}
}

// traceResultJSON reads an explicit top-level envelope, never a nested example
// or an arbitrary JSON object. Duplicate envelope keys are ambiguous.
func traceResultJSON(text string) (*Result, bool) {
	return traceEnvelopeJSON[Result](text, "trace_result")
}

func traceEnvelopeJSON[T any](text, envelope string) (*T, bool) {
	decoder := json.NewDecoder(strings.NewReader(text))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, false
	}
	var result *T
	present := false
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, present
		}
		isResult := key == envelope
		if isResult && present {
			return nil, true
		}
		present = present || isResult
		var value json.RawMessage
		if decoder.Decode(&value) != nil {
			return nil, present
		}
		if isResult && json.Unmarshal(value, &result) != nil {
			return nil, true
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, present
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, present
	}
	return result, present
}

// A report may accompany its one structured result with prose. Only a closed,
// explicitly json-labelled fence is eligible; other code blocks stay prose.
// Multiple candidates, including malformed/null candidates, are not selected.
func traceResultClaim(text string) *Result {
	result, _ := traceResultEnvelope(text)
	return result
}

func traceResultEnvelope(text string) (*Result, bool) {
	return traceEnvelope[Result](text, "trace_result")
}

func traceEnvelope[T any](text, envelope string) (*T, bool) {
	if result, present := traceEnvelopeJSON[T](strings.TrimSpace(text), envelope); present {
		return result, true
	}
	var result *T
	presentResult := false
	lines := strings.Split(text, "\n")
	fence, start, jsonBlock := "", 0, false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		marks := 0
		if len(line) > 0 && (line[0] == '`' || line[0] == '~') {
			marks = len(line) - len(strings.TrimLeft(line, line[:1]))
		}
		if fence == "" {
			if marks >= 3 {
				fence, start = line[:marks], i+1
				jsonBlock = strings.EqualFold(strings.TrimSpace(line[marks:]), "json")
			}
			continue
		}
		if !strings.HasPrefix(line, fence) || strings.TrimSpace(line[marks:]) != "" {
			continue
		}
		if jsonBlock {
			claim, present := traceEnvelopeJSON[T](strings.Join(lines[start:i], "\n"), envelope)
			if present {
				if claim == nil || result != nil {
					return nil, true
				}
				presentResult = true
				result = claim
			}
		}
		fence, jsonBlock = "", false
	}
	if fence != "" && jsonBlock {
		_, present := traceEnvelopeJSON[T](strings.Join(lines[start:], "\n"), envelope)
		return nil, presentResult || present
	}
	return result, presentResult
}

func refreshResult(s *Snapshot) {
	defer func() {
		if gaps := rootContractGaps(s); len(gaps) > 0 && s.Result.Status == "success" {
			s.Result.Status = "incomplete"
			s.Result.TerminalReason = strings.Join(gaps, "; ")
		}
	}()
	refreshVerification(s)
	defer func() {
		if len(s.Requirements) == 0 {
			return
		}
		if missing := unmetRequirements(s); len(missing) > 0 && s.Result.Status == "success" {
			s.Result.Status = "incomplete"
			s.Result.TerminalReason = "required evidence checks have not passed: " + strings.Join(missing, ", ")
		}
		cleanup, all := false, true
		for _, r := range s.Requirements {
			if r.Category != "cleanup" {
				continue
			}
			cleanup = true
			c := latestCheck(s, r.ID)
			all = all && c != nil && c.Status == "pass"
		}
		if cleanup && all {
			s.Result.CleanupStatus = "completed"
		}
	}()
	for i := range s.Evidence {
		s.Evidence[i].Verification = verifyEvidence(s, &s.Evidence[i])
	}
	result := newIncomplete("run has not produced a verified final result")
	switch s.Run.Status {
	case StatusCancelled:
		result.Status = "cancelled"
		result.TerminalReason = "operator cancelled run"
	case StatusFailed:
		result.Status = "failed"
		result.TerminalReason = s.Run.Error
	case StatusPaused:
		result.TerminalReason = s.Run.PauseReason
		if result.TerminalReason == "" {
			result.TerminalReason = s.Run.Error
		}
	}
	pausedResult := s.Run.Status == StatusPaused && (s.Run.PauseReason == "report_translation_missing" || s.Run.PauseReason == "child_report_delivery_pending" || s.Run.PauseReason == "completion_blocked")
	// Delivery failure stops the executor without rewriting a source-verified
	// business conclusion. Recompute from its original claim and ledger so a
	// changed evidence source still revokes success.
	deliveryResult := s.ReportDelivery != nil && s.ReportDelivery.Pending
	repairResult := s.Run.Status == StatusRunning && len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == "user" && strings.HasPrefix(s.Messages[len(s.Messages)-1].Content, "Runtime completion audit: the required outcome observations and answer values already validate;")
	childDeliveryResult := s.Run.Status == StatusRunning && len(s.Messages) > 0 && s.Messages[len(s.Messages)-1].Role == "user" && strings.HasPrefix(s.Messages[len(s.Messages)-1].Content, "Business results are retained; full delivery still requires the current source-bound Chinese and English file reports.")
	if s.Run.Status == StatusCancelled || s.Run.Status == StatusFailed && !deliveryResult || s.Run.Status != StatusCompleted && !deliveryResult && !pausedResult && !repairResult && !childDeliveryResult {
		s.Result = result
		return
	}
	final, summary, report := finalReportTurn(s)
	result.Report = report
	result.Summary = summary
	result.TerminalReason = "model reply completed; target success has not been verified"
	if len(pendingToolCalls(s)) > 0 {
		result.TerminalReason = "tool results remain unresolved"
		s.Result = result
		return
	}
	if err := toolCleanupError(*s); err != nil {
		result.TerminalReason = err.Error()
		s.Result = result
		return
	}
	for _, worker := range s.Workers {
		if workerUnsettled(s, worker) {
			result.TerminalReason = "child worker completion or cleanup remains unresolved"
			s.Result = result
			return
		}
	}
	if reason := planDispositionFailure(s); reason != "" {
		result.Status, result.TerminalReason = "failed", reason
		s.Result = result
		return
	}
	for _, task := range s.Plan.Tasks {
		if !taskSatisfiedForRoot(s, task) {
			result.TerminalReason = "planned tasks remain unresolved"
			s.Result = result
			return
		}
	}
	claim, explicitResult := traceResultEnvelope(final)
	if explicitResult && claim == nil {
		result.ValidationIssues = []ResultValidationIssue{{Code: "malformed_result", Field: "trace_result", Blocking: true, Repair: "Return exactly one non-null trace_result object in valid JSON or one closed json fence; reuse the existing evidence IDs."}}
		result.TerminalReason = resultValidationSummary(result.ValidationIssues)
	}
	if claim != nil {
		if claim.Report != nil && result.Report == nil {
			result.Report = claim.Report
		}
		result.Summary = claim.Summary
		result.Answers = claim.Answers
		result.EvidenceIDs = claim.EvidenceIDs
		result.SupportingEvidenceIDs = claim.SupportingEvidenceIDs
		result.ArtifactEvidenceIDs = claim.ArtifactEvidenceIDs
		result.ReproductionSteps = claim.ReproductionSteps
		if result.Answers == nil {
			result.Answers = []Answer{}
		}
		if result.EvidenceIDs == nil {
			result.EvidenceIDs = []string{}
		}
		if result.ReproductionSteps == nil {
			result.ReproductionSteps = []string{}
		}
		allValid := validateResultReferences(s, &result)
		switch claim.Status {
		case "failed":
			result.Status = "failed"
			result.TerminalReason = "structured result reports failure"
		case "success":
			if allValid {
				result.Status = "success"
				result.TerminalReason = "structured success marker and referenced execution evidence verified"
			} else {
				result.TerminalReason = resultValidationSummary(result.ValidationIssues)
			}
		case "incomplete":
			result.TerminalReason = "structured result reports incomplete work"
		default:
			result.ValidationIssues = append(result.ValidationIssues, ResultValidationIssue{
				Code: "invalid_result_status", Field: "trace_result.status", Blocking: true,
				Repair: fmt.Sprintf("trace_result.status received %q; allowed values are success, failed, incomplete. Choose the business outcome supported by the existing evidence; completed is a run lifecycle state, not a result status. Preserve the required answers, references and checks.", claim.Status),
			})
			result.TerminalReason = resultValidationSummary(result.ValidationIssues)
		}
		// Cleanup assertions need their own proof; retain unknown until a cleanup verifier exists.
	}
	// An explicit incomplete/failed or ambiguous envelope must not be
	// overwritten by a separate passing predicate or requirement set.
	predicateAllowed := !explicitResult || claim != nil && claim.Status == "success"
	for _, issue := range result.ValidationIssues {
		// A predicate may supply an omitted outcome, but must not launder
		// an invalid answer or a missing top-level answer reference.
		if issue.Blocking && issue.Code != "outcome_evidence_missing" {
			predicateAllowed = false
		}
	}
	for _, id := range result.EvidenceIDs {
		if _, ok := verifiedOutput(s, id); !ok {
			predicateAllowed = false
		}
	}
	for _, answer := range result.Answers {
		if !answer.Verified {
			predicateAllowed = false
		}
	}
	if s.Run.SuccessContains != "" && result.Status == "success" {
		result.Status = "incomplete"
		result.TerminalReason = "persisted success predicate has not matched execution evidence"
	}
	if s.Run.SuccessContains != "" && predicateAllowed {
		for _, e := range s.Evidence {
			output, ok := verifiedOutput(s, e.ID)
			if ok && strings.Contains(output, s.Run.SuccessContains) {
				result.Status = "success"
				result.TerminalReason = "persisted stdout-contains success predicate matched verified execution evidence"
				if !hasString(result.EvidenceIDs, e.ID) {
					result.EvidenceIDs = append(result.EvidenceIDs, e.ID)
				}
				result.Answers = append(result.Answers, Answer{Value: s.Run.SuccessContains, Kind: "answer", EvidenceIDs: []string{e.ID}, Verified: true})
				// The persisted predicate supplies the previously absent outcome ref.
				validateResultReferences(s, &result)
				break
			}
		}
	}
	if len(s.Requirements) > 0 && len(unmetRequirements(s)) == 0 && !explicitResult && s.Run.SuccessContains == "" {
		s.Result = result
		result.Status = "success"
		result.TerminalReason = "all declared evidence checks passed"
		for _, c := range s.Checks {
			if c.Status == "pass" {
				for _, id := range c.EvidenceIDs {
					if !hasString(result.EvidenceIDs, id) {
						result.EvidenceIDs = append(result.EvidenceIDs, id)
					}
				}
			}
		}
	}
	s.Result = result
}

func verifyEvidence(s *Snapshot, e *Evidence) string {
	if e.ContentHash != promptHash(e.Content) {
		return "invalid"
	}
	eventOK := false
	for _, event := range s.Events {
		if event.Sequence == e.SourceEventSequence && event.Type == "tool_result" {
			data, _ := json.Marshal(event.Data)
			var fields map[string]string
			json.Unmarshal(data, &fields)
			eventOK = fields["call_id"] == e.CallID && fields["evidence_id"] == e.ID
			// Scope binds the declared execution contract, never an external-state
			// observation. Preserve legacy unscoped ledgers without a scope stamp.
			if e.Scope != nil || fields["evidence_scope_hash"] != "" {
				eventOK = eventOK && fields["evidence_scope_hash"] == valueHash(e.Scope)
			}
		}
	}
	if !eventOK {
		return "invalid"
	}
	messageOK := false
	for _, m := range s.Messages {
		if m.Role == "tool" && m.ToolCallID == e.CallID && m.Content == e.Content {
			messageOK = true
			break
		}
	}
	if !messageOK {
		return "invalid"
	}
	if e.Type != "tool_execution" {
		return "unverified"
	}
	for _, action := range s.Actions {
		if action.RequestID == e.RequestID && action.CallID == e.CallID && action.Result != nil && action.Status == "completed" {
			if !actionMatchesResponse(s, action) {
				return "invalid"
			}
			data, _ := json.Marshal(action.Result)
			if string(data) == e.Content && action.Result.ExitCode == 0 && !action.Result.Interrupted && !action.Result.MCPIsError && !action.Result.OutcomeUncertain && !action.Result.WorkerOutcomeUncertain && action.Result.Error == "" {
				if !verifyArtifacts(s.artifactRoot, action.Result) {
					return "invalid"
				}
				return "verified"
			}
		}
	}
	return "unverified"
}

// Tool call IDs are response-scoped. Preserve the original model routing and
// arguments as an independent ledger counterpart to the recorded action.
func actionMatchesResponse(s *Snapshot, action Action) bool {
	requestFound := false
	for _, request := range s.Requests {
		if request.ID == action.RequestID && request.Purpose != "evidence_review" {
			requestFound = true
			break
		}
	}
	if !requestFound {
		return false
	}
	for _, response := range s.Responses {
		if response.RequestID != action.RequestID || response.Error != "" {
			continue
		}
		for _, call := range response.Response.ToolCalls {
			if call.ID == action.CallID {
				return call.Name == action.Name && call.Arguments == action.Arguments
			}
		}
	}
	return false
}

func verifiedOutput(s *Snapshot, id string) (string, bool) {
	output, issue := diagnoseResultEvidence(s, id, false)
	return output, issue == nil
}
func hasString(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (s *Store) Result(id string) (Result, error) {
	snapshot, err := s.Load(id)
	if err != nil {
		return Result{}, err
	}
	refreshResult(&snapshot)
	return snapshot.Result, nil
}
func (s *Store) EvidenceLedger(id string) ([]Evidence, error) {
	snapshot, err := s.Load(id)
	if err != nil {
		return nil, err
	}
	refreshResult(&snapshot)
	if snapshot.Evidence == nil {
		return []Evidence{}, nil
	}
	return snapshot.Evidence, nil
}
func (s *Store) Report(id string) (string, error) {
	return s.ReportVersion(id, "")
}

func (s *Store) readReportSource(id string) (Snapshot, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	return s.verifySourceLocked(id, map[string]bool{}, readSourceOnly)
}

// Fixed exports render only the detached, revalidated snapshot. Ordinary
// reports also check the current external sources without writing them back.
func (s *Store) ReportVersion(id, hash string) (string, error) {
	snapshot, err := s.readReportSource(id)
	if err != nil {
		return "", err
	}
	if hash != "" {
		if snapshot.Run.Deleted {
			return "", os.ErrNotExist
		}
		if childReportHash(&snapshot) != hash {
			return "", fmt.Errorf("source report version changed or unavailable")
		}
		return renderReportSnapshot(&snapshot, nil, ChildReportReceipts(snapshot, 0)), nil
	}
	refreshUsage(&snapshot)
	chains := map[string]EvidenceChainEntry{}
	refs := append([]EvidenceChainReference(nil), snapshot.EvidenceChainRefs...)
	for _, chain := range snapshot.EvidenceChains {
		refs = append(refs, EvidenceChainReference{SourceRunID: id, ChainID: chain.ID, ChainHash: chain.ContentHash})
	}
	for _, ref := range refs {
		if entry, err := s.GetEvidenceChain(ref.SourceRunID, ref.ChainID); err == nil {
			chains[ref.SourceRunID+"/"+ref.ChainID] = entry
		}
	}
	reports, err := s.ChildReportsFrom(snapshot, 0)
	if err != nil {
		return "", err
	}
	return renderReportSnapshot(&snapshot, chains, reports), nil
}

// A nil chains map denotes a fixed export: local chain records are detached,
// external provenance remains explicitly unchecked. Keep this renderer free of
// Store/artifact I/O and derived-state refreshes.
func renderReportSnapshot(snapshot *Snapshot, chains map[string]EvidenceChainEntry, reports []ChildReport) string {
	result := snapshot.Result
	var b strings.Builder
	fmt.Fprintf(&b, "# Trace 研究报告\n\n## 研究结论\n\n状态：%s\n\n摘要：%s\n\n判定记录：%s\n\n清理状态：%s\n\n## 研究目标\n\n%s\n\n## 结论条目\n", reportStatus(result.Status), result.Summary, result.TerminalReason, reportStatus(result.CleanupStatus), snapshot.Run.Goal)
	if len(result.Answers) == 0 {
		b.WriteString("\n暂无结论条目。\n")
	}
	for i, a := range result.Answers {
		title, original := reportEvidenceText(a.Title, fmt.Sprintf("结论条目 %d", i+1), 80)
		fmt.Fprintf(&b, "\n### %s\n", title)
		if original != "" {
			fmt.Fprintf(&b, "\n原标题：%s\n", original)
		}
		if a.Summary != "" {
			fmt.Fprintf(&b, "\n简介：%s\n", a.Summary)
		}
		matched := "未通过"
		if a.Verified {
			matched = "通过"
		}
		fmt.Fprintf(&b, "\n原始值：%s\n\n类型：%s；输出匹配：%s；引用证据：%s\n", a.Value, a.Kind, matched, strings.Join(a.EvidenceIDs, ", "))
	}
	writeResultReferenceDiagnostics(&b, result)
	b.WriteString("\n## 复现步骤\n")
	for i, step := range result.ReproductionSteps {
		fmt.Fprintf(&b, "\n%d. %s\n", i+1, step)
	}
	b.WriteString("\n## 报告引用证据\n\n本节仅列结论、发现、复测及当前验收检查明确引用的记录。来源校验与业务判定分开，完整原始操作记录保留在 JSON 导出中。\n")
	cited := 0
	for _, e := range snapshot.Evidence {
		links := reportEvidenceLinks(snapshot, e.ID)
		if len(links) == 0 {
			continue
		}
		cited++
		fmt.Fprintf(&b, "\n### 证据 %s\n\n来源：%s；事件：%d；类型：%s；SHA-256：%s\n", e.ID, reportStatus(e.Verification), e.SourceEventSequence, e.Type, e.ContentHash)
		for _, link := range links {
			fmt.Fprintf(&b, "\n- 关联：%s（%s）；状态：%s。%s\n", link.Title, link.ID, reportStatus(link.Status), link.Summary)
		}
	}
	if cited == 0 {
		b.WriteString("\n暂无报告引用证据。\n")
	}
	b.WriteString("\n## 操作证据链\n\n整链引用保留原始步骤；来源完整不等于业务成功，引用不表示重新执行。\n")
	if chains == nil {
		b.WriteString("\n本固定版本只展示快照内链路及固定来源 ID/hash；链路与子报告的当前来源撤销状态需单独检查，不以固定摘要证明父运行执行。\n")
	}
	refs := append([]EvidenceChainReference(nil), snapshot.EvidenceChainRefs...)
	for _, c := range snapshot.EvidenceChains {
		refs = append(refs, EvidenceChainReference{SourceRunID: snapshot.Run.ID, ChainID: c.ID, ChainHash: c.ContentHash})
	}
	for _, ref := range refs {
		entry, found := chains[ref.SourceRunID+"/"+ref.ChainID]
		if chains == nil {
			if ref.SourceRunID != snapshot.Run.ID {
				fmt.Fprintf(&b, "\n- 固定外部引用：%s/%s；整链 SHA256：%s；来源状态：not_checked（需单独检查当前来源）。\n", ref.SourceRunID, ref.ChainID, ref.ChainHash)
				if ref.Purpose != "" {
					fmt.Fprintf(&b, "  支撑任务/结论：%s\n", ref.Purpose)
				}
				continue
			}
			for _, chain := range snapshot.EvidenceChains {
				if chain.ID == ref.ChainID {
					entry, found = EvidenceChainEntry{RunID: snapshot.Run.ID, Chain: chain, Verification: "not_checked"}, true
					break
				}
			}
		}
		if !found || entry.Chain.ContentHash != ref.ChainHash {
			fmt.Fprintf(&b, "\n- 链路 %s/%s：来源缺失或引用已过期。\n", ref.SourceRunID, ref.ChainID)
			continue
		}
		fmt.Fprintf(&b, "\n### %s\n\n%s\n\n来源：%s/%s；整链 SHA256：%s；来源状态：%s\n", entry.Chain.Name, entry.Chain.Summary, entry.RunID, entry.Chain.ID, ref.ChainHash, reportStatus(entry.Verification))
		if ref.Purpose != "" {
			fmt.Fprintf(&b, "\n支撑任务/结论：%s\n", ref.Purpose)
		}
		fmt.Fprintf(&b, "\n方法：%s；变体：%s\n\n适用条件：%s\n\n吸收经验：%s\n", entry.Chain.MethodID, entry.Chain.Variant, entry.Chain.Applicability, entry.Chain.Lesson)
		for i, step := range entry.Chain.Steps {
			fmt.Fprintf(&b, "\n%d. %s；来源 %s/%s；SHA256 %s\n", i+1, step.Label, step.RunID, step.EvidenceID, step.ContentHash)
		}
	}
	writeChildReports(&b, reports)
	usage, _ := json.MarshalIndent(snapshot.Run.Usage, "", "  ")
	if len(snapshot.Requirements) > 0 || len(snapshot.Findings) > 0 {
		b.WriteString("\n## 验收检查\n")
		for _, r := range snapshot.Requirements {
			c := latestCheck(snapshot, r.ID)
			state := "unknown"
			if c != nil {
				state = c.Status
			}
			fmt.Fprintf(&b, "\n- %s：%s；状态：%s\n", r.ID, r.Description, reportStatus(state))
			if c != nil {
				fmt.Fprintf(&b, "  引用证据：%s；判定记录：%s\n", strings.Join(c.EvidenceIDs, ", "), c.Reason)
			}
		}
		b.WriteString("\n## 发现与复测\n")
		for _, f := range snapshot.Findings {
			title, original := reportEvidenceText(f.Title, "发现："+f.ID, 80)
			fmt.Fprintf(&b, "\n### %s [%s]\n", title, f.Severity)
			if original != "" {
				fmt.Fprintf(&b, "\n原标题：%s\n", original)
			}
			fmt.Fprintf(&b, "\n状态：%s；来源：%s；处置：%s\n\n根因：%s\n\n影响：%s\n\n修复建议：%s\n\n引用证据：%s\n", reportStatus(f.Status), reportStatus(f.SourceStatus), reportStatus(f.Triage), f.RootCause, f.Impact, f.Remediation, strings.Join(f.EvidenceIDs, ", "))
			for _, step := range f.ReproductionSteps {
				fmt.Fprintf(&b, "\n- %s\n", step)
			}
			if f.Review != nil {
				fmt.Fprintf(&b, "\n模型审阅：%s — %s\n", reportStatus(f.Review.Status), f.Review.Reason)
			}
			for _, r := range f.Retests {
				fmt.Fprintf(&b, "\n实际复测：%s — %s（引用证据：%s）\n", reportStatus(r.Status), r.Reason, strings.Join(r.EvidenceIDs, ", "))
			}
		}
	}
	fmt.Fprintf(&b, "\n## 用量记录\n\n```json\n%s\n```\n", usage)
	return b.String()
}
