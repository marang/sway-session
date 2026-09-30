package session

// RestorePolicyDecision describes next-login eligibility, not whether a launch
// or application-internal recovery will succeed. It is derived from the sole
// authoritative lifecycle and desired-open fields.
type RestorePolicyDecision struct {
	Eligible bool   `json:"eligible"`
	Reason   string `json:"reason"`
}

func EvaluateRestorePolicy(context Context) RestorePolicyDecision {
	if context.State != ContextActive {
		return RestorePolicyDecision{Reason: "archived"}
	}
	if context.App == nil {
		return RestorePolicyDecision{Eligible: true, Reason: "active_terminal"}
	}
	if !context.App.DesiredOpen {
		return RestorePolicyDecision{Reason: "desired_closed"}
	}
	if context.App.RestorePolicy == ApplicationRestorePinned {
		return RestorePolicyDecision{Eligible: true, Reason: "desired_open_pinned"}
	}
	return RestorePolicyDecision{Eligible: true, Reason: "desired_open_follow"}
}

func RestorePolicyExplanation(reason string) string {
	switch reason {
	case "active_terminal":
		return "active terminal will reopen"
	case "archived":
		return "archived; automatic restore disabled"
	case "desired_closed":
		return "application is not desired open"
	case "desired_open_pinned":
		return "pinned application is desired open"
	case "desired_open_follow":
		return "follow application is desired open"
	default:
		return "restore policy unknown"
	}
}
