package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

func remainingDeclarations(s *Snapshot) []RemainingWork {
	items, _ := collectRemainingDeclarations(s)
	return items
}

// Read entries separately so a malformed sibling cannot erase an obligation.
// Nonempty identity fields stay frozen; a later matching declaration may fill
// missing fields and repair evidence references or a duplicate entry.
func collectRemainingDeclarations(s *Snapshot) ([]RemainingWork, []ResultValidationIssue) {
	var items []RemainingWork
	positions := map[string]int{}
	known := map[string]RemainingWork{}
	invalid := map[string]ResultValidationIssue{}
	anonymous := map[string]bool{}
	var order []string
	record := func(key, field, reason string) {
		if _, exists := invalid[key]; !exists {
			order = append(order, key)
		}
		invalid[key] = ResultValidationIssue{Code: "invalid_remaining_work", Field: "trace_result.remaining", Blocking: true,
			Repair: fmt.Sprintf("%s: %s; restate the ID with the same nonempty goal and next_action; for an unidentified entry or invalid array, explicitly restate the complete remaining array including every known obligation", field, reason)}
	}
	for mi, message := range s.Messages {
		if message.Role != "assistant" || message.ReportOnly {
			continue
		}
		claim, _ := traceEnvelope[map[string]json.RawMessage](message.Content, "trace_result")
		if claim == nil {
			continue
		}
		raw, present := (*claim)["remaining"]
		if !present {
			continue
		}
		field := fmt.Sprintf("messages[%d].trace_result.remaining", mi)
		var entries []json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil || !strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			anonymous[field] = true
			record(field, field, "remaining must be an array")
			continue
		}
		seen := map[string]bool{}
		complete := true
		for i, entry := range entries {
			location := fmt.Sprintf("%s[%d]", field, i)
			var item RemainingWork
			err := json.Unmarshal(entry, &item)
			key := item.ID
			if strings.TrimSpace(key) == "" {
				key = location
				anonymous[key] = true
			}
			duplicate := seen[key]
			seen[key] = true
			previous, exists := known[key]
			conflict := exists && (strings.TrimSpace(previous.Goal) != "" && item.Goal != previous.Goal || strings.TrimSpace(previous.NextAction) != "" && item.NextAction != previous.NextAction)
			if !conflict {
				if strings.TrimSpace(previous.Goal) == "" {
					previous.Goal = item.Goal
				}
				if strings.TrimSpace(previous.NextAction) == "" {
					previous.NextAction = item.NextAction
				}
				known[key] = previous
			}
			switch {
			case err != nil:
				complete = false
				record(key, location, "remaining entry has invalid field types")
				continue
			case validateRemainingShape([]RemainingWork{item}) != nil:
				complete = false
				record(key, location, "remaining entry needs an ID, goal and next_action")
				continue
			case conflict:
				complete = false
				record(key, location, "remaining entry changed an existing goal or next_action")
				continue
			}
			if position, exists := positions[item.ID]; exists {
				items[position].EvidenceIDs = append([]string(nil), item.EvidenceIDs...)
			} else {
				positions[item.ID] = len(items)
				items = append(items, cloneRemainingWork([]RemainingWork{item})[0])
			}
			if duplicate {
				complete = false
				record(key, location, "remaining entry repeats an ID in the same declaration")
			} else {
				delete(invalid, key)
			}
		}
		for _, item := range items {
			complete = complete && seen[item.ID]
		}
		for key := range invalid {
			if !anonymous[key] && !seen[key] {
				complete = false
			}
		}
		if complete {
			for key := range anonymous {
				delete(invalid, key)
				delete(anonymous, key)
			}
		}
	}
	var issues []ResultValidationIssue
	emitted := map[string]bool{}
	for _, key := range order {
		if issue, exists := invalid[key]; exists && !emitted[key] {
			issues = append(issues, issue)
			emitted[key] = true
		}
	}
	return items, issues
}
