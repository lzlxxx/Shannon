package workflows

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/Kocoro-lab/Shannon/go/orchestrator/internal/activities"
	"github.com/Kocoro-lab/Shannon/go/orchestrator/internal/constants"
	ometrics "github.com/Kocoro-lab/Shannon/go/orchestrator/internal/metrics"
	"github.com/Kocoro-lab/Shannon/go/orchestrator/internal/roles"
	"github.com/Kocoro-lab/Shannon/go/orchestrator/internal/templates"
	"github.com/Kocoro-lab/Shannon/go/orchestrator/internal/workflows/opts"
	"github.com/Kocoro-lab/Shannon/go/orchestrator/internal/workflows/strategies"
)

const (
	// DefaultReviewTimeout 表示“研究计划等待人工审核”这一步的默认超时时间。
	// 当上游没有在上下文里显式传入 review_timeout 时，编排器就使用这个值作为兜底值。
	// 这里设为 15 分钟，是因为前端和文档都把人工确认窗口控制在一个较短、可接受的交互时长内。
	DefaultReviewTimeout = 15 * time.Minute
)

// OrchestratorWorkflow 是整个编排层的总入口函数。
// 它自己的职责很“薄”：先根据输入做少量预处理和路由判断，再把任务委派给更具体的子工作流。
// 它并不直接驱动具体 agent 执行，而是负责以下几类事情：
// 1. 初始化日志、事件流、控制信号处理器。
// 2. 按版本门和上下文决定是否走模板、学习路由、强制 research / swarm / browser 等快捷路径。
// 3. 在需要时调用分解 Activity，把原始问题转换成 subtasks 和路由策略。
// 4. 在真正下发执行前补齐预算、审批、元数据和前端事件。
// 5. 统一把结果包装成 TaskResult 返回给调用方。
//
// 参数说明：
// ctx：Temporal 的工作流上下文。所有日志、Activity、子工作流、Signal、Timer 都必须从这里派生，
// 才能保证工作流可重放且保持确定性。
// input：本次任务的完整输入，里面包含用户问题、租户信息、历史消息、上下文字段、审批要求等。
//
// 返回值说明：
// TaskResult：业务层的执行结果。很多“业务失败”不会作为 Go error 抛出，而是体现在 Success=false。
// error：Temporal/工作流层面的错误，例如 Activity 执行失败、子工作流失败、取消等。
func OrchestratorWorkflow(ctx workflow.Context, input TaskInput) (TaskResult, error) {
	// 从 Temporal 上下文中取出 logger，后续整个编排流程统一使用这一个日志对象记录关键状态。
	logger := workflow.GetLogger(ctx)
	// 读取当前工作流实例的唯一 ID，后面发事件、注册子工作流、等待信号时都要用到它。
	workflowID := workflow.GetInfo(ctx).WorkflowExecution.ID

	// 记录工作流启动日志，便于从 Temporal 历史和日志平台看到本次请求的基本上下文。
	logger.Info("Starting OrchestratorWorkflow",
		"query", input.Query, // query：用户原始问题或任务描述。
		"user_id", input.UserID, // user_id：发起本次请求的用户 ID。
		"session_id", input.SessionID, // session_id：当前对话或任务会话 ID。
	)

	// emitCtx 是“发事件专用”的 Activity 上下文。
	// 这里把超时时间设得很短、且不重试，是因为 SSE/事件流通知属于尽力而为，不应该拖慢主流程。
	emitCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,                           // 单次发事件最长允许执行 5 秒。
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1}, // 发事件失败不重试，避免重复通知。
	})

	// controlHandler 负责统一处理 pause / resume / cancel 等控制信号，
	// 并负责登记当前编排器启动过的子工作流，便于在父级收到控制命令时向下游同步。
	controlHandler := &ControlSignalHandler{
		WorkflowID: workflowID,     // WorkflowID：当前父工作流 ID，供控制事件和状态同步使用。
		AgentID:    "orchestrator", // AgentID：事件流里标识“这条消息来自 orchestrator”。
		Logger:     logger,         // Logger：复用上面取出的工作流日志器。
		EmitCtx:    emitCtx,        // EmitCtx：控制事件在发送 SSE 时使用的短超时上下文。
	}
	controlHandler.Setup(ctx) // 注册控制信号监听逻辑，让后续 pause/cancel 能真正生效。

	// 向前端/事件流发出“工作流已启动”的通知。
	// 这一步即使失败也不影响主逻辑，所以只记录 warning，不中断编排。
	if err := workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
		WorkflowID: workflowID,                            // WorkflowID：告诉事件系统是哪一个工作流发出的通知。
		EventType:  activities.StreamEventWorkflowStarted, // EventType：事件类型是“工作流开始”。
		AgentID:    "orchestrator",                        // AgentID：事件来源标记为 orchestrator。
		Message:    activities.MsgWorkflowStarted(),       // Message：给前端展示的人类可读文案。
		Timestamp:  workflow.Now(ctx),                     // Timestamp：使用 workflow.Now 保证 Temporal 重放时仍然确定。
		Payload: map[string]interface{}{
			"task_context": input.Context, // task_context：把原始上下文透传给前端，便于界面展示任务状态。
		},
	}).Get(ctx, nil); err != nil {
		logger.Warn("Failed to emit workflow started event", "error", err) // 发启动事件失败仅记警告，不阻塞主流程。
	}

	// titleGateVersion 是一个版本门，用来告诉 Temporal：
	// “标题是否需要生成”的判定逻辑从这个 changeID 开始有了新版本。
	// 这样老工作流在重放历史时仍然走旧逻辑，新工作流才会走新逻辑。
	titleGateVersion := workflow.GetVersion(ctx, "title_gate_v2", workflow.DefaultVersion, 1)
	needsTitle := true // 默认认为需要生成标题，后面再根据上下文把它置为 false。
	if titleGateVersion >= 1 {
		// 新逻辑：优先从 SessionCtx 判断当前会话是否已有标题。
		// 这种方式比“看历史消息长度”更可靠，因为历史消息不一定能准确代表标题状态。
		if input.SessionCtx != nil {
			if existingTitle, ok := input.SessionCtx["title"].(string); ok && existingTitle != "" {
				needsTitle = false // 只要 SessionCtx.title 已存在且非空，就跳过异步标题生成。
			}
		}
	} else {
		// 旧逻辑：历史消息存在时，认为这个会话大概率已经建立过标题。
		// 这段代码保留是为了让旧工作流在回放时仍然能得到和历史一致的执行结果。
		if len(input.History) > 0 {
			needsTitle = false // 历史不为空时，不再重复生成标题。
		}
	}
	if needsTitle {
		startAsyncTitleGeneration(ctx, input.SessionID, input.Query) // 非阻塞地后台生成会话标题，提升前端早期体验。
	}

	// actx 是“普通快速规划类 Activity”使用的上下文。
	// 相比 emitCtx，这里给更长超时和有限重试，因为配置读取、任务分解等步骤属于主流程。
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 60 * time.Second,                          // 允许此类 Activity 最长跑 60 秒。
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3}, // 失败时最多尝试 3 次，提高稳健性。
	})

	// cfg 保存路由、预算、审批、模板回退等开关配置。
	// 这类配置通常来自外部配置中心或 Activity 的运行时加载结果。
	var cfg activities.WorkflowConfig
	if err := workflow.ExecuteActivity(actx, activities.GetWorkflowConfig).Get(ctx, &cfg); err != nil {
		// 配置读取失败时不直接中止，而是继续走代码里的默认值，保证编排器具备兜底能力。
	}
	simpleThreshold := cfg.SimpleThreshold // 读取“判定为简单任务”的复杂度阈值。
	if simpleThreshold == 0 {
		simpleThreshold = 0.3 // 如果配置未设置阈值，则使用经验默认值 0.3。
	}

	// templateVersionGate 用来保护“模板路由”这段新逻辑，避免影响旧工作流回放。
	templateVersionGate := workflow.GetVersion(ctx, "template_router_v1", workflow.DefaultVersion, 1)
	var templateEntry templates.Entry                          // templateEntry：命中模板后保存模板注册表里的完整条目。
	templateFound := false                                     // templateFound：是否真的在注册表里找到了请求的模板。
	templateRequested := false                                 // templateRequested：输入中是否明确表达了“想走模板”。
	var requestedTemplateName, requestedTemplateVersion string // 保存用户请求的模板名和模板版本。
	if templateVersionGate >= 1 {
		requestedTemplateName, requestedTemplateVersion = extractTemplateRequest(input) // 从 input 和 context 中统一提取模板请求信息。
		if requestedTemplateName != "" {
			templateRequested = true // 只要模板名非空，就说明调用方显式提出了模板执行意图。
			if entry, ok := TemplateRegistry().Find(requestedTemplateName, requestedTemplateVersion); ok {
				templateEntry = entry // 保存命中的模板条目，后面直接交给 TemplateWorkflow 使用。
				templateFound = true  // 标记模板解析成功。
				if input.Context == nil {
					input.Context = map[string]interface{}{} // 确保 Context 非空，便于把模板信息写回去。
				}
				input.Context["template_resolved"] = entry.Key             // template_resolved：最终解析到的注册表键值。
				input.Context["template_content_hash"] = entry.ContentHash // template_content_hash：模板内容哈希，用于追踪版本。
			}
		}
		if input.DisableAI && !templateFound {
			msg := fmt.Sprintf("requested template '%s' not found", requestedTemplateName) // 默认错误信息：调用方指定了模板，但没有找到。
			if requestedTemplateName == "" {
				msg = "template execution required but no template specified" // 更具体的错误信息：要求走模板，但连模板名都没给。
			}
			logger.Error("Template requirement cannot be satisfied",
				"template", requestedTemplateName, // template：记录请求的模板名。
				"version", requestedTemplateVersion, // version：记录请求的模板版本。
			)
			return TaskResult{
				Success:      false, // Success=false：业务上明确失败。
				ErrorMessage: msg,   // ErrorMessage：把失败原因返回给调用方。
				Metadata: map[string]interface{}{
					"template_requested": requestedTemplateName,    // template_requested：原始请求的模板名。
					"template_version":   requestedTemplateVersion, // template_version：原始请求的模板版本。
				},
			}, nil
		}
		if templateRequested && !templateFound {
			logger.Warn("Requested template not found; continuing with heuristic routing",
				"template", requestedTemplateName, // 记录未命中的模板名，便于排查配置或拼写问题。
				"version", requestedTemplateVersion, // 记录未命中的模板版本。
			)
		}
	}

	// learningVersionGate 保护“持续学习路由”能力。
	// 如果系统已经从历史任务里学到某个用户更适合某种策略，就会优先尝试直接路由。
	learningVersionGate := workflow.GetVersion(ctx, "learning_router_v1", workflow.DefaultVersion, 1)
	if learningVersionGate >= 1 && !templateFound && cfg.ContinuousLearningEnabled {
		if rec, err := recommendStrategy(ctx, input); err == nil && rec != nil && rec.Strategy != "" {
			if input.Context == nil {
				input.Context = map[string]interface{}{} // 确保 Context 非空，后面写入学习路由结果。
			}
			input.Context["learning_strategy"] = rec.Strategy     // learning_strategy：学习路由推荐出的策略名。
			input.Context["learning_confidence"] = rec.Confidence // learning_confidence：推荐置信度。
			if rec.Source != "" {
				input.Context["learning_source"] = rec.Source // learning_source：推荐来源，例如规则、历史样本等。
			}
			if result, handled, err := routeStrategyWorkflow(ctx, input, rec.Strategy, "learning", emitCtx, controlHandler); handled {
				return result, err // 如果学习路由已经成功接管，就直接返回，不再继续后续大路由逻辑。
			}
			logger.Warn("Learning router returned unknown strategy", "strategy", rec.Strategy) // 推荐了未知策略时仅警告，继续走正常路由。
		}
	}

	// 在真正进入复杂路由前，先检查一次 pause/cancel。
	// 这样用户如果刚好在初始化阶段发了暂停或取消，就不会继续创建子工作流。
	if err := controlHandler.CheckPausePoint(ctx, "pre_routing"); err != nil {
		return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 同时返回业务失败结果和底层 error，方便上层区分。
	}

	// 如果已经找到模板，则优先直接走 TemplateWorkflow。
	// 这是最强约束的路由方式，因为模板本身已经把执行骨架定义清楚了，不需要再做 LLM 分解。
	if templateFound {
		input.TemplateName = templateEntry.Template.Name       // 把模板真实名称回填到输入，供子工作流直接使用。
		input.TemplateVersion = templateEntry.Template.Version // 把模板真实版本回填到输入，避免上下文信息丢失。

		templateInput := TemplateWorkflowInput{
			Task:         input,                     // Task：原始任务输入本体。
			TemplateKey:  templateEntry.Key,         // TemplateKey：模板注册表中的唯一键。
			TemplateHash: templateEntry.ContentHash, // TemplateHash：模板内容哈希，便于追踪模板版本。
		}

		ometrics.WorkflowsStarted.WithLabelValues("TemplateWorkflow", "template").Inc() // 记录“模板工作流启动”指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,                 // WorkflowID：当前父工作流 ID。
			EventType:  activities.StreamEventDelegation,                           // EventType：告诉前端，任务即将委派给子工作流。
			AgentID:    "orchestrator",                                             // AgentID：发消息的仍然是 orchestrator。
			Message:    activities.MsgHandoffTemplate(templateEntry.Template.Name), // Message：说明具体交给哪个模板执行。
			Timestamp:  workflow.Now(ctx),                                          // Timestamp：记录委派发生时间。
		}).Get(ctx, nil)

		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父工作流关闭时，请求取消模板子工作流，避免孤儿任务。
		})
		var result TaskResult                                                                      // result：接收模板子工作流的业务结果。
		templateFuture := workflow.ExecuteChildWorkflow(childCtx, TemplateWorkflow, templateInput) // 启动模板子工作流。
		var templateExec workflow.Execution                                                        // templateExec：保存子工作流执行信息，便于登记控制。
		if err := templateFuture.GetChildWorkflowExecution().Get(childCtx, &templateExec); err != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, err // 子工作流都没成功启动，直接返回失败。
		}
		controlHandler.RegisterChildWorkflow(templateExec.ID) // 把模板子工作流注册到控制处理器，后续可被 pause/cancel 管控。
		if err := templateFuture.Get(childCtx, &result); err != nil {
			controlHandler.UnregisterChildWorkflow(templateExec.ID) // 子工作流结束或失败后立即反注册，避免状态残留。
			if cfg.TemplateFallbackEnabled {
				logger.Warn("Template workflow failed; falling back to AI decomposition", "error", err)
				ometrics.TemplateFallbackTriggered.WithLabelValues("error").Inc() // 记录“模板失败触发回退”的次数。
				ometrics.TemplateFallbackSuccess.WithLabelValues("error").Inc()   // 这里表示“成功切回 AI 路由分支”。
				templateFound = false                                             // 把 templateFound 改回 false，允许后续继续进入普通分解路径。
			} else {
				result = AddTaskContextToMetadata(result, input.Context) // 把输入上下文补到 metadata，方便 API 暴露更多调试信息。
				return result, err                                       // 不允许回退时，直接把模板失败结果抛给上层。
			}
		} else if !result.Success {
			controlHandler.UnregisterChildWorkflow(templateExec.ID) // 模板返回了结果，但业务上标记为失败，同样要反注册。
			if cfg.TemplateFallbackEnabled {
				logger.Warn("Template workflow returned unsuccessful result; falling back to AI decomposition")
				ometrics.TemplateFallbackTriggered.WithLabelValues("unsuccessful").Inc() // 记录“模板结果不成功”导致的回退。
				ometrics.TemplateFallbackSuccess.WithLabelValues("unsuccessful").Inc()   // 记录回退分支被允许继续执行。
				templateFound = false                                                    // 继续进入后面的 AI 分解流程。
			} else {
				scheduleStreamEnd(ctx)                                   // 结束 SSE 事件流，通知前端这个路径已结束。
				result = AddTaskContextToMetadata(result, input.Context) // 补齐 metadata 后返回业务失败结果。
				return result, nil
			}
		} else {
			controlHandler.UnregisterChildWorkflow(templateExec.ID)  // 模板成功执行完成，清理子工作流注册信息。
			scheduleStreamEnd(ctx)                                   // 正常结束事件流。
			result = AddTaskContextToMetadata(result, input.Context) // 把输入上下文带回结果里，方便调用方查看。
			return result, nil                                       // 模板路径已经产出最终结果，主编排器到此结束。
		}
	}

	// skip_synthesis 是一个“早路由”开关：
	// 某些上游流程已经生成了结构化 JSON 或者明确知道要走简单执行，
	// 如果再进入编排器分解或总结阶段，反而会被 LLM 重写，破坏原始结构。
	skipSynthesisVersion := workflow.GetVersion(ctx, "skip_synthesis_early_route_v1", workflow.DefaultVersion, 1)
	if skipSynthesisVersion >= 1 && GetContextBool(input.Context, "skip_synthesis") {
		logger.Info("Early route: skip_synthesis forces SimpleTaskWorkflow") // 记录这次是被 skip_synthesis 强制导向简单工作流。

		wfID := workflow.GetInfo(ctx).WorkflowExecution.ID // 再次读取父工作流 ID，作为统一事件流归属。
		input.ParentWorkflowID = wfID                      // 把父工作流 ID 写入输入，子工作流发事件时可回到同一条会话流。

		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: wfID,                             // WorkflowID：事件归属的父工作流。
			EventType:  activities.StreamEventDelegation, // EventType：表示开始委派。
			AgentID:    "orchestrator",                   // AgentID：委派动作由 orchestrator 发出。
			Message:    activities.MsgHandoffSimple(),    // Message：告诉前端接下来进入 SimpleTaskWorkflow。
			Timestamp:  workflow.Now(ctx),                // Timestamp：记录委派时间。
		}).Get(ctx, nil)

		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时同步取消简单子工作流。
		})
		var result TaskResult                                                             // result：保存 SimpleTaskWorkflow 的返回结果。
		childFuture := workflow.ExecuteChildWorkflow(childCtx, SimpleTaskWorkflow, input) // 启动简单子工作流。
		if err := childFuture.Get(ctx, &result); err != nil {
			return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 子工作流失败时同时返回业务失败信息和底层 error。
		}
		scheduleStreamEnd(ctx)                                   // 子工作流结束后补发 STREAM_END，通知前端输出流收尾。
		result = AddTaskContextToMetadata(result, input.Context) // 把输入上下文写进 metadata，便于上层查看完整任务上下文。
		return result, nil                                       // skip_synthesis 路径处理完成，直接返回。
	}

	// force_swarm 同样属于早路由开关。
	// SwarmWorkflow 内部已经有 Lead Agent 负责初始规划，因此再在 orchestrator 层分解只会重复消耗 token。
	forceSwarmVersion := workflow.GetVersion(ctx, "force_swarm_early_route_v1", workflow.DefaultVersion, 1)
	if forceSwarmVersion >= 1 && GetContextBool(input.Context, "force_swarm") {
		logger.Info("Force swarm detected - bypassing orchestrator decomposition, Lead Agent will plan")
		swarmWfID := workflow.GetInfo(ctx).WorkflowExecution.ID // 读取父工作流 ID，用于统一事件流和子工作流挂靠。
		input.ParentWorkflowID = swarmWfID                      // 把父工作流 ID 注入输入，让 swarm 子工作流继续沿用这条事件流。

		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父流程关闭时请求取消 SwarmWorkflow。
		})
		var result TaskResult                                                     // result：接收 SwarmWorkflow 的执行结果。
		ometrics.WorkflowsStarted.WithLabelValues("SwarmWorkflow", "swarm").Inc() // 上报 swarm 路由启动指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: swarmWfID,                        // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation, // EventType：委派事件。
			AgentID:    "orchestrator",                   // AgentID：委派来源。
			Message:    activities.MsgSwarmStarted(),     // Message：提示前端当前已经切换到 swarm 执行模式。
			Timestamp:  workflow.Now(ctx),                // Timestamp：记录切换时间。
		}).Get(ctx, nil)
		childFuture := workflow.ExecuteChildWorkflow(childCtx, SwarmWorkflow, input) // 真正启动 SwarmWorkflow 子工作流。
		var childExec workflow.Execution                                             // childExec：保存子工作流执行 ID，供控制信号转发使用。
		if err := childFuture.GetChildWorkflowExecution().Get(childCtx, &childExec); err != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to start SwarmWorkflow: %v", err)}, err // 子工作流启动失败直接返回。
		}
		controlHandler.RegisterChildWorkflow(childExec.ID) // 登记子工作流，后面 pause/cancel 时才能找到它。

		// human-input 是一个人工介入信号通道。
		// 这里把父工作流收到的人类输入信号原样转发给 SwarmWorkflow 子工作流，
		// 从而实现父子工作流之间的人机协作链路打通。
		humanInputCh := workflow.GetSignalChannel(ctx, "human-input") // 监听名为 human-input 的 Signal Channel。
		workflow.Go(ctx, func(gCtx workflow.Context) {
			for {
				var humanMsg map[string]string                                                                      // humanMsg：暂存收到的人类输入内容。
				humanInputCh.Receive(gCtx, &humanMsg)                                                               // 阻塞等待父工作流收到新的人工输入信号。
				_ = workflow.SignalExternalWorkflow(gCtx, childExec.ID, "", "human-input", humanMsg).Get(gCtx, nil) // 把同名信号转发给 swarm 子工作流。
			}
		})

		execErr := childFuture.Get(childCtx, &result)        // 等待 SwarmWorkflow 执行结束。
		controlHandler.UnregisterChildWorkflow(childExec.ID) // 一旦结束，立刻把子工作流从控制器里移除。
		if execErr != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("SwarmWorkflow failed: %v", execErr)}, execErr // swarm 路径失败时返回错误。
		}
		scheduleStreamEnd(ctx)                                   // 发出流结束事件。
		result = AddTaskContextToMetadata(result, input.Context) // 补齐 metadata。
		return result, nil                                       // swarm 路径成功结束。
	}

	// force_research 也是早路由开关。
	// ResearchWorkflow 内部本来就有“查询改写 + 分解 + 多步研究”的专门管线，
	// 所以 orchestrator 不再重复做一遍任务分解。
	if GetContextBool(input.Context, "force_research") {
		logger.Info("Force research detected - bypassing orchestrator decomposition")

		// 给强制 research 路径补充当前日期，让下游研究类 Prompt 能具备时间感知能力。
		// 这里必须用 workflow.Now，而不能用 time.Now，否则会破坏 Temporal 的可重放性。
		// 同时使用版本门保护，避免老工作流回放时突然多出这一步。
		forceResearchDateVersion := workflow.GetVersion(ctx, "force_research_current_date_v1", workflow.DefaultVersion, 1)
		if forceResearchDateVersion >= 1 {
			if _, hasDate := input.Context["current_date"]; !hasDate {
				workflowTime := workflow.Now(ctx)                                                  // workflowTime：取当前工作流时间，后续同时生成机器可读和人类可读日期。
				input.Context["current_date"] = workflowTime.UTC().Format("2006-01-02")            // current_date：标准 YYYY-MM-DD 格式。
				input.Context["current_date_human"] = workflowTime.UTC().Format("January 2, 2006") // current_date_human：面向自然语言 prompt 的英文日期格式。
			}
		}

		// 研究类任务支持 HITL（Human In The Loop）人工审阅。
		// 这里同时兼容旧字段 require_review=true 和前端新字段 review_plan=manual。
		requireReview := GetContextBool(input.Context, "require_review") ||
			GetContextString(input.Context, "review_plan") == "manual"
		if requireReview {
			logger.Info("HITL review enabled - generating research plan")

			reviewTimeout := DefaultReviewTimeout // 先使用默认人工审核超时值。
			if t, ok := input.Context["review_timeout"]; ok {
				var seconds int // seconds：把不同类型的 review_timeout 最终统一转换成秒数。
				switch v := t.(type) {
				case float64:
					seconds = int(v) // 兼容 JSON 数字被解成 float64 的情况。
				case int:
					seconds = v // 已经是 int，直接使用。
				case int32:
					seconds = int(v) // int32 转成 int。
				case int64:
					seconds = int(v) // int64 转成 int。
				case string:
					if parsed, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
						seconds = parsed // 字符串数字去空白后解析成秒数。
					}
				}
				if seconds > 0 {
					reviewTimeout = time.Duration(seconds) * time.Second // 只接受正数超时，转成 Duration。
				}
			}
			// 再做一次安全钳制，避免外部传入超长等待时间，导致工作流长期挂起。
			if reviewTimeout > DefaultReviewTimeout {
				reviewTimeout = DefaultReviewTimeout // 超过上限时强制回落到 15 分钟。
			}

			// 第 1 步：先调用研究计划生成 Activity，让 LLM 产出初始计划，并初始化 Redis 中的评审状态。
			var plan activities.ResearchPlanResult // plan：保存初始研究计划生成结果。
			planInput := activities.ResearchPlanInput{
				Query:      input.Query,                                // Query：当前研究任务的问题描述。
				Context:    input.Context,                              // Context：补充 Prompt 和执行约束的上下文字段。
				WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, // WorkflowID：当前工作流实例 ID。
				SessionID:  input.SessionID,                            // SessionID：前端会话 ID。
				UserID:     input.UserID,                               // UserID：当前用户 ID。
				TenantID:   input.TenantID,                             // TenantID：租户 ID。
				TTL:        reviewTimeout,                              // TTL：研究计划在审核阶段可保留的有效期。
			}
			planCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
				StartToCloseTimeout: 60 * time.Second,                          // 计划生成可能涉及 LLM 调用，因此给到 60 秒。
				RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3}, // 短暂失败时允许有限重试。
			})
			if err := workflow.ExecuteActivity(planCtx, constants.GenerateResearchPlanActivity, planInput).Get(ctx, &plan); err != nil {
				return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to generate research plan: %v", err)}, err // 计划都生成不出来时，研究路径直接失败。
			}

			// 第 2 步：把“研究计划已准备好”的事件发给前端。
			// plan.Message 在 Activity 内已经去掉了内部标签，前端可以直接展示。
			_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
				WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,                         // WorkflowID：父工作流 ID。
				EventType:  activities.StreamEventResearchPlanReady,                            // EventType：研究计划已就绪。
				AgentID:    "planner",                                                          // AgentID：表示该消息来自 planner 角色。
				Message:    plan.Message,                                                       // Message：给用户展示的研究计划摘要。
				Timestamp:  workflow.Now(ctx),                                                  // Timestamp：计划就绪时间。
				Payload:    map[string]interface{}{"round": plan.Round, "intent": plan.Intent}, // Payload：额外附带当前轮次和意图标签。
			}).Get(ctx, nil)

			// 第 3 步：等待用户通过 Signal 确认计划，或者等待超时。
			sigName := "research-plan-approved-" + workflow.GetInfo(ctx).WorkflowExecution.ID // sigName：按工作流 ID 拼出唯一信号名。
			ch := workflow.GetSignalChannel(ctx, sigName)                                     // ch：监听这次研究计划审核的专用信号通道。
			timerCtx, cancelTimer := workflow.WithCancel(ctx)                                 // timerCtx：为定时器单独创建可取消上下文。
			timer := workflow.NewTimer(timerCtx, reviewTimeout)                               // timer：审核窗口倒计时。

			var reviewResult activities.ResearchReviewResult // reviewResult：保存用户最终确认后的计划和对话。
			var timedOut bool                                // timedOut：标记本次等待是否走到了超时分支。

			sel := workflow.NewSelector(ctx) // selector：同时监听“收到信号”和“定时器到期”两个事件源。
			sel.AddReceive(ch, func(c workflow.ReceiveChannel, more bool) {
				c.Receive(ctx, &reviewResult) // 收到用户确认信号后，把内容读取到 reviewResult。
				cancelTimer()                 // 收到结果后主动取消定时器，避免 Temporal UI 里残留一个还在等待的 timer。
			})
			sel.AddFuture(timer, func(f workflow.Future) {
				timedOut = true // 定时器先结束时，说明人工审核超时。
			})
			sel.Select(ctx) // 阻塞等待两条分支中先发生的一条。

			if timedOut {
				wfID := workflow.GetInfo(ctx).WorkflowExecution.ID // wfID：当前工作流 ID，用于发超时事件。
				logger.Warn("HITL review timed out", "workflow_id", wfID)
				_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
					WorkflowID: wfID,                                // WorkflowID：当前工作流。
					EventType:  activities.StreamEventErrorOccurred, // EventType：错误事件。
					AgentID:    "planner",                           // AgentID：由 planner 发出审核超时消息。
					Message:    activities.MsgResearchTimedOut(),    // Message：提示用户研究计划审核超时。
					Timestamp:  workflow.Now(ctx),                   // Timestamp：超时时间。
				}).Get(ctx, nil)
				// 即使是超时失败，也补发 WORKFLOW_COMPLETED，避免前端一直停留在 running 状态。
				_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
					WorkflowID: wfID,                                    // WorkflowID：当前工作流。
					EventType:  activities.StreamEventWorkflowCompleted, // EventType：工作流结束。
					AgentID:    "orchestrator",                          // AgentID：由 orchestrator 发结束事件。
					Message:    activities.MsgWorkflowCompleted(),       // Message：结束提示。
					Timestamp:  workflow.Now(ctx),                       // Timestamp：结束时间。
				}).Get(ctx, nil)
				return TaskResult{Success: false, ErrorMessage: "research plan review timed out"}, nil // 业务上失败，但不是系统错误。
			}

			// 第 4 步：把用户确认后的最终计划及对话记录写回 context，供 ResearchWorkflow 继续使用。
			input.Context["confirmed_plan"] = reviewResult.FinalPlan         // confirmed_plan：审核通过后的最终计划。
			input.Context["review_conversation"] = reviewResult.Conversation // review_conversation：审核轮次中的对话历史。
			if reviewResult.ResearchBrief != "" {
				input.Context["research_brief"] = reviewResult.ResearchBrief // research_brief：额外的研究摘要，供后续 prompt 使用。
			}

			// 第 5 步：通知前端“研究计划已确认，可以继续执行了”。
			_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
				WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, // WorkflowID：当前工作流 ID。
				EventType:  activities.StreamEventResearchPlanApproved, // EventType：研究计划已批准。
				AgentID:    "planner",                                  // AgentID：planner 发出批准消息。
				Message:    activities.MsgResearchConfirmed(),          // Message：面向前端的确认文案。
				Timestamp:  workflow.Now(ctx),                          // Timestamp：批准时间。
			}).Get(ctx, nil)

			logger.Info("HITL review approved, continuing with research",
				"conversation_rounds", len(reviewResult.Conversation), // 记录一共进行了多少轮人工审阅对话。
			)
		}

		// 即使是强制 research，也仍然需要先做预算预检。
		// 这样下游模式工作流在执行工具调用时，才能拿到预算信息并正确记录成本。
		// 这里同样用版本门保护，避免历史工作流回放时突然多执行一个新 Activity。
		forceResearchBudgetVersion := workflow.GetVersion(ctx, "force_research_budget_v1", workflow.DefaultVersion, 1)
		if forceResearchBudgetVersion >= 1 && input.UserID != "" {
			est := EstimateTokensWithConfig(activities.DecompositionResult{
				ComplexityScore: 0.5,                                            // 这里构造一个近似复杂度，用于估算预算。
				Subtasks:        []activities.Subtask{{ID: "force_research-1"}}, // 强制 research 至少视作一个子任务。
			}, &cfg)
			if res, err := BudgetPreflight(ctx, input, est); err == nil && res != nil {
				if !res.CanProceed {
					scheduleStreamEnd(ctx)                                                                                                // 预算不足时也要补发流结束事件。
					out := TaskResult{Success: false, ErrorMessage: res.Reason, Metadata: map[string]interface{}{"budget_blocked": true}} // out：预算阻断时返回的失败结果。
					out = AddTaskContextToMetadata(out, input.Context)                                                                    // 补齐上下文元数据。
					return out, nil                                                                                                       // 预算不足属于业务失败，不额外返回系统 error。
				}
				if input.Context == nil {
					input.Context = map[string]interface{}{} // 确保 context 可写，后面写入预算信息。
				}
				input.Context["budget_remaining"] = res.RemainingTaskBudget // budget_remaining：当前任务剩余总预算。
				agentMax := res.RemainingTaskBudget                         // ResearchWorkflow 自己再拆预算，所以先把全部剩余预算交给它。
				if v := os.Getenv("TOKEN_BUDGET_PER_AGENT"); v != "" {
					if n, err := strconv.Atoi(v); err == nil && n > 0 && n < agentMax {
						agentMax = n // 环境变量可进一步限制单 agent 预算上限。
					}
				}
				if capv, ok := input.Context["token_budget_per_agent"].(int); ok && capv > 0 && capv < agentMax {
					agentMax = capv // 请求上下文里的 int 上限优先级更高，可继续收紧预算。
				}
				if capv, ok := input.Context["token_budget_per_agent"].(float64); ok && capv > 0 && int(capv) < agentMax {
					agentMax = int(capv) // 兼容 JSON 数字被解成 float64 的情况。
				}
				input.Context["budget_agent_max"] = agentMax // budget_agent_max：下游每个 agent 可使用的最大预算。
			}
		}

		// 在真正委派给 ResearchWorkflow 之前，先设置 ParentWorkflowID，
		// 这样所有子工作流发出的事件都能归并到同一条父级事件流。
		parentWorkflowID := workflow.GetInfo(ctx).WorkflowExecution.ID // parentWorkflowID：当前父工作流 ID。
		input.ParentWorkflowID = parentWorkflowID                      // 写回输入，供子工作流继续透传。

		// 给前端发出一次显式的“切换到 ResearchWorkflow”事件。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: parentWorkflowID,                                                    // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation,                                    // EventType：委派事件。
			AgentID:    "orchestrator",                                                      // AgentID：委派发起者。
			Message:    activities.MsgWorkflowRouting("ResearchWorkflow", "force_research"), // Message：说明路由目标和原因。
			Timestamp:  workflow.Now(ctx),                                                   // Timestamp：委派时间。
		}).Get(ctx, nil)

		ometrics.WorkflowsStarted.WithLabelValues("ResearchWorkflow", "force_research").Inc() // 记录强制 research 路由启动指标。

		strategiesInput := convertToStrategiesInput(input) // 把 workflows 层的 TaskInput 转成 strategies 包使用的输入结构。
		var strategiesResult strategies.TaskResult         // strategiesResult：接收 strategies 包返回的结果。
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时同步取消 research 子工作流。
		})
		researchFuture := workflow.ExecuteChildWorkflow(childCtx, strategies.ResearchWorkflow, strategiesInput) // 启动 strategies.ResearchWorkflow。
		var researchExec workflow.Execution                                                                     // researchExec：保存子工作流执行 ID。
		if err := researchFuture.GetChildWorkflowExecution().Get(childCtx, &researchExec); err != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, err // 子工作流没成功启动时直接失败。
		}
		controlHandler.RegisterChildWorkflow(researchExec.ID)      // 注册 research 子工作流，纳入控制信号管理。
		execErr := researchFuture.Get(childCtx, &strategiesResult) // 等待 research 子工作流执行结束。
		controlHandler.UnregisterChildWorkflow(researchExec.ID)    // 结束后反注册。

		scheduleStreamEnd(ctx) // 无论成功失败，这条强制 research 路径都到这里结束事件流。

		if execErr != nil {
			controlHandler.EmitCancelledIfNeeded(ctx, execErr.Error())            // 如果底层错误本质上是取消，则补发取消事件。
			return AddTaskContextToMetadata(TaskResult{}, input.Context), execErr // 返回一个带上下文的空结果和底层 error。
		}
		result := convertFromStrategiesResult(strategiesResult)  // 把 strategies 层结果转回 workflows 层结果结构。
		result = AddTaskContextToMetadata(result, input.Context) // 补齐输入上下文。
		return result, nil                                       // 强制 research 路径到此结束。
	}

	// 从这里开始进入“通用任务分解 + 通用路由”主路径。
	// 前面那些 force_* 或模板逻辑都属于捷径；如果都没命中，就走这套标准编排流程。
	//
	// decompContext 是传给分解 Activity 的上下文副本。
	// 这里不直接把 input.Context 原样复用，而是复制一份，避免后续为分解阶段注入的辅助字段污染原始输入。
	decompContext := make(map[string]interface{})
	if input.Context != nil {
		for k, v := range input.Context {
			decompContext[k] = v // 把原始 context 的每个键值复制到分解专用 context 中。
		}
	}
	// 给分解阶段补充当前日期，让分解 Prompt 在处理“今天/最近/每周”等时间词时更准确。
	// 仅在上游没有显式传入日期时才注入，给调用方保留覆盖权。
	if _, hasDate := decompContext["current_date"]; !hasDate {
		workflowTime := workflow.Now(ctx)                                                  // workflowTime：取当前工作流时间，用它生成两种格式的日期。
		decompContext["current_date"] = workflowTime.UTC().Format("2006-01-02")            // current_date：机器友好的标准日期。
		decompContext["current_date_human"] = workflowTime.UTC().Format("January 2, 2006") // current_date_human：自然语言更容易理解的日期格式。
	}
	// 如果系统启用了 P2P 协同，就告诉分解 Prompt 需要考虑 subtasks 之间的 produces/consumes 关系。
	if cfg.P2PCoordinationEnabled {
		decompContext["p2p_enabled"] = true // p2p_enabled：提示下游分解器输出任务间数据依赖。
	}
	// 如果当前任务带有历史消息，就把历史压平为字符串放进分解上下文，
	// 让分解器知道本轮问题是在什么对话背景下提出的。
	if len(input.History) > 0 {
		historyLines := convertHistoryForAgent(input.History)       // 把结构化历史转换成适合 prompt 使用的文本行。
		decompContext["history"] = strings.Join(historyLines, "\n") // history：多轮对话拼成单段字符串，供分解 Activity 使用。
	}

	var decomp activities.DecompositionResult // decomp：保存任务分解结果，包括复杂度、子任务、策略、token 统计等。

	// agentPresent 表示调用方已经明确指定了单一 agent。
	// 这时不再让 LLM 做通用分解，因为单 agent 模式本身就是一个更强约束的执行入口。
	agentPresent := false
	if input.Context != nil {
		if agentID, ok := input.Context["agent"].(string); ok && agentID != "" {
			agentPresent = true // 标记已经显式指定 agent，后面跳过普通分解。
			logger.Info("Agent specified - bypassing LLM decomposition", "agent_id", agentID)

			// suggested_tools 用于把 API 上游建议的工具列表透传给单 agent。
			// 这样像 Sagasu 这类场景就能在 setup 阶段提示 agent 优先用 web_search 等工具。
			var agentTools []string // agentTools：收集解析后的建议工具名列表。
			if toolsVal, ok := input.Context["suggested_tools"]; ok {
				if arr, ok := toolsVal.([]interface{}); ok {
					for _, v := range arr {
						if s, ok := v.(string); ok {
							agentTools = append(agentTools, s) // 只保留字符串类型的工具名，忽略异常值。
						}
					}
				}
			}

			// 人工构造一个“单子任务”的分解结果，供后续路由流程复用，
			// 这样虽然跳过了 LLM 分解，但下游代码仍能按统一结构处理。
			decomp = activities.DecompositionResult{
				Mode:              "simple",     // Mode：把这次任务标记成简单模式。
				ComplexityScore:   0.0,          // ComplexityScore：显式指定 agent 时认为复杂度最低。
				ExecutionStrategy: "sequential", // ExecutionStrategy：单 agent 按串行执行。
				ConcurrencyLimit:  1,            // ConcurrencyLimit：并发上限就是 1。
				Subtasks: []activities.Subtask{
					{
						ID:              "task-1",                                 // ID：唯一子任务编号。
						Description:     fmt.Sprintf("Execute agent %s", agentID), // Description：说明这个子任务就是执行指定 agent。
						Dependencies:    []string{},                               // Dependencies：单任务没有依赖。
						EstimatedTokens: 0,                                        // EstimatedTokens：这里不估算分解开销。
						SuggestedTools:  agentTools,                               // SuggestedTools：把上游建议工具透传给该 agent。
						ToolParameters:  map[string]interface{}{},                 // ToolParameters：单 agent 情况下暂时不给额外工具参数。
					},
				},
				TotalEstimatedTokens: 0, // TotalEstimatedTokens：不额外估算分解 token。
				TokensUsed:           0, // TokensUsed：因为没有调用 LLM 分解，所以为 0。
				InputTokens:          0, // InputTokens：分解阶段输入 token 为 0。
				OutputTokens:         0, // OutputTokens：分解阶段输出 token 为 0。
			}
		}
	}

	// rolePresent 表示调用方没有指定单 agent，但显式指定了 role。
	// 某些 role 内部自带多步逻辑，如果 orchestrator 再做一次分解，反而会和 role 内部约定冲突。
	rolePresent := false
	if !agentPresent && input.Context != nil {
		if role, ok := input.Context["role"].(string); ok && role != "" {
			rolePresent = true                    // 标记本次是角色驱动路径。
			roleTools := roles.AllowedTools(role) // roleTools：查出这个角色被允许使用的工具集合。
			logger.Info("Role specified - bypassing LLM decomposition", "role", role, "tool_count", len(roleTools))

			// 先给前端发出 ROLE_ASSIGNED 事件，告诉界面当前任务已经绑定到某个角色。
			_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
				WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,       // WorkflowID：当前工作流 ID。
				EventType:  activities.StreamEventRoleAssigned,               // EventType：角色已分配。
				AgentID:    role,                                             // AgentID：把角色名本身作为消息来源。
				Message:    activities.MsgRoleAssigned(role, len(roleTools)), // Message：说明角色名和工具数量。
				Timestamp:  workflow.Now(ctx),                                // Timestamp：角色分配时间。
				Payload: map[string]interface{}{
					"role":       role,           // role：当前分配的角色名。
					"tools":      roleTools,      // tools：这个角色允许使用的工具列表。
					"tool_count": len(roleTools), // tool_count：工具列表长度。
				},
			}).Get(ctx, nil)

			// 同样手工构造一个单子任务的分解结果，让后续路由逻辑保持统一。
			decomp = activities.DecompositionResult{
				Mode:              "simple",     // Mode：角色指定路径仍然按简单模式交给下游。
				ComplexityScore:   0.5,          // ComplexityScore：角色任务不一定最简单，因此给中等偏低复杂度。
				ExecutionStrategy: "sequential", // ExecutionStrategy：由 role agent 自己串行处理。
				ConcurrencyLimit:  1,            // ConcurrencyLimit：角色任务不在 orchestrator 层并发拆分。
				Subtasks: []activities.Subtask{
					{
						ID:              "task-1",                            // ID：唯一子任务编号。
						Description:     input.Query,                         // Description：子任务内容直接复用用户原始 Query。
						Dependencies:    []string{},                          // Dependencies：单子任务无依赖。
						EstimatedTokens: 5000,                                // EstimatedTokens：给一个经验估值，用于预算预检。
						SuggestedTools:  append([]string(nil), roleTools...), // SuggestedTools：把该角色允许的工具复制进去。
						ToolParameters:  map[string]interface{}{},            // ToolParameters：具体参数由 agent 再从 context 里构造。
					},
				},
				TotalEstimatedTokens: 5000, // TotalEstimatedTokens：整个任务估算 token。
				TokensUsed:           0,    // TokensUsed：这里没有调用 LLM 分解，因此为 0。
				InputTokens:          0,    // InputTokens：分解阶段输入 token 为 0。
				OutputTokens:         0,    // OutputTokens：分解阶段输出 token 为 0。
			}

			// 角色分配和事件发送过程中也可能收到 pause/cancel，因此这里补做一次暂停点检查。
			if err := controlHandler.CheckPausePoint(ctx, "post_role_assignment"); err != nil {
				return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 如果用户在这时取消，立刻终止后续路由。
			}
		}
	}

	// 如果既没有显式 agent，也没有显式 role，就走标准的 LLM 分解流程。
	if !rolePresent && !agentPresent {
		// 在真正分解前先给前端发一个“正在理解你的请求”的进度消息，提升交互反馈。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, // WorkflowID：当前工作流。
			EventType:  activities.StreamEventProgress,             // EventType：进度更新。
			AgentID:    "planner",                                  // AgentID：由 planner 发出。
			Message:    activities.MsgUnderstandingRequest(),       // Message：告诉用户系统正在理解需求。
			Timestamp:  workflow.Now(ctx),                          // Timestamp：消息发送时间。
		}).Get(ctx, nil)

		if err := workflow.ExecuteActivity(actx, constants.DecomposeTaskActivity, activities.DecompositionInput{
			Query:          input.Query,   // Query：当前用户问题。
			Context:        decompContext, // Context：为分解阶段准备的增强上下文。
			AvailableTools: nil,           // AvailableTools：留空，让 llm-service 根据工具注册表和 role 预设自行推导。
		}).Get(ctx, &decomp); err != nil {
			logger.Warn("Task decomposition failed, falling back to SimpleTaskWorkflow", "error", err)
			// 分解失败时，先通知前端这是一次降级，而不是无声失败。
			_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
				WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, // WorkflowID：当前工作流。
				EventType:  activities.StreamEventProgress,             // EventType：仍然作为进度更新发出。
				AgentID:    "planner",                                  // AgentID：planner。
				Message:    activities.MsgDecompositionFailed(),        // Message：告诉前端分解失败，系统将采用降级策略。
				Timestamp:  workflow.Now(ctx),                          // Timestamp：消息时间。
			}).Get(ctx, nil)

			// 人工构造一个降级版分解结果，强制后面走 SimpleTaskWorkflow。
			decomp = activities.DecompositionResult{
				Mode:              "simple",     // Mode：降级到简单模式。
				ComplexityScore:   0.1,          // ComplexityScore：设得足够低，确保命中简单路由。
				ExecutionStrategy: "sequential", // ExecutionStrategy：按单步串行执行。
				CognitiveStrategy: "",           // CognitiveStrategy：不再指定认知策略。
				Subtasks: []activities.Subtask{
					{
						ID:           "1",         // ID：唯一子任务 ID。
						Description:  input.Query, // Description：直接把用户原始问题当作任务描述。
						TaskType:     "generic",   // TaskType：通用任务类型。
						Dependencies: []string{},  // Dependencies：没有依赖。
					},
				},
				TotalEstimatedTokens: 5000, // TotalEstimatedTokens：使用一个保守估值。
				TokensUsed:           0,    // TokensUsed：降级分解没有 LLM 消耗。
				InputTokens:          0,    // InputTokens：为 0。
				OutputTokens:         0,    // OutputTokens：为 0。
			}
			logger.Info("Created fallback decomposition for simple execution", "query", input.Query)
		}

		// LLM 分解 Activity 可能比较慢，因此结束后再检查一次 pause/cancel。
		if err := controlHandler.CheckPausePoint(ctx, "post_decomposition"); err != nil {
			return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 如有暂停或取消，立刻中断后续流程。
		}
	}

	// 如果分解阶段返回了 token 统计信息，就把这次分解调用记到账务/观测系统里。
	if decomp.TokensUsed > 0 || decomp.InputTokens > 0 || decomp.OutputTokens > 0 {
		inTok := decomp.InputTokens   // inTok：分解阶段输入 token 数。
		outTok := decomp.OutputTokens // outTok：分解阶段输出 token 数。
		if inTok == 0 && outTok == 0 && decomp.TokensUsed > 0 {
			inTok = int(float64(decomp.TokensUsed) * 0.6) // 如果只返回总 token，则按 6:4 粗略拆分输入输出。
			outTok = decomp.TokensUsed - inTok            // 用总量减去输入，得到输出 token。
		}
		wid := workflow.GetInfo(ctx).WorkflowExecution.ID // wid：任务 ID，这里直接复用工作流 ID。
		recCtx := opts.WithTokenRecordOptions(ctx)        // recCtx：套用记录 token 的专用 Activity 选项。
		_ = workflow.ExecuteActivity(recCtx, constants.RecordTokenUsageActivity, activities.TokenUsageInput{
			UserID:       input.UserID,                                 // UserID：把成本归属到当前用户。
			SessionID:    input.SessionID,                              // SessionID：归属到当前会话。
			TaskID:       wid,                                          // TaskID：归属到当前任务。
			AgentID:      "decompose",                                  // AgentID：这里把分解器视作一个逻辑 agent。
			Model:        decomp.ModelUsed,                             // Model：本次分解用到的模型名。
			Provider:     decomp.Provider,                              // Provider：模型提供方。
			InputTokens:  inTok,                                        // InputTokens：输入 token 数。
			OutputTokens: outTok,                                       // OutputTokens：输出 token 数。
			Metadata:     map[string]interface{}{"phase": "decompose"}, // Metadata：标记成本发生在 decompose 阶段。
		}).Get(ctx, nil)
	}

	// 把最终分解结果写到日志里，便于后续回查“为什么这次路由到了某个工作流”。
	logger.Info("Routing decision",
		"complexity", decomp.ComplexityScore, // complexity：复杂度分数。
		"mode", decomp.Mode, // mode：简单 / 复杂等模式标签。
		"num_subtasks", len(decomp.Subtasks), // num_subtasks：分解后的子任务数量。
		"cognitive_strategy", decomp.CognitiveStrategy, // cognitive_strategy：是否命中了特定认知策略。
	)

	// 把分解结果转换成更适合前端展示的“计划摘要”，包括步骤列表和依赖边。
	{
		steps := make([]map[string]interface{}, 0, len(decomp.Subtasks)) // steps：前端展示用的步骤列表。
		deps := make([]map[string]string, 0, 4)                          // deps：前端展示用的依赖关系边列表。
		for _, st := range decomp.Subtasks {
			steps = append(steps, map[string]interface{}{
				"id":   st.ID,          // id：步骤唯一标识。
				"name": st.Description, // name：步骤的人类可读描述。
				"type": st.TaskType,    // type：步骤类型。
			})
			for _, d := range st.Dependencies {
				deps = append(deps, map[string]string{"from": d, "to": st.ID}) // 用 from -> to 记录任务依赖方向。
			}
		}
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,          // WorkflowID：当前工作流 ID。
			EventType:  activities.StreamEventProgress,                      // EventType：把“计划已创建”作为进度消息发出。
			AgentID:    "planner",                                           // AgentID：由 planner 发出。
			Message:    activities.MsgPlanCreated(len(steps)),               // Message：告诉前端共生成了多少个步骤。
			Timestamp:  workflow.Now(ctx),                                   // Timestamp：计划创建时间。
			Payload:    map[string]interface{}{"plan": steps, "deps": deps}, // Payload：把详细步骤和依赖直接带给前端。
		}).Get(ctx, nil)
	}

	// 把已经生成好的分解结果挂到 input 上，
	// 后面的子工作流就可以直接复用这份计划，避免再次调用分解器。
	input.PreplannedDecomposition = &decomp

	// 预算预检只在能识别到用户时才有意义，因为预算通常是按用户或租户维度结算和限流的。
	if input.UserID != "" {
		est := EstimateTokensWithConfig(decomp, &cfg) // est：根据分解结果和配置估算本次任务大概会消耗多少 token。
		if res, err := BudgetPreflight(ctx, input, est); err == nil && res != nil {
			if !res.CanProceed {
				scheduleStreamEnd(ctx)                                                                                                // 预算预检未通过时，仍要显式结束事件流。
				out := TaskResult{Success: false, ErrorMessage: res.Reason, Metadata: map[string]interface{}{"budget_blocked": true}} // out：预算不足时的业务失败结果。
				out = AddTaskContextToMetadata(out, input.Context)                                                                    // 把上下文一并带回去，方便前端展示更多信息。
				return out, nil                                                                                                       // 预算不足属于业务阻断，不额外返回系统 error。
			}
			// 预算预检通过后，把预算信息写回 context，供后续子工作流和 agent 使用。
			if input.Context == nil {
				input.Context = map[string]interface{}{} // 确保 context 可写。
			}
			// 顺手把 current_date 也写回原始 input.Context，确保后续所有子工作流都能拿到统一日期。
			if _, hasDate := input.Context["current_date"]; !hasDate {
				workflowTime := workflow.Now(ctx)                                                  // workflowTime：当前工作流时间。
				input.Context["current_date"] = workflowTime.UTC().Format("2006-01-02")            // current_date：标准日期。
				input.Context["current_date_human"] = workflowTime.UTC().Format("January 2, 2006") // current_date_human：适合 prompt 的自然语言日期。
			}
			input.Context["budget_remaining"] = res.RemainingTaskBudget // budget_remaining：当前任务剩余总预算。
			n := len(decomp.Subtasks)                                   // n：子任务数量，用来粗分单 agent 预算。
			if n == 0 {
				n = 1 // 防止除零；没有子任务时也至少按 1 份预算计算。
			}
			agentMax := res.RemainingTaskBudget / n // agentMax：平均分摊后的单 agent 初始预算。
			if v := os.Getenv("TOKEN_BUDGET_PER_AGENT"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n < agentMax {
					agentMax = n // 环境变量可进一步收紧单 agent 最大预算。
				}
			}
			if capv, ok := input.Context["token_budget_per_agent"].(int); ok && capv > 0 && capv < agentMax {
				agentMax = capv // context 中的 int 预算上限优先级更高。
			}
			if capv, ok := input.Context["token_budget_per_agent"].(float64); ok && capv > 0 && int(capv) < agentMax {
				agentMax = int(capv) // 兼容 JSON 数字转 float64 的场景。
			}
			input.Context["budget_agent_max"] = agentMax // budget_agent_max：后续每个 agent 最多可用的预算。
		}
	}

	// 下面进入审批闸门。
	// 它既可以由系统配置全局打开，也可以由本次请求显式要求。
	if cfg.ApprovalEnabled {
		// 这里暂时不做额外操作，只保留语义提示：
		// 当前审批策略的阈值主要通过下面构造的 pol 传入 CheckApprovalPolicyWith。
	}
	if cfg.ApprovalEnabled || input.RequireApproval {
		// pol：根据运行时配置组装出的审批策略。
		pol := activities.ApprovalPolicy{
			ComplexityThreshold: cfg.ApprovalComplexityThreshold, // ComplexityThreshold：超过该复杂度时触发审批。
			TokenBudgetExceeded: false,                           // TokenBudgetExceeded：这里先不在策略对象中硬编码预算超限。
			RequireForTools:     cfg.ApprovalDangerousTools,      // RequireForTools：命中危险工具时需要审批。
		}
		if need, reason := CheckApprovalPolicyWith(pol, input, decomp); need {
			if ar, err := RequestAndWaitApproval(ctx, input, reason); err != nil {
				scheduleStreamEnd(ctx)                                                                           // 审批流本身异常时，也补发流结束事件。
				out := TaskResult{Success: false, ErrorMessage: fmt.Sprintf("approval request failed: %v", err)} // out：审批请求流程失败时的结果。
				out = AddTaskContextToMetadata(out, input.Context)                                               // 补齐上下文元数据。
				return out, err                                                                                  // 审批流程异常属于系统级错误，需要返回 error。
			} else if ar == nil || !ar.Approved {
				msg := reason // 默认把策略判定原因作为拒绝文案。
				if ar != nil && ar.Feedback != "" {
					msg = ar.Feedback // 如果审批方给了更具体反馈，则优先使用审批反馈。
				}
				scheduleStreamEnd(ctx)                                                                   // 审批被拒绝也需要结束事件流。
				out := TaskResult{Success: false, ErrorMessage: fmt.Sprintf("approval denied: %s", msg)} // out：审批拒绝时的业务失败结果。
				out = AddTaskContextToMetadata(out, input.Context)                                       // 补齐上下文元数据。
				return out, nil                                                                          // 审批拒绝是业务决策，不额外返回系统 error。
			}
		}
	}

	// 到这里，分解、预算、审批都已准备好，开始做最终路由判断。
	// needsTools 用来判断这个计划是不是“真正的一次性任务”。
	// 只要子任务里出现工具、依赖、produces/consumes 或工具参数，就不再视作极简路径。
	needsTools := false
	for _, st := range decomp.Subtasks {
		if len(st.SuggestedTools) > 0 || len(st.Dependencies) > 0 || len(st.Consumes) > 0 || len(st.Produces) > 0 {
			needsTools = true // 命中任一复杂特征，就说明这不是最简单的一步到位任务。
			break             // 一旦确认需要工具或依赖，后面无需继续扫描。
		}
		if st.ToolParameters != nil && len(st.ToolParameters) > 0 {
			needsTools = true // 只要显式带了工具参数，也说明这条路径存在工具编排需求。
			break
		}
	}
	if rolePresent {
		needsTools = false // 显式 role 路径交给 role agent 自己处理，因此这里不再用 needsTools 阻止 simple 路由。
	}
	simpleByShape := len(decomp.Subtasks) == 0 || (len(decomp.Subtasks) == 1 && !needsTools) // simpleByShape：结构形态上是否足够简单。
	isSimple := decomp.ComplexityScore < simpleThreshold && simpleByShape                    // isSimple：复杂度和结构同时满足时，才真正走简单路由。

	// 统一设置 ParentWorkflowID，保证后续无论路由到哪个策略工作流，事件都回到同一个父工作流。
	parentWorkflowID := workflow.GetInfo(ctx).WorkflowExecution.ID // parentWorkflowID：当前父工作流 ID。
	input.ParentWorkflowID = parentWorkflowID                      // 写回输入，供所有后续子工作流透传使用。

	// browser_use 角色是一条单独的策略工作流。
	// 这里还要兼容旧版本：v2+ 走 BrowserUseWorkflow，v1 仍走 ReactWorkflow。
	browserUseVersion := workflow.GetVersion(ctx, "browser_use_routing_v1", workflow.DefaultVersion, 2)
	if browserUseVersion >= 2 && rolePresent && input.Context != nil {
		if role, ok := input.Context["role"].(string); ok && role == "browser_use" {
			logger.Info("Routing to BrowserUseWorkflow based on browser_use role")
			input.Context["force_tools"] = true // force_tools：强制模型真正调用浏览器工具，避免“空想已完成”。
			if result, handled, err := routeStrategyWorkflow(ctx, input, "browser_use", decomp.Mode, emitCtx, controlHandler); handled {
				return result, err // browser_use 路径被接管后，直接返回。
			}
		}
	} else if browserUseVersion == 1 && rolePresent && input.Context != nil {
		// 旧版本兼容：早期 browser_use 还是用 ReactWorkflow 承载。
		if role, ok := input.Context["role"].(string); ok && role == "browser_use" {
			logger.Info("Routing to ReactWorkflow based on browser_use role (legacy v1)")
			input.Context["force_tools"] = true // 同样强制使用工具，避免模型直接编造浏览结果。
			if result, handled, err := routeStrategyWorkflow(ctx, input, "react", decomp.Mode, emitCtx, controlHandler); handled {
				return result, err // 旧版 browser_use 路由一旦命中也直接结束当前函数。
			}
		}
	}

	// 如果调用方没有显式指定 role，就再做一次浏览器意图自动检测。
	// 当前 detectBrowserIntent 已收敛为“只对必须 JS 渲染的网站返回 true”，避免误判。
	autoDetectVersion := workflow.GetVersion(ctx, "browser_auto_detect_v2", workflow.DefaultVersion, 1)
	if autoDetectVersion >= 1 && !rolePresent && detectBrowserIntent(input.Query) {
		logger.Info("Auto-detected browser intent, assigning browser_use role")
		if input.Context == nil {
			input.Context = map[string]interface{}{} // 确保 context 存在，后面要写入自动检测标记。
		}
		input.Context["role"] = "browser_use"      // role：自动推断当前任务应该走 browser_use。
		input.Context["role_auto_detected"] = true // role_auto_detected：标记这是系统自动识别的，不是用户手动指定。
		input.Context["force_tools"] = true        // force_tools：强制下游浏览器策略真正使用工具。

		// 把自动检测到的角色也显式告诉前端，方便用户理解系统为什么切换到浏览器模式。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,                    // WorkflowID：当前工作流 ID。
			EventType:  activities.StreamEventRoleAssigned,                            // EventType：角色分配事件。
			AgentID:    "browser_use",                                                 // AgentID：消息来源使用 browser_use。
			Message:    activities.MsgRoleAssigned("browser_use (auto-detected)", 10), // Message：告诉前端这是自动识别出来的角色。
			Timestamp:  workflow.Now(ctx),                                             // Timestamp：识别时间。
			Payload: map[string]interface{}{
				"role":          "browser_use", // role：自动检测到的角色名。
				"auto_detected": true,          // auto_detected：标记这是自动推断结果。
			},
		}).Get(ctx, nil)

		strategy := "browser_use" // strategy：默认在新版里走 browser_use。
		if browserUseVersion == 1 {
			strategy = "react" // 老版本兼容时回落到 react 策略。
		}
		if result, handled, err := routeStrategyWorkflow(ctx, input, strategy, decomp.Mode, emitCtx, controlHandler); handled {
			return result, err // 自动识别后的浏览器策略接管成功则直接返回。
		}
	}

	// 如果分解器返回了明确的认知策略，就优先按该策略路由。
	// 只有 direct / decompose 这类“通用标签”不会在这里直接接管。
	if decomp.CognitiveStrategy != "" && decomp.CognitiveStrategy != "direct" && decomp.CognitiveStrategy != "decompose" {
		if result, handled, err := routeStrategyWorkflow(ctx, input, decomp.CognitiveStrategy, decomp.Mode, emitCtx, controlHandler); handled {
			return result, err // 认知策略命中具体工作流后直接结束。
		}
		logger.Warn("Unknown cognitive strategy; continuing routing", "strategy", decomp.CognitiveStrategy) // 未知认知策略只打警告，继续走后续通用路由。
	}

	// 这里保留一层 force_research 兜底判断，兼容通过 CLI、定时任务等入口传入的上下文字段。
	// GetContextBool 之所以重要，是因为某些 proto 或 map<string,string> 场景会把 true 变成字符串。
	if GetContextBool(input.Context, "force_research") {
		logger.Info("Forcing ResearchWorkflow via context flag")
		if result, handled, err := routeStrategyWorkflow(ctx, input, "research", decomp.Mode, emitCtx, controlHandler); handled {
			return result, err // research 策略一旦接管，直接返回。
		}
	}

	forceP2P := GetContextBool(input.Context, "force_p2p") // forceP2P：调用方是否强制要求 P2P 协同。
	if forceP2P {
		logger.Info("P2P coordination forced via context flag") // 记录本次是由外部显式要求走 P2P 路线。
	}

	// hasDeps 用于判断任务间是否存在显式依赖；如果 forceP2P=true，直接视为存在依赖。
	hasDeps := forceP2P
	if !hasDeps {
		for _, st := range decomp.Subtasks {
			if len(st.Dependencies) > 0 || len(st.Consumes) > 0 {
				hasDeps = true // 只要子任务声明了 depends-on 或 consumes，就认为存在任务间数据/执行依赖。
				break
			}
		}
	}

	switch {
	case isSimple && !forceP2P:
		// 命中简单路径时，先做最后一次 pause/cancel 检查，避免刚要起子工作流时状态已变化。
		if err := controlHandler.CheckPausePoint(ctx, "pre_simple_workflow"); err != nil {
			return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 被暂停或取消时直接结束。
		}
		// 简单任务仍然放到子工作流里执行，这样能保持父工作流职责清晰，也便于隔离重试和事件流。
		var result TaskResult                                                           // result：接收简单子工作流的业务结果。
		ometrics.WorkflowsStarted.WithLabelValues("SimpleTaskWorkflow", "simple").Inc() // 记录 simple 路由命中指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: parentWorkflowID,                 // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation, // EventType：委派事件。
			AgentID:    "orchestrator",                   // AgentID：由 orchestrator 发出。
			Message:    activities.MsgHandoffSimple(),    // Message：提示前端切到简单执行模式。
			Timestamp:  workflow.Now(ctx),                // Timestamp：委派时间。
		}).Get(ctx, nil)

		// 如果分解器给简单任务推荐了工具，则顺手传给 SimpleTaskWorkflow，
		// 这样简单工作流里的 agent 就不必再自己猜该用什么工具。
		if len(decomp.Subtasks) > 0 && len(decomp.Subtasks[0].SuggestedTools) > 0 {
			input.SuggestedTools = decomp.Subtasks[0].SuggestedTools // SuggestedTools：继承第一个子任务推荐的工具列表。
			input.ToolParameters = decomp.Subtasks[0].ToolParameters // ToolParameters：继承对应工具参数。
		}

		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时同步取消简单子工作流。
		})
		childFuture := workflow.ExecuteChildWorkflow(childCtx, SimpleTaskWorkflow, input) // 启动简单子工作流。
		var childExec workflow.Execution                                                  // childExec：保存子工作流执行信息。
		if err := childFuture.GetChildWorkflowExecution().Get(childCtx, &childExec); err != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, err // 子工作流没启动成功时直接失败。
		}
		controlHandler.RegisterChildWorkflow(childExec.ID)   // 注册子工作流，便于控制信号下发。
		execErr := childFuture.Get(childCtx, &result)        // 阻塞等待简单子工作流结束。
		controlHandler.UnregisterChildWorkflow(childExec.ID) // 结束后清理注册信息。

		scheduleStreamEnd(ctx) // 不管成功失败，都结束这条事件流。

		if execErr != nil {
			controlHandler.EmitCancelledIfNeeded(ctx, execErr.Error()) // 如果子工作流是被取消的，补发取消事件。
			result = AddTaskContextToMetadata(result, input.Context)   // 即使失败也尽量把上下文写回结果。
			return result, execErr                                     // 返回业务结果和底层错误。
		}
		result = AddTaskContextToMetadata(result, input.Context) // 成功时同样补齐 metadata。
		return result, nil                                       // 简单路径执行完成。

	case false: // 这里显式写成 false，表示当前版本已禁用 Supervisor 路由，保留代码只为以后恢复更方便。
		if err := controlHandler.CheckPausePoint(ctx, "pre_supervisor_workflow"); err != nil {
			return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 如果未来重新启用 supervisor，也保留这层暂停点。
		}
		var result TaskResult                                                            // result：接收 SupervisorWorkflow 的返回结果。
		ometrics.WorkflowsStarted.WithLabelValues("SupervisorWorkflow", "complex").Inc() // 记录 supervisor 路由启动指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: parentWorkflowID,                  // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation,  // EventType：委派事件。
			AgentID:    "orchestrator",                    // AgentID：orchestrator。
			Message:    activities.MsgHandoffSupervisor(), // Message：前端展示“交给 SupervisorWorkflow 处理”。
			Timestamp:  workflow.Now(ctx),                 // Timestamp：委派时间。
		}).Get(ctx, nil)
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时取消 supervisor 子工作流。
		})
		childFuture := workflow.ExecuteChildWorkflow(childCtx, SupervisorWorkflow, input) // 启动 SupervisorWorkflow。
		var childExec workflow.Execution                                                  // childExec：保存子工作流执行信息。
		if err := childFuture.GetChildWorkflowExecution().Get(childCtx, &childExec); err != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, err
		}
		controlHandler.RegisterChildWorkflow(childExec.ID)   // 注册子工作流。
		execErr := childFuture.Get(childCtx, &result)        // 等待 supervisor 路径完成。
		controlHandler.UnregisterChildWorkflow(childExec.ID) // 结束后移除注册。

		scheduleStreamEnd(ctx) // 结束事件流。

		if execErr != nil {
			controlHandler.EmitCancelledIfNeeded(ctx, execErr.Error()) // 如有取消，补发取消事件。
			result = AddTaskContextToMetadata(result, input.Context)   // 补齐上下文。
			return result, execErr                                     // 返回 supervisor 路径错误。
		}
		result = AddTaskContextToMetadata(result, input.Context) // 补齐 metadata。
		return result, nil                                       // supervisor 路径执行完成。

	default:
		// 默认分支走 DAGWorkflow，适用于多子任务、多依赖、需要扇出扇入的标准团队编排场景。
		if err := controlHandler.CheckPausePoint(ctx, "pre_dag_workflow"); err != nil {
			return TaskResult{Success: false, ErrorMessage: err.Error()}, err // 启动 DAG 之前仍然先尊重 pause/cancel。
		}
		ometrics.WorkflowsStarted.WithLabelValues("DAGWorkflow", "standard").Inc() // 记录 DAG 路由启动指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: parentWorkflowID,                 // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation, // EventType：委派事件。
			AgentID:    "orchestrator",                   // AgentID：orchestrator。
			Message:    activities.MsgHandoffTeamPlan(),  // Message：告诉前端接下来会按团队计划执行。
			Timestamp:  workflow.Now(ctx),                // Timestamp：委派时间。
		}).Get(ctx, nil)
		strategiesInput := convertToStrategiesInput(input) // 转换成 strategies 包的输入格式。
		var strategiesResult strategies.TaskResult         // strategiesResult：接收 DAGWorkflow 结果。
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时取消 DAG 子工作流。
		})
		dagFuture := workflow.ExecuteChildWorkflow(childCtx, strategies.DAGWorkflow, strategiesInput) // 启动 DAGWorkflow。
		var dagExec workflow.Execution                                                                // dagExec：保存 DAG 子工作流执行信息。
		if err := dagFuture.GetChildWorkflowExecution().Get(childCtx, &dagExec); err != nil {
			return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, err // 子工作流启动失败直接结束。
		}
		controlHandler.RegisterChildWorkflow(dagExec.ID)      // 注册 DAG 子工作流，纳入控制管理。
		execErr := dagFuture.Get(childCtx, &strategiesResult) // 等待 DAGWorkflow 执行结束。
		controlHandler.UnregisterChildWorkflow(dagExec.ID)    // 结束后移除注册。

		scheduleStreamEnd(ctx) // 结束这条执行路径的事件流。

		if execErr != nil {
			controlHandler.EmitCancelledIfNeeded(ctx, execErr.Error())                                                // 如底层是取消，则补发取消事件。
			out := AddTaskContextToMetadata(TaskResult{Success: false, ErrorMessage: execErr.Error()}, input.Context) // out：携带上下文的失败结果。
			return out, execErr                                                                                       // 返回 DAG 路径错误。
		}
		result := convertFromStrategiesResult(strategiesResult)  // 把 strategies 结果转换回 workflows 结果结构。
		result = AddTaskContextToMetadata(result, input.Context) // 补齐元数据。
		return result, nil                                       // DAG 路径正常结束。
	}
}

