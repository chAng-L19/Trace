package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func findingSubject(f Finding) any {
	return struct {
		ID, Title, Severity, RootCause, Impact, Remediation string
		Steps                                               []string
		Predicate                                           EvidencePredicate
	}{f.ID, f.Title, f.Severity, f.RootCause, f.Impact, f.Remediation, f.ReproductionSteps, f.Predicate}
}

func findingByID(s *Snapshot, id string) *Finding {
	for i := range s.Findings {
		if s.Findings[i].ID == id {
			return &s.Findings[i]
		}
	}
	return nil
}

func (a *Agent) SubmitFinding(id string, f Finding) (Snapshot, error) {
	if strings.TrimSpace(f.ID) == "" || strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.RootCause) == "" || strings.TrimSpace(f.Impact) == "" || strings.TrimSpace(f.Remediation) == "" || len(f.ReproductionSteps) == 0 {
		return Snapshot{}, fmt.Errorf("finding requires ID, title, root cause, impact, remediation and reproduction steps")
	}
	switch f.Severity {
	case "critical", "high", "medium", "low", "info":
	default:
		return Snapshot{}, fmt.Errorf("invalid finding severity")
	}
	if err := validatePredicate(f.Predicate); err != nil {
		return Snapshot{}, err
	}
	return a.store.Update(id, func(s *Snapshot) error {
		refs, err := evidenceReferences(s, f.EvidenceIDs)
		if err != nil {
			return err
		}
		if !findingObservations(s, f.EvidenceIDs) {
			return fmt.Errorf("finding requires actual observation actions; bookkeeping output is not impact evidence")
		}
		f.References = refs
		f.Review = nil
		f.Retests = nil
		f.Status = "submitted"
		f.SourceStatus = "verified"
		f.Triage = "open"
		f.CreatedSequence = s.Run.EventSeq + 1
		f.UpdatedAt = time.Now().UTC()
		if old := findingByID(s, f.ID); old != nil {
			f.Retests = old.Retests
			f.Triage = old.Triage
			*old = f
		} else {
			s.Findings = append(s.Findings, f)
		}
		addEvent(s, "finding_submitted", "发现已留档，等待审阅及独立复测", map[string]any{"id": f.ID, "subject_hash": valueHash(findingSubject(f)), "evidence": refs})
		return nil
	})
}

func (a *Agent) ReviewFinding(ctx context.Context, id, findingID string) (Snapshot, error) {
	s, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	f := findingByID(&s, findingID)
	if f == nil {
		return Snapshot{}, fmt.Errorf("finding not found")
	}
	hash := valueHash(findingSubject(*f))
	review, err := a.assessEvidence(ctx, id, findingSubject(*f), f.EvidenceIDs)
	if err != nil {
		return Snapshot{}, err
	}
	return a.store.Update(id, func(s *Snapshot) error {
		current := findingByID(s, findingID)
		if current == nil || valueHash(findingSubject(*current)) != hash {
			return fmt.Errorf("finding changed during review")
		}
		current.Review = review
		refreshFindings(s)
		addEvent(s, "finding_reviewed", "发现审阅已记录；审阅与复现分开", map[string]any{"id": findingID, "request_id": review.RequestID, "status": review.Status})
		return nil
	})
}

func (a *Agent) RetestFinding(id, findingID string, ids []string) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		f := findingByID(s, findingID)
		if f == nil {
			return fmt.Errorf("finding not found")
		}
		refs, err := evidenceReferences(s, ids)
		if err != nil {
			return err
		}
		if !sameFindingProbes(s, *f, ids) {
			return fmt.Errorf("retest must execute the original observation probes with the same arguments, adapter and command")
		}
		status, reason := checkPredicate(s, f.Predicate, ids, "", "", f.CreatedSequence)
		if status == "unknown" {
			return fmt.Errorf("%s", reason)
		}
		for _, r := range refs {
			for _, old := range f.References {
				if r.ID == old.ID {
					return fmt.Errorf("original finding evidence is not a new retest")
				}
			}
		}
		f.Retests = append(f.Retests, FindingRetest{EvidenceIDs: ids, References: refs, FindingHash: valueHash(findingSubject(*f)), Status: status, Reason: reason, CreatedAt: time.Now().UTC()})
		refreshFindings(s)
		addEvent(s, "finding_retested", "发现复测执行记录已关联", map[string]any{"id": findingID, "status": status, "evidence": refs})
		return nil
	})
}

