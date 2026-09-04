package policy

import "olt-diagnostic-agent/internal/domain"

// Decision 是策略引擎对一次工具调用的评估结论：是否允许、是否需审批及原因。
type Decision struct {
	Allowed          bool   `json:"allowed"`
	ApprovalRequired bool   `json:"approvalRequired"`
	Reason           string `json:"reason"`
}

// Engine 基于工具注解计算审批策略：只读且非破坏、非敏感的工具直接放行，
// 其余一律要求人工审批。
type Engine struct{}

// NewEngine 创建一个策略引擎实例。
func NewEngine() *Engine {
	return &Engine{}
}

// Evaluate 根据工具注解返回决策：纯只读调用直接放行；可能改变外部状态、
// 暴露敏感数据或删除/覆盖外部状态的调用标记为需要审批，并给出原因。
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