// startAsyncTitleGeneration 会在工作流一启动时，后台异步触发“会话标题生成”。
// 这个函数故意不阻塞主流程，因为标题本质上是体验增强项，不应该延迟真实任务执行。
// 它的执行特点是：
// 1. 通过版本门控制新老行为，保证 Temporal 重放安全。
// 2. 使用 workflow.Go 在后台并行执行。
// 3. 采用短超时 + 不重试的 best-effort 策略，失败也不影响主工作流。
func startAsyncTitleGeneration(ctx workflow.Context, sessionID, query string) {
	titleVersion := workflow.GetVersion(ctx, "session_title_async_v1", workflow.DefaultVersion, 1) // titleVersion：保护“异步标题生成”这项新行为。
	if titleVersion < 1 {
		return // 老工作流回放到这里时，直接跳过新逻辑。
	}
	if sessionID == "" {
		return // 没有 sessionID 时无法把标题写回会话，直接跳过。
	}

	// workflow.Go 会在 Temporal 工作流内部开启一个轻量协程；
	// 这里采用 fire-and-forget 方式，不等待标题生成结果。
	workflow.Go(ctx, func(gCtx workflow.Context) {
		titleOpts := workflow.ActivityOptions{
			StartToCloseTimeout: 15 * time.Second, // StartToCloseTimeout：给标题生成 15 秒执行时间。
			RetryPolicy: &temporal.RetryPolicy{
				MaximumAttempts: 1, // MaximumAttempts：只尝试一次，失败也不重试。
			},
		}
		titleCtx := workflow.WithActivityOptions(gCtx, titleOpts) // titleCtx：给标题生成 Activity 绑定专用配置。

		// 这里显式忽略错误，因为标题生成不是主链路成功条件。
		_ = workflow.ExecuteActivity(titleCtx, "GenerateSessionTitle", activities.GenerateSessionTitleInput{
			SessionID: sessionID, // SessionID：告诉 Activity 要给哪个会话生成标题。
			Query:     query,     // Query：标题生成的核心语义来源，通常就是用户首条问题。
		}).Get(titleCtx, nil)
	})
}

