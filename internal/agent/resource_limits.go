package agent

import (
	"fmt"
	"strings"
)

// ResourceLimits records explicit creation configuration, never inferred goal text.
// Worker ceilings govern executor slots, not external instances or API calls.
type ResourceLimits struct {
	WorkerCeiling *WorkerCeiling `json:"worker_ceiling,omitempty"`
}

type WorkerCeiling struct {
	Capacity int    `json:"capacity"`
	Source   string `json:"source"`
	Scope    string `json:"scope"`
}

type WorkerCeilingConflictError struct {
	Capacity int
	Ceiling  WorkerCeiling
}

func (e *WorkerCeilingConflictError) Error() string {
	return fmt.Sprintf("worker capacity %d exceeds declared ceiling %d (source=%q scope=%q); reduce plan capacity before dispatch", e.Capacity, e.Ceiling.Capacity, e.Ceiling.Source, e.Ceiling.Scope)
}

func ValidateResourceLimits(limits *ResourceLimits) error {
	if limits == nil || limits.WorkerCeiling == nil {
		return nil
	}
	ceiling := limits.WorkerCeiling
	if ceiling.Capacity < 1 || ceiling.Capacity > 32 {
		return fmt.Errorf("worker ceiling capacity must be 1..32")
	}
	if strings.TrimSpace(ceiling.Source) == "" {
		return fmt.Errorf("worker ceiling source is required")
	}
	if ceiling.Scope != "root-tree" {
		return fmt.Errorf("worker ceiling scope must be root-tree")
	}
	return nil
}

func defaultWorkerCapacity(s *Snapshot) int {
	capacity := 5
	if s.Run.ParentTaskID == "" && s.Run.ResourceLimits != nil && s.Run.ResourceLimits.WorkerCeiling != nil {
		capacity = min(capacity, s.Run.ResourceLimits.WorkerCeiling.Capacity)
	}
	return capacity
}

func validatePlanWorkerCeiling(s *Snapshot, capacity int) error {
	if s.Run.ParentTaskID != "" {
		return nil
	}
	if err := ValidateResourceLimits(s.Run.ResourceLimits); err != nil {
		return err
	}
	if s.Run.ResourceLimits != nil && s.Run.ResourceLimits.WorkerCeiling != nil && capacity > s.Run.ResourceLimits.WorkerCeiling.Capacity {
		return &WorkerCeilingConflictError{Capacity: capacity, Ceiling: *s.Run.ResourceLimits.WorkerCeiling}
	}
	return nil
}
