package events

const (
	// KindAgentTurnStart is emitted when an agent turn starts.
	KindAgentTurnStart Kind = "agent.turn.start"
	// KindAgentTurnEnd is emitted when an agent turn ends.
	KindAgentTurnEnd Kind = "agent.turn.end"

	// KindAgentLLMRequest is emitted before an LLM request.
	KindAgentLLMRequest Kind = "agent.llm.request"
	// KindAgentLLMDelta is emitted for streaming LLM deltas.
	KindAgentLLMDelta Kind = "agent.llm.delta"
	// KindAgentLLMResponse is emitted after an LLM response.
	KindAgentLLMResponse Kind = "agent.llm.response"
	// KindAgentLLMRetry is emitted before retrying an LLM request.
	KindAgentLLMRetry Kind = "agent.llm.retry"

	// KindAgentContextCompress is emitted when agent context is compressed.
	KindAgentContextCompress Kind = "agent.context.compress"
	// KindAgentSessionSummarize is emitted when session summarization completes.
	KindAgentSessionSummarize Kind = "agent.session.summarize"

	// KindAgentToolExecStart is emitted before a tool executes.
	KindAgentToolExecStart Kind = "agent.tool.exec_start"
	// KindAgentToolExecEnd is emitted after a tool finishes.
	KindAgentToolExecEnd Kind = "agent.tool.exec_end"
	// KindAgentToolExecSkipped is emitted when a tool call is skipped.
	KindAgentToolExecSkipped Kind = "agent.tool.exec_skipped"

	// KindAgentSteeringInjected is emitted when steering is injected into context.
	KindAgentSteeringInjected Kind = "agent.steering.injected"
	// KindAgentFollowUpQueued is emitted when async follow-up input is queued.
	KindAgentFollowUpQueued Kind = "agent.follow_up.queued"
	// KindAgentInterruptReceived is emitted when a turn interrupt is accepted.
	KindAgentInterruptReceived Kind = "agent.interrupt.received"

	// KindAgentSubTurnSpawn is emitted when a sub-turn is spawned.
	KindAgentSubTurnSpawn Kind = "agent.subturn.spawn"
	// KindAgentSubTurnEnd is emitted when a sub-turn ends.
	KindAgentSubTurnEnd Kind = "agent.subturn.end"
	// KindAgentSubTurnResultDelivered is emitted when a sub-turn result is delivered.
	KindAgentSubTurnResultDelivered Kind = "agent.subturn.result_delivered"
	// KindAgentSubTurnOrphan is emitted when a sub-turn result cannot be delivered.
	KindAgentSubTurnOrphan Kind = "agent.subturn.orphan"
	// KindAgentError is emitted when agent execution reports an error.
	KindAgentError Kind = "agent.error"

	// KindChannelLifecycleStarted is emitted when a channel starts.
	KindChannelLifecycleStarted Kind = "channel.lifecycle.started"
	// KindChannelLifecycleInitialized is emitted when a channel is initialized.
	KindChannelLifecycleInitialized Kind = "channel.lifecycle.initialized"
	// KindChannelLifecycleStartFailed is emitted when a channel fails to start.
	KindChannelLifecycleStartFailed Kind = "channel.lifecycle.start_failed"
	// KindChannelLifecycleStopped is emitted when a channel stops.
	KindChannelLifecycleStopped Kind = "channel.lifecycle.stopped"
	// KindChannelWebhookRegistered is emitted when a channel webhook is registered.
	KindChannelWebhookRegistered Kind = "channel.webhook.registered"
	// KindChannelWebhookUnregistered is emitted when a channel webhook is unregistered.
	KindChannelWebhookUnregistered Kind = "channel.webhook.unregistered"
	// KindChannelMessageOutboundQueued is emitted when an outbound message is queued.
	KindChannelMessageOutboundQueued Kind = "channel.message.outbound_queued"
	// KindChannelMessageOutboundSent is emitted when an outbound channel message is sent.
	KindChannelMessageOutboundSent Kind = "channel.message.outbound_sent"
	// KindChannelMessageOutboundFailed is emitted when an outbound channel message fails.
	KindChannelMessageOutboundFailed Kind = "channel.message.outbound_failed"
	// KindChannelRateLimited is emitted when channel rate limiting blocks delivery.
	KindChannelRateLimited Kind = "channel.rate_limited"

	// KindBusPublishFailed is emitted when message bus publish fails.
	KindBusPublishFailed Kind = "bus.publish.failed"
	// KindBusMessageDropped is emitted when a message is dropped due to
	// backpressure (channel full for longer than the drop budget).
	KindBusMessageDropped Kind = "bus.message.dropped"
	// KindBusCloseStarted is emitted when message bus close starts.
	KindBusCloseStarted Kind = "bus.close.started"
	// KindBusCloseCompleted is emitted when message bus close completes.
	KindBusCloseCompleted Kind = "bus.close.completed"
	// KindBusCloseDrained is emitted when message bus close drains buffered messages.
	KindBusCloseDrained Kind = "bus.close.drained"

	// KindGatewayStart is emitted when gateway startup reaches runtime bootstrap.
	KindGatewayStart Kind = "gateway.start"
	// KindGatewayReady is emitted when gateway services are started and ready.
	KindGatewayReady Kind = "gateway.ready"
	// KindGatewayShutdown is emitted when gateway shutdown starts.
	KindGatewayShutdown Kind = "gateway.shutdown"
	// KindGatewayReloadStarted is emitted when gateway reload starts.
	KindGatewayReloadStarted Kind = "gateway.reload.started"
	// KindGatewayReloadCompleted is emitted when gateway reload completes.
	KindGatewayReloadCompleted Kind = "gateway.reload.completed"
	// KindGatewayReloadFailed is emitted when gateway reload fails.
	KindGatewayReloadFailed Kind = "gateway.reload.failed"

	// KindMCPServerConnected is emitted when an MCP server connects.
	KindMCPServerConnected Kind = "mcp.server.connected"
	// KindMCPServerConnecting is emitted before connecting to an MCP server.
	KindMCPServerConnecting Kind = "mcp.server.connecting"
	// KindMCPServerFailed is emitted when an MCP server fails.
	KindMCPServerFailed Kind = "mcp.server.failed"
	// KindMCPToolDiscovered is emitted when an MCP tool is discovered.
	KindMCPToolDiscovered Kind = "mcp.tool.discovered"
	// KindMCPToolCallStart is emitted when an MCP tool call starts.
	KindMCPToolCallStart Kind = "mcp.tool.call.start"
	// KindMCPToolCallEnd is emitted when an MCP tool call ends.
	KindMCPToolCallEnd Kind = "mcp.tool.call.end"

	// Perception: observations of the outside world. Published by sources
	// (MR-202 onward); nothing publishes them yet.

	// KindPerceptionMarketTick is emitted for a market price tick.
	KindPerceptionMarketTick Kind = "perception.market.tick"
	// KindPerceptionMarketCandleClosed is emitted when a market candle closes.
	KindPerceptionMarketCandleClosed Kind = "perception.market.candle_closed"
	// KindPerceptionPortfolioSnapshot is emitted for a portfolio snapshot.
	KindPerceptionPortfolioSnapshot Kind = "perception.portfolio.snapshot"
	// KindPerceptionFuturesPositions is emitted for a futures positions sample.
	KindPerceptionFuturesPositions Kind = "perception.futures.positions"
	// KindPerceptionOrderFilled is emitted when an order fills.
	KindPerceptionOrderFilled Kind = "perception.order.filled"
	// KindPerceptionOrderPlaced is emitted when an order is placed.
	KindPerceptionOrderPlaced Kind = "perception.order.placed"
	// KindPerceptionPlanHealth is emitted for a plan health observation.
	KindPerceptionPlanHealth Kind = "perception.plan.health"
	// KindPerceptionPlanDCADue is emitted when a DCA plan order is due.
	KindPerceptionPlanDCADue Kind = "perception.plan.dca_due"
	// KindPerceptionClockSession is emitted at a market session boundary.
	KindPerceptionClockSession Kind = "perception.clock.session"
	// KindPerceptionClockDayStart is emitted at the start of a day.
	KindPerceptionClockDayStart Kind = "perception.clock.day_start"
	// KindPerceptionClockTimer is emitted when a timer scheduled on the clock source
	// comes due (a reminder).
	KindPerceptionClockTimer Kind = "perception.clock.timer"
	// KindPerceptionExternalTrigger is emitted for a trigger from outside the process.
	KindPerceptionExternalTrigger Kind = "perception.external.trigger"
	// KindPerceptionDevice is emitted for a device observation.
	KindPerceptionDevice Kind = "perception.device"
	// KindPerceptionNewsItem is emitted for a news item.
	KindPerceptionNewsItem Kind = "perception.news.item"
	// KindPerceptionSourceHealth is emitted when a perception source's health changes.
	KindPerceptionSourceHealth Kind = "perception.source.health"

	// Proactive pipeline: what was derived from perception and what became of it.

	// KindWorldModelDiff is emitted when the world model changes.
	KindWorldModelDiff Kind = "worldmodel.diff"
	// KindTriggerCandidate is emitted when a trigger proposes a candidate.
	KindTriggerCandidate Kind = "trigger.candidate"
	// KindAttentionDecision is emitted when the attention governor decides on a candidate.
	KindAttentionDecision Kind = "attention.decision"
	// KindNotifyDelivered is emitted when an owner notification is delivered.
	KindNotifyDelivered Kind = "notify.delivered"

	// Approvals, journal and control.

	// KindApprovalCreated is emitted when an approval proposal is created.
	KindApprovalCreated Kind = "approval.created"
	// KindApprovalDecided is emitted when an approval proposal is decided.
	KindApprovalDecided Kind = "approval.decided"
	// KindApprovalExpired is emitted when an approval proposal expires.
	KindApprovalExpired Kind = "approval.expired"
	// KindJournalStep is emitted when a workflow journal step starts, finishes, fails
	// or is scheduled to retry. It carries ids, names and numbers only.
	KindJournalStep Kind = "journal.step"
	// KindJournalWorkflow is emitted when a workflow is started and when it reaches
	// a terminal status (succeeded, failed or cancelled). Ids, names and numbers only.
	KindJournalWorkflow Kind = "journal.workflow"
	// KindControlCommand is emitted when a control command is received.
	KindControlCommand Kind = "control.command"
	// KindPolicyDecision is emitted when the policy engine records a verdict for a tool
	// call (shadow mode: the call itself is decided by the existing checks). It carries
	// ids, names, the mode and the tier only: never a call's arguments or amounts.
	KindPolicyDecision Kind = "policy.decision"
	// KindFeedbackRecorded is emitted when a response of the owner to a proactive message
	// is recorded (pkg/feedback). It carries ids and the kind only, never the value.
	KindFeedbackRecorded Kind = "feedback.recorded"
	// KindFeedbackPosterior is emitted when an owner response changes an acceptance
	// posterior (pkg/feedback). It carries the trigger kind, the hour bucket and the mean
	// rounded to two decimals, nothing else.
	KindFeedbackPosterior Kind = "feedback.posterior"
)