// scheduleStreamEnd 负责补发一个 STREAM_END 事件，告诉前端：
// “这一条工作流路径已经结束，不会再继续往 SSE 流里写内容了”。
// 之所以单独抽成函数，是因为主工作流的多个 return 分支都需要复用这段收尾逻辑。
func scheduleStreamEnd(ctx workflow.Context) {
	streamEndVersion := workflow.GetVersion(ctx, "stream_end_v1", workflow.DefaultVersion, 1) // streamEndVersion：保护“发 STREAM_END”这一新行为。
	if streamEndVersion < 1 {
		return // 老工作流回放时不执行这段新逻辑。
	}

	emitCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Second,                           // StartToCloseTimeout：发流结束事件的短超时。
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1}, // RetryPolicy：只尝试一次，失败不重试。
	})
	_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
		WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID, // WorkflowID：当前父工作流 ID。
		EventType:  activities.StreamEventStreamEnd,            // EventType：专门表示流结束。
		AgentID:    "orchestrator",                             // AgentID：收尾消息由 orchestrator 发出。
		Message:    activities.MsgStreamEnd(),                  // Message：给前端消费的流结束文案。
		Timestamp:  workflow.Now(ctx),                          // Timestamp：流结束时间。
	}).Get(emitCtx, nil)
}

