package runtime

import (
	"encoding/json"
	"log"
	"time"

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/session"
)

const maxTraceRequestContextBytes = 2 * 1024 * 1024

// recordRequestContextLocked runs once per session's first observed root
// request. In a resumed legacy session this can be a later turn, which stays
// explicit in the snapshot. Header.System is never substituted as evidence.
func (a *TraceAdapter) recordRequestContextLocked(origin traceOrigin, ev agent.Event) {
	if ev.RequestContext == nil || origin.subAgent != "" || ev.SubAgentParentID != "" || a.requestContexts[origin.sessionID] {
		return
	}
	for _, existing := range a.store.Events(origin.sessionID) {
		if existing.Kind == "request_context" && existing.RequestContext != nil && existing.SubAgentOf == "" && existing.ParentID == "" && existing.TraceParentInvocationID == "" {
			a.requestContexts[origin.sessionID] = true
			return
		}
	}
	req := ev.RequestContext
	context := &session.TraceRequestContext{
		Available: true, Source: "logical-request", CapturedAt: time.Now(), Turn: origin.turn,
		System: security.RedactSubprocessText(req.System),
		Model:  security.RedactSubprocessText(req.Model), Provider: security.RedactSubprocessText(req.Provider),
		Tools: []session.TraceRequestTool{},
	}
	for _, section := range req.SystemSections {
		context.SystemSections = append(context.SystemSections, session.TraceSystemSection{
			Name: security.RedactSubprocessText(section.Name), Body: security.RedactSubprocessText(section.Body),
			Cache: section.Cache, Volatile: section.Volatile,
		})
	}
	for _, tool := range req.Tools {
		// Serialize only the schema, never the complete llm.Request. Redact
		// before decoding a fresh map so caller-owned request maps stay intact.
		raw, err := json.Marshal(tool.InputSchema)
		var schema map[string]any
		if err == nil {
			err = json.Unmarshal([]byte(security.RedactSubprocessText(string(raw))), &schema)
		}
		if err != nil {
			context.Truncated = true
			context.Limitation = "One or more tool schemas could not be recorded."
		}
		context.Tools = append(context.Tools, session.TraceRequestTool{
			Name: security.RedactSubprocessText(tool.Name), Description: security.RedactSubprocessText(tool.Description), InputSchema: schema,
		})
	}
	boundTraceRequestContext(context)
	row := &session.TraceEvent{SessionID: origin.sessionID, Turn: origin.turn, Kind: "request_context",
		TS: context.CapturedAt, TraceInvocationID: origin.invocationID, RequestContext: context,
		Source: "logical-request"}
	if err := a.store.Append(row); err != nil {
		log.Printf("trace: append request context: %v", err)
		return
	}
	a.requestContexts[origin.sessionID] = true
	// Make the snapshot visible before generation starts, including workers
	// read by a parent process. No disk fsync is added to the request path.
	if err := a.store.FlushSession(origin.sessionID); err != nil {
		log.Printf("trace: publish request context: %v", err)
	}
}

func boundTraceRequestContext(context *session.TraceRequestContext) {
	raw, err := json.Marshal(context)
	if err == nil && len(raw) <= maxTraceRequestContextBytes {
		return
	}
	context.Truncated = true
	context.Limitation = "Snapshot exceeds the 2 MiB storage limit; system and/or tool definitions are incomplete."
	context.System = traceUTF8Prefix(context.System, 512*1024)
	context.SystemSections = nil
	tools := context.Tools
	context.Tools = []session.TraceRequestTool{}
	for _, tool := range tools {
		context.Tools = append(context.Tools, tool)
		raw, err = json.Marshal(context)
		if err != nil || len(raw) > maxTraceRequestContextBytes {
			context.Tools = context.Tools[:len(context.Tools)-1]
			break
		}
	}
	// JSON escaping can expand a 512 KiB control-character system beyond the
	// byte budget. Never create rows larger than the reader's scanner limit.
	for {
		raw, err = json.Marshal(context)
		if err == nil && len(raw) <= maxTraceRequestContextBytes {
			break
		}
		context.System = traceUTF8Prefix(context.System, len(context.System)/2)
	}
}
