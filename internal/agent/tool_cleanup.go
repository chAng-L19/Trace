package agent

import (
	"errors"
	"fmt"
)

// Manual outcome submission resolves observations, not process cleanup.
func toolCleanupError(s Snapshot) error {
	var result error
	for _, action := range s.Actions {
		if action.ExecutorOwnershipError != "" {
			result = errors.Join(result, fmt.Errorf("tool %s/%s executor ownership: %s", action.RequestID, action.CallID, action.ExecutorOwnershipError))
		}
		if (action.Adapter == "exec" || action.Adapter == "local") && action.Result == nil && action.ResourceCleanupStatus != "completed" {
			result = errors.Join(result, fmt.Errorf("tool %s/%s has no durable process cleanup observation", action.RequestID, action.CallID))
		}
		if action.Result != nil && len(action.Result.WorkerCleanupError) > 0 && action.ResourceCleanupStatus != "completed" {
			result = errors.Join(result, fmt.Errorf("tool %s/%s process cleanup: %s", action.RequestID, action.CallID, action.Result.WorkerCleanupError))
		}
		if action.ResourceCleanupError != "" {
			result = errors.Join(result, fmt.Errorf("tool %s/%s resource cleanup: %s", action.RequestID, action.CallID, action.ResourceCleanupError))
		}
	}
	return result
}