// convertToStrategiesInput 把 workflows 包里的 TaskInput 映射成 strategies 包使用的 TaskInput。
// 之所以需要这层转换，是因为两个包虽然语义相近，但为了避免循环依赖和边界耦合，各自维护了自己的类型。
func convertToStrategiesInput(input TaskInput) strategies.TaskInput {
	history := make([]strategies.Message, len(input.History)) // history：预先分配与原始历史等长的目标切片。
	for i, msg := range input.History {
		history[i] = strategies.Message{
			Role:      msg.Role,      // Role：消息角色，例如 user / assistant。
			Content:   msg.Content,   // Content：消息正文。
			Timestamp: msg.Timestamp, // Timestamp：消息时间戳。
		}
	}

	return strategies.TaskInput{
		Query:                   input.Query,                   // Query：用户原始问题。
		UserID:                  input.UserID,                  // UserID：当前用户 ID。
		TenantID:                input.TenantID,                // TenantID：当前租户 ID。
		SessionID:               input.SessionID,               // SessionID：当前会话 ID。
		Context:                 input.Context,                 // Context：透传的动态上下文参数。
		Mode:                    input.Mode,                    // Mode：任务模式标签。
		TemplateName:            input.TemplateName,            // TemplateName：模板名称。
		TemplateVersion:         input.TemplateVersion,         // TemplateVersion：模板版本。
		DisableAI:               input.DisableAI,               // DisableAI：是否禁用 AI 兜底能力。
		History:                 history,                       // History：转换后的历史消息切片。
		SessionCtx:              input.SessionCtx,              // SessionCtx：会话级上下文。
		RequireApproval:         input.RequireApproval,         // RequireApproval：是否显式要求审批。
		ApprovalTimeout:         input.ApprovalTimeout,         // ApprovalTimeout：审批等待时间。
		BypassSingleResult:      input.BypassSingleResult,      // BypassSingleResult：是否跳过单结果捷径。
		ParentWorkflowID:        input.ParentWorkflowID,        // ParentWorkflowID：父工作流 ID。
		PreplannedDecomposition: input.PreplannedDecomposition, // PreplannedDecomposition：预先生成好的分解结果。
	}
}

