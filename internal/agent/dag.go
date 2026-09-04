package agent

import (
	"context"
	"fmt"
)

// DAGStep 是依赖图中的一个可执行单元。Work 返回非 nil error 表示该步骤失败，
// 调度器会据此把它的所有下游步骤标记为 SKIP 而不是继续执行。
type DAGStep struct {
	ID        string
	DependsOn []string
	Work      func(ctx context.Context) error
}

// DAGResult 是单个步骤的调度结果。Err 非空表示执行失败；Skipped 为 true 时
// SkipFrom 记录导致它被跳过的那个失败上游步骤 ID。
type DAGResult struct {
	ID       string
	Err      error
	Skipped  bool
	SkipFrom string
}

// dagNodeState 是调度器中单个节点的生命周期状态。
// 它只由 dispatcher goroutine 独占读写，因此不需要额外加锁。
type dagNodeState int

const (
	// dagPending 初始状态：依赖尚未全部完成，等待被调度。
	dagPending dagNodeState = iota
	// dagRunning 已满足依赖条件，正在并发执行中。
	dagRunning
	// dagDone 正常执行完成（无论 Work 是否返回错误，完成即置为 Done）。
	dagDone
	// dagSkipped 因某个上游步骤失败而被跳过（含间接依赖）。
	dagSkipped
)

// dagNode 是依赖图中的一个内部节点，由 ScheduleDAG 从 DAGStep 构建而来。
// 与 DAGStep（公共输入类型）相比，它额外维护调度器运行期所需的图元信息：
//   - dependents：依赖本节点的下游节点 ID 列表（用于失败时级联 skip、成功时入度减一）
//   - indegree：还剩多少个上游依赖未完成（为 0 且处于 pending 时才能被调度）
//   - state：当前生命周期状态
//   - work：真正的执行函数
type dagNode struct {
	id         string
	dependents []string
	indegree   int
	state      dagNodeState
	work       func(ctx context.Context) error
}

// dagCompletion 是 worker goroutine 执行完一个节点后通过 channel 回传给
// dispatcher 的完成通知，携带节点 ID 与执行结果错误。
// 之所以用独立类型而不是直接传 DAGResult：worker 阶段还不清楚"是否被跳过"，
// 跳过标记由 dispatcher 在收到完成通知后统一判定。
type dagCompletion struct {
	id  string
	err error
}