var knownKinds = []Kind{
	KindAgentTurnStart,
	KindAgentTurnEnd,
	KindAgentLLMRequest,
	KindAgentLLMDelta,
	KindAgentLLMResponse,
	KindAgentLLMRetry,
	KindAgentContextCompress,
	KindAgentSessionSummarize,
	KindAgentToolExecStart,
	KindAgentToolExecEnd,
	KindAgentToolExecSkipped,
	KindAgentSteeringInjected,
	KindAgentFollowUpQueued,
	KindAgentInterruptReceived,
	KindAgentSubTurnSpawn,
	KindAgentSubTurnEnd,
	KindAgentSubTurnResultDelivered,
	KindAgentSubTurnOrphan,
	KindAgentError,
	KindChannelLifecycleStarted,
	KindChannelLifecycleInitialized,
	KindChannelLifecycleStartFailed,
	KindChannelLifecycleStopped,
	KindChannelWebhookRegistered,
	KindChannelWebhookUnregistered,
	KindChannelMessageOutboundQueued,
	KindChannelMessageOutboundSent,
	KindChannelMessageOutboundFailed,
	KindChannelRateLimited,
	KindBusPublishFailed,
	KindBusMessageDropped,
	KindBusCloseStarted,
	KindBusCloseCompleted,
	KindBusCloseDrained,
	KindGatewayStart,
	KindGatewayReady,
	KindGatewayShutdown,
	KindGatewayReloadStarted,
	KindGatewayReloadCompleted,
	KindGatewayReloadFailed,
	KindMCPServerConnected,
	KindMCPServerConnecting,
	KindMCPServerFailed,
	KindMCPToolDiscovered,
	KindMCPToolCallStart,
	KindMCPToolCallEnd,

	KindPerceptionMarketTick,
	KindPerceptionMarketCandleClosed,
	KindPerceptionPortfolioSnapshot,
	KindPerceptionFuturesPositions,
	KindPerceptionOrderFilled,
	KindPerceptionOrderPlaced,
	KindPerceptionPlanHealth,
	KindPerceptionPlanDCADue,
	KindPerceptionClockSession,
	KindPerceptionClockDayStart,
	KindPerceptionClockTimer,
	KindPerceptionExternalTrigger,
	KindPerceptionDevice,
	KindPerceptionNewsItem,
	KindPerceptionSourceHealth,
	KindWorldModelDiff,
	KindTriggerCandidate,
	KindAttentionDecision,
	KindNotifyDelivered,
	KindApprovalCreated,
	KindApprovalDecided,
	KindApprovalExpired,
	KindJournalStep,
	KindJournalWorkflow,
	KindControlCommand,
	KindPolicyDecision,
	KindFeedbackRecorded,
	KindFeedbackPosterior,
}

// KnownKinds returns the runtime event kinds declared by this package.
func KnownKinds() []Kind {
	return append([]Kind(nil), knownKinds...)
}