// convertFromStrategiesResult 把 strategies 包的 TaskResult 映射回 workflows 包的 TaskResult。
// 这层转换能把底层策略工作流和上层编排接口隔开，避免不同包之间互相泄漏类型定义。
func convertFromStrategiesResult(result strategies.TaskResult) TaskResult {
	return TaskResult{
		Result:       result.Result,       // Result：子策略工作流产出的最终文本或结构化结果。
		Success:      result.Success,      // Success：业务是否成功。
		TokensUsed:   result.TokensUsed,   // TokensUsed：执行过程中消耗的 token 总量。
		ErrorMessage: result.ErrorMessage, // ErrorMessage：失败时的错误说明。
		Metadata:     result.Metadata,     // Metadata：补充元数据，例如上下文、统计信息等。
	}
}

// extractTemplateRequest 从 TaskInput 及其 Context 中提取模板名和模板版本。
// 该函数兼容多种字段来源，目的是把“模板请求”的解析逻辑集中到一处，减少主流程分支里的重复代码。
func extractTemplateRequest(input TaskInput) (string, string) {
	name := strings.TrimSpace(input.TemplateName)       // name：优先使用顶层字段里的模板名，并去掉首尾空白。
	version := strings.TrimSpace(input.TemplateVersion) // version：优先使用顶层字段里的模板版本。

	if name == "" && input.Context != nil {
		if v, ok := input.Context["template"].(string); ok {
			name = strings.TrimSpace(v) // 如果顶层没给模板名，就尝试从 context.template 读取。
		}
		if name == "" {
			if v2, ok2 := input.Context["template_name"].(string); ok2 {
				name = strings.TrimSpace(v2) // 再兼容旧字段 template_name。
			}
		}
	}
	if version == "" && input.Context != nil {
		if v, ok := input.Context["template_version"].(string); ok {
			version = strings.TrimSpace(v) // 如果顶层没给版本，就尝试从 context.template_version 读取。
		}
	}
	return name, version // 返回最终解析出的模板名和模板版本。
}

