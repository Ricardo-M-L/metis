package agent

// HasPendingSubAgentNotifications is a non-consuming readiness check for a
// private Desktop worker between Runs. The worker must serialize Run ownership
// and must not call it concurrently with ResetSession replacing the channel.
func (l *Loop) HasPendingSubAgentNotifications() bool {
	return l != nil && len(l.subAgentNotify) > 0
}