func (a *Agent) TriageFinding(id, findingID, triage string) (Snapshot, error) {
	switch triage {
	case "open", "accepted", "fixed", "dismissed":
	default:
		return Snapshot{}, fmt.Errorf("invalid finding triage")
	}
	return a.store.Update(id, func(s *Snapshot) error {
		f := findingByID(s, findingID)
		if f == nil {
			return fmt.Errorf("finding not found")
		}
		f.Triage = triage
		f.UpdatedAt = time.Now().UTC()
		addEvent(s, "finding_triaged", "发现处置状态已记录", map[string]string{"id": findingID, "triage": triage})
		return nil
	})
}

func refreshFindings(s *Snapshot) {
	for i := range s.Findings {
		f := &s.Findings[i]
		f.SourceStatus = "verified"
		f.Status = "submitted"
		if !referencesCurrent(s, f.References) {
			f.SourceStatus = "invalid"
			f.Status = "invalid"
			continue
		}
		hash := valueHash(findingSubject(*f))
		if reviewCurrent(s, f.Review, hash, f.References) {
			switch f.Review.Status {
			case "pass":
				f.Status = "reviewed"
			case "fail":
				f.Status = "rejected"
			default:
				f.Status = "needs_evidence"
			}
		}
		for j := range f.Retests {
			r := &f.Retests[j]
			if r.FindingHash != hash || !referencesCurrent(s, r.References) || !sameFindingProbes(s, *f, r.EvidenceIDs) {
				r.Status = "unknown"
				r.Reason = "finding or retest evidence changed"
				continue
			}
			r.Status, r.Reason = checkPredicate(s, f.Predicate, r.EvidenceIDs, "", "", f.CreatedSequence)
		}
		if f.Status == "reviewed" && len(f.Retests) > 0 && f.Retests[len(f.Retests)-1].Status == "pass" {
			f.Status = "reproduced"
		}
	}
}

func evidenceAction(s *Snapshot, id string) *Action {
	for _, e := range s.Evidence {
		if e.ID != id {
			continue
		}
		for i := range s.Actions {
			a := &s.Actions[i]
			if a.CallID == e.CallID && a.RequestID == e.RequestID {
				return a
			}
		}
	}
	return nil
}
func findingObservations(s *Snapshot, ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		a := evidenceAction(s, id)
		if a == nil || !observationAction(*a) {
			return false
		}
	}
	return true
}
func findingProbeHash(a Action) string {
	return valueHash(struct {
		Name, Arguments, Adapter, ServerName, RemoteName, RemoteMethod, LocalName string
		Command                                                                   []string
	}{a.Name, a.Arguments, a.Adapter, a.ServerName, a.RemoteName, a.RemoteMethod, a.LocalName, a.Command})
}
func sameFindingProbes(s *Snapshot, f Finding, ids []string) bool {
	if !findingObservations(s, ids) {
		return false
	}
	want := map[string]bool{}
	for _, id := range f.EvidenceIDs {
		a := evidenceAction(s, id)
		if a == nil || !observationAction(*a) {
			return false
		}
		want[findingProbeHash(*a)] = true
	}
	got := map[string]bool{}
	for _, id := range ids {
		a := evidenceAction(s, id)
		if actionStartSequence(s, *a) <= f.CreatedSequence {
			return false
		}
		key := findingProbeHash(*a)
		if !want[key] {
			return false
		}
		got[key] = true
	}
	return len(got) == len(want)
}