// routeStrategyWorkflow 根据 strategy 字符串，把任务交给对应的策略工作流。
// 返回值语义：
// TaskResult：如果 handled=true，则这里返回的是目标工作流执行后的结果。
// bool：表示当前 strategy 是否被这个函数识别并接管。
// error：表示子工作流启动或执行过程中产生的底层错误。
func routeStrategyWorkflow(ctx workflow.Context, input TaskInput, strategy string, mode string, emitCtx workflow.Context, controlHandler *ControlSignalHandler) (TaskResult, bool, error) {
	strategyLower := strings.ToLower(strings.TrimSpace(strategy)) // strategyLower：把策略名统一成小写并去空白，便于稳定比较。
	if strategyLower == "" {
		return TaskResult{}, false, nil // 空策略表示这里无法接管，交给外层继续判断。
	}

	switch strategyLower {
	case "simple":
		// simple 策略直接委派给 SimpleTaskWorkflow。
		if controlHandler != nil {
			if err := controlHandler.CheckPausePoint(ctx, "pre_simple_strategy"); err != nil {
				return TaskResult{Success: false, ErrorMessage: err.Error()}, true, err // 已识别为 simple，但在真正启动前被暂停或取消。
			}
		}
		var result TaskResult                                                       // result：接收 SimpleTaskWorkflow 的结果。
		ometrics.WorkflowsStarted.WithLabelValues("SimpleTaskWorkflow", mode).Inc() // 记录 simple 策略启动指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,    // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation,              // EventType：委派事件。
			AgentID:    "orchestrator",                                // AgentID：orchestrator。
			Message:    activities.MsgWorkflowRouting("simple", mode), // Message：说明当前是因什么 mode 路由到 simple。
			Timestamp:  workflow.Now(ctx),                             // Timestamp：委派时间。
		}).Get(ctx, nil)
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时同步取消简单子工作流。
		})
		childFuture := workflow.ExecuteChildWorkflow(childCtx, SimpleTaskWorkflow, input) // 启动 SimpleTaskWorkflow。
		var childExecID string                                                            // childExecID：用于记录被注册到 controlHandler 的子工作流 ID。
		if controlHandler != nil {
			var childExec workflow.Execution
			if err := childFuture.GetChildWorkflowExecution().Get(childCtx, &childExec); err != nil {
				return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, true, err // 已接管但子工作流启动失败。
			}
			childExecID = childExec.ID                        // 保存真实子工作流 ID。
			controlHandler.RegisterChildWorkflow(childExecID) // 注册到控制处理器，支持 pause/cancel 透传。
		}
		execErr := childFuture.Get(childCtx, &result) // 等待 simple 子工作流执行结束。
		if controlHandler != nil && childExecID != "" {
			controlHandler.UnregisterChildWorkflow(childExecID) // 执行结束后清理注册状态。
		}

		scheduleStreamEnd(ctx) // 结束事件流。

		if execErr != nil {
			if controlHandler != nil {
				controlHandler.EmitCancelledIfNeeded(ctx, execErr.Error()) // 如果错误实质是取消，则补发取消事件。
			}
			result = AddTaskContextToMetadata(result, input.Context) // 失败时也尽量把上下文补齐。
			return result, true, execErr                             // handled=true 表示 strategy 已经被本函数接管处理。
		}
		result = AddTaskContextToMetadata(result, input.Context) // 成功时补齐上下文元数据。
		return result, true, nil                                 // 返回 simple 路径结果。
	case "react", "exploratory", "research", "scientific", "ads_research", "browser_use", "competitor_watch", "morning_brief", "seo_rank":
		// 这一组策略都归入“特定策略工作流”路径。
		if controlHandler != nil {
			if err := controlHandler.CheckPausePoint(ctx, "pre_"+strategyLower+"_workflow"); err != nil {
				return TaskResult{Success: false, ErrorMessage: err.Error()}, true, err // 启动前若被暂停/取消，则直接返回。
			}
		}
		var wfName string      // wfName：用于指标和事件消息的人类可读工作流名。
		var wfFunc interface{} // wfFunc：真正要执行的工作流函数引用。
		switch strategyLower {
		case "react":
			wfName = "ReactWorkflow" // ReactWorkflow：偏工具驱动、逐步思考的策略工作流。
			wfFunc = strategies.ReactWorkflow
		case "exploratory":
			wfName = "ExploratoryWorkflow" // ExploratoryWorkflow：偏探索式任务的策略工作流。
			wfFunc = strategies.ExploratoryWorkflow
		case "research":
			wfName = "ResearchWorkflow" // ResearchWorkflow：偏研究型任务的策略工作流。
			wfFunc = strategies.ResearchWorkflow
		case "scientific":
			wfName = "ScientificWorkflow" // ScientificWorkflow：偏科学研究范式的策略工作流。
			wfFunc = strategies.ScientificWorkflow
		case "browser_use":
			wfName = "BrowserUseWorkflow" // BrowserUseWorkflow：必须依赖真实浏览器执行的策略工作流。
			wfFunc = strategies.BrowserUseWorkflow
		}

		strategiesInput := convertToStrategiesInput(input)            // 把输入转换成 strategies 包使用的结构。
		var strategiesResult strategies.TaskResult                    // strategiesResult：接收策略工作流结果。
		ometrics.WorkflowsStarted.WithLabelValues(wfName, mode).Inc() // 记录具体策略工作流的启动指标。
		_ = workflow.ExecuteActivity(emitCtx, "EmitTaskUpdate", activities.EmitTaskUpdateInput{
			WorkflowID: workflow.GetInfo(ctx).WorkflowExecution.ID,  // WorkflowID：父工作流 ID。
			EventType:  activities.StreamEventDelegation,            // EventType：委派事件。
			AgentID:    "orchestrator",                              // AgentID：orchestrator。
			Message:    activities.MsgWorkflowRouting(wfName, mode), // Message：告诉前端路由到了哪个具体工作流。
			Timestamp:  workflow.Now(ctx),                           // Timestamp：委派时间。
		}).Get(ctx, nil)
		childCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
			ParentClosePolicy: enums.PARENT_CLOSE_POLICY_REQUEST_CANCEL, // 父级关闭时请求取消策略子工作流。
		})
		strategyFuture := workflow.ExecuteChildWorkflow(childCtx, wfFunc, strategiesInput) // 启动具体策略工作流。
		var strategyExecID string                                                          // strategyExecID：记录已注册的策略子工作流 ID。
		if controlHandler != nil {
			var strategyExec workflow.Execution
			if err := strategyFuture.GetChildWorkflowExecution().Get(childCtx, &strategyExec); err != nil {
				return TaskResult{Success: false, ErrorMessage: fmt.Sprintf("Failed to get child execution: %v", err)}, true, err // 子工作流没启动成功时直接失败。
			}
			strategyExecID = strategyExec.ID                     // 取出真实执行 ID。
			controlHandler.RegisterChildWorkflow(strategyExecID) // 注册到控制器。
		}
		execErr := strategyFuture.Get(childCtx, &strategiesResult) // 等待策略工作流执行结束。
		if controlHandler != nil && strategyExecID != "" {
			controlHandler.UnregisterChildWorkflow(strategyExecID) // 执行完成后移除注册。
		}

		scheduleStreamEnd(ctx) // 结束事件流。

		if execErr != nil {
			if controlHandler != nil {
				controlHandler.EmitCancelledIfNeeded(ctx, execErr.Error()) // 若是取消错误，则补发取消事件。
			}
			res := AddTaskContextToMetadata(TaskResult{}, input.Context) // 失败时返回一个带上下文的空结果壳。
			return res, true, execErr
		}
		result := convertFromStrategiesResult(strategiesResult)  // 转回 workflows 层的结果结构。
		result = AddTaskContextToMetadata(result, input.Context) // 补齐上下文元数据。
		return result, true, nil                                 // 返回策略工作流执行结果。
	default:
		return TaskResult{}, false, nil // 未识别的策略名，不接管，交给外层继续判断。
	}
}