// ScheduleDAG 按拓扑顺序调度一组带依赖关系的步骤：
//
//   - 依赖满足的独立步骤并发执行，并发上限为 maxParallel（<=0 时退化为 1）。
//   - 某步骤失败后，所有（直接或间接）依赖它的下游步骤被标记为 Skipped。
//   - 存在环或不可满足的依赖时返回 error，避免死锁。
//   - ctx 取消时返回 ctx.Err()。
//
// 返回按 step.ID 索引的结果 map。调度器内部由单一 dispatcher goroutine 持有
// 全部节点状态，避免并发更新 indegree / 结果 map 的竞态。
func ScheduleDAG(ctx context.Context, steps []DAGStep, maxParallel int) (map[string]DAGResult, error) {
	if maxParallel <= 0 {
		maxParallel = 1
	}
	if len(steps) == 0 {
		return map[string]DAGResult{}, nil
	}

	nodes := make(map[string]*dagNode, len(steps))
	order := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.ID == "" {
			return nil, fmt.Errorf("DAG step id is required")
		}
		if _, exists := nodes[step.ID]; exists {
			return nil, fmt.Errorf("duplicate DAG step id: %s", step.ID)
		}
		nodes[step.ID] = &dagNode{id: step.ID, work: step.Work}
		order = append(order, step.ID)
	}
	for _, step := range steps {
		node := nodes[step.ID]
		for _, dep := range step.DependsOn {
			depNode, exists := nodes[dep]
			if !exists {
				return nil, fmt.Errorf("DAG step %s depends on unknown step %s", step.ID, dep)
			}
			node.indegree++
			depNode.dependents = append(depNode.dependents, step.ID)
		}
	}

	results := make(map[string]DAGResult, len(steps))
	// 缓冲到步骤数，保证 dispatcher 提前退出（ctx 取消 / 环）时，还在跑的
	// worker 不会阻塞在 channel 上。
	completed := make(chan dagCompletion, len(steps))
	done := make(chan struct{})
	var runErr error

	go func() {
		defer close(done)

		active := 0
		// skip 递归地把"以 id 为起点的所有下游"标记为 Skipped。
		// 已处于 Done/Skipped 的节点直接返回，避免重复标记；SkipFrom 记录触发跳过的
		// 失败上游 ID，方便调用方定位责任链。
		var skip func(id, from string)
		skip = func(id, from string) {
			node := nodes[id]
			if node.state == dagDone || node.state == dagSkipped {
				return
			}
			node.state = dagSkipped
			results[id] = DAGResult{ID: id, Skipped: true, SkipFrom: from}
			for _, dep := range node.dependents {
				skip(dep, from)
			}
		}

		// scheduleReady 扫描全部节点，把"空闲且入度清零"的节点启动为 worker。
		// 受 maxParallel 限制：一旦在跑数量达到上限立即返回，等下一个完成通知再来补位。
		var scheduleReady func()
		scheduleReady = func() {
			for _, id := range order {
				if active >= maxParallel {
					return
				}
				node := nodes[id]
				if node.state != dagPending || node.indegree != 0 {
					continue
				}
				node.state = dagRunning
				active++
				go func(n *dagNode) {
					err := runDAGWork(ctx, n)
					completed <- dagCompletion{id: n.id, err: err}
				}(node)
			}
		}

		scheduleReady()
		for {
			// active == 0 时若还有未完成节点，说明存在环或不可满足的依赖，
			// 永远不会有新的完成通知到来，必须主动报错退出，防止死锁。
			if active == 0 {
				remaining := 0
				for _, id := range order {
					switch nodes[id].state {
					case dagDone, dagSkipped:
					default:
						remaining++
					}
				}
				if remaining == 0 {
					return
				}
				runErr = fmt.Errorf("DAG schedule stalled: %d step(s) have unsatisfied dependencies (possible cycle)", remaining)
				return
			}

			select {
			case completion := <-completed:
				// 收到一个 worker 完成通知：先把它标记为 Done，
				// 然后按结果做两种处理：
				//   1) 出错：结果记录 Err，并级联 skip 所有下游；
				//   2) 成功：把每个下游的 indegree 减一（释放一个依赖槽位），
				//      跳过已被 skip 的下游（防止把错误状态再拉回可调度）。
				active--
				node := nodes[completion.id]
				node.state = dagDone
				if completion.err != nil {
					results[completion.id] = DAGResult{ID: completion.id, Err: completion.err}
					for _, dep := range node.dependents {
						skip(dep, completion.id)
					}
				} else {
					results[completion.id] = DAGResult{ID: completion.id}
					for _, dep := range node.dependents {
						depNode := nodes[dep]
						if depNode.state == dagSkipped {
							continue
						}
						depNode.indegree--
					}
				}
				scheduleReady()
			case <-ctx.Done():
				// 调用方取消：记录 ctx.Err() 并立即退出，不再调度新节点。
				runErr = ctx.Err()
				return
			}
		}
	}()

	<-done
	if runErr != nil {
		return nil, runErr
	}
	return results, nil
}

// runDAGWork 执行单个节点的 work 函数，并把 panic 转换为携带节点 ID 的错误返回，
// 保证调度器不会因某个步骤的 panic 而整体崩溃。
func runDAGWork(ctx context.Context, node *dagNode) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("DAG step %s panicked: %v", node.id, recovered)
		}
	}()
	if node.work == nil {
		return fmt.Errorf("DAG step %s has no work function", node.id)
	}
	return node.work(ctx)
}
