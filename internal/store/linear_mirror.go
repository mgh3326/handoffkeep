package store

// Mirror reasons are explicit task policy recorded through authenticated hk
// writes. A title, job, placement, link or incoming webhook body never grants
// eligibility by itself. App intake must pass operatorActed before recording
// LinearMirrorOpened; the request producer records LinearMirrorDots.
const (
	LinearMirrorDots            = "dots"
	LinearMirrorOperatorVisible = "operator_visible"
	LinearMirrorOpened          = "linear_opened"
)

func validLinearMirrorReason(reason string) bool {
	switch reason {
	case "", LinearMirrorDots, LinearMirrorOperatorVisible, LinearMirrorOpened:
		return true
	}
	return false
}

// LinearMirrorReason is the shared creation admission policy. Existing issue
// maintenance does not use this to create replacements. Priority zero is hk's
// unset default; only explicit priorities 1 and 2 admit ordinary tasks.
func LinearMirrorReason(task Task) string {
	if IsHKDotsRequest(task) {
		return ""
	}
	if IsSettledState(task.State) {
		return ""
	}
	if task.Refs.Linear != nil {
		if reason := task.Refs.Linear.MirrorReason; reason != "" && validLinearMirrorReason(reason) {
			return reason
		}
		// The legacy explicit opt-in remains an operator-visible request,
		// not an inference from having been claimed or placed.
		if task.Refs.Linear.Sync {
			return LinearMirrorOperatorVisible
		}
	}
	if request := task.Refs.DecisionRequest; request != nil && request.Status == DecisionRequestOpen {
		return "decision"
	}
	if task.Priority >= 1 && task.Priority <= 2 {
		return "priority"
	}
	return ""
}