// recommendStrategy 调用学习路由 Activity，尝试根据历史任务数据推荐更合适的执行策略。
// 这个函数除了返回推荐结果外，还会异步记录一条学习路由观测指标。
func recommendStrategy(ctx workflow.Context, input TaskInput) (*activities.RecommendStrategyOutput, error) {
	startTime := workflow.Now(ctx) // startTime：记录推荐开始时间，用于后面计算延迟。

	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Second, // StartToCloseTimeout：推荐路由属于轻量 Activity，给 10 秒即可。
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts: 2, // MaximumAttempts：最多重试 2 次，兼顾稳定性和延迟。
		},
	})

	var rec activities.RecommendStrategyOutput // rec：保存学习路由返回的推荐结果。
	err := workflow.ExecuteActivity(actx, activities.RecommendWorkflowStrategy, activities.RecommendStrategyInput{
		SessionID: input.SessionID, // SessionID：当前会话 ID。
		UserID:    input.UserID,    // UserID：当前用户 ID。
		TenantID:  input.TenantID,  // TenantID：当前租户 ID。
		Query:     input.Query,     // Query：当前问题，用于做策略推荐。
	}).Get(ctx, &rec)

	// metricsCtx 专门用于记录指标。
	// 这里仍然使用短超时和单次尝试，因为指标失败不应该影响主逻辑。
	metricsCtx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: 2 * time.Second,                           // StartToCloseTimeout：记录指标应尽快完成。
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 1}, // RetryPolicy：不重试，避免重复记录。
	})

	latency := workflow.Now(ctx).Sub(startTime).Seconds() // latency：推荐 Activity 总耗时（秒）。
	strategy := "none"                                    // strategy：默认表示没有推荐结果。
	source := "none"                                      // source：默认表示没有来源。
	confidence := 0.0                                     // confidence：默认置信度为 0。
	success := false                                      // success：默认认为推荐失败。

	if err == nil && rec.Strategy != "" {
		strategy = rec.Strategy     // strategy：成功时写入推荐出的策略名。
		source = rec.Source         // source：成功时写入推荐来源。
		confidence = rec.Confidence // confidence：成功时写入置信度。
		success = true              // success：标记这次推荐有效。
	}

	workflow.ExecuteActivity(
		metricsCtx,
		"RecordLearningRouterMetrics",
		map[string]interface{}{
			"latency_seconds": latency,    // latency_seconds：推荐耗时。
			"strategy":        strategy,   // strategy：推荐出的策略名或 none。
			"source":          source,     // source：推荐来源或 none。
			"confidence":      confidence, // confidence：推荐置信度。
			"success":         success,    // success：本次推荐是否成功。
		},
	)

	if err != nil {
		return nil, err // 推荐 Activity 失败时直接返回 nil 和错误。
	}
	return &rec, nil // 返回推荐结果指针。
}

