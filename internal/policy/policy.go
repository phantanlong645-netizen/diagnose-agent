package policy

import "olt-diagnostic-agent/internal/domain"

type Decision struct {
	Allowed          bool   `json:"allowed"`
	ApprovalRequired bool   `json:"approvalRequired"`
	Reason           string `json:"reason"`
}

type Engine struct{}

func NewEngine() *Engine {
	return &Engine{}
}

func (e *Engine) Evaluate(call domain.PreparedCall) Decision {
	annotations := call.Annotations
	if annotations.ReadOnly && !annotations.Destructive && !annotations.Sensitive {
		return Decision{
			Allowed: true,
			Reason:  "read-only operation",
		}
	}

	reason := "operation may change external state"
	if annotations.Sensitive {
		reason = "operation may expose sensitive local data"
	} else if annotations.Destructive {
		reason = "operation may delete or overwrite external state"
	} else if annotations.OpenWorld {
		reason = "operation interacts with an external system"
	}

	return Decision{
		Allowed:          true,
		ApprovalRequired: true,
		Reason:           reason,
	}
}