// detectBrowserIntent 用于做一个非常保守的浏览器意图识别。
// 它不会看到 URL 就直接返回 true，而只会在“明显必须依赖真实浏览器渲染”的网站上命中。
// 这样做的目标是避免把本可用 web_fetch / web_search 完成的任务，错误地升级成昂贵的浏览器自动化任务。
func detectBrowserIntent(query string) bool {
	q := strings.ToLower(query) // q：把用户输入统一转成小写，便于做不区分大小写的域名匹配。

	// jsRequiredDomains 列出那些必须使用真实浏览器渲染的网站域名。
	// 这些站点通常依赖重 JavaScript、反爬策略或动态加载，简单 HTTP 抓取不够可靠。
	jsRequiredDomains := []string{
		"weixin.qq.com",    // weixin.qq.com：微信文章页，通常需要较重 JS 环境。
		"mp.weixin.qq.com", // mp.weixin.qq.com：微信公众号文章域名。
	}
	for _, domain := range jsRequiredDomains {
		if strings.Contains(q, domain) {
			return true // 只要 query 中包含任一必须 JS 渲染的域名，就认定需要浏览器自动化。
		}
	}

	return false // 其他普通 URL 仍交给正常分解流程，由系统选择 web_fetch、web_search 或 research 等工具链。
}
