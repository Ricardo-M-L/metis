package agent

import (
	"context"
	"fmt"

	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

// ImageAttachmentReader reads only images referenced by the requested session.
// The runtime supplies its actual session store, independently of trace/UI state.
type ImageAttachmentReader interface {
	ReadImageAttachment(sessionID, attachmentID string) ([]byte, error)
}

type imageAttachmentSessionKey struct{}

type imageAttachmentSession struct {
	reader    ImageAttachmentReader
	sessionID string
	rootCall  string
}

// WithImageAttachmentSession freezes attachment ownership for one invocation.
// Children inherit this binding, including when the UI selects another session.
func WithImageAttachmentSession(ctx context.Context, reader ImageAttachmentReader, sessionID string) context.Context {
	return context.WithValue(ctx, imageAttachmentSessionKey{}, imageAttachmentSession{reader: reader, sessionID: sessionID})
}

// WithImageAttachmentRootCall pins the root tool occurrence before entering a
// child. Nested tools keep that root owner so branch cutoffs can locate the
// parent receipt even though child messages live in a separate transcript.
func WithImageAttachmentRootCall(ctx context.Context) context.Context {
	bound, ok := ctx.Value(imageAttachmentSessionKey{}).(imageAttachmentSession)
	if !ok || bound.rootCall != "" {
		return ctx
	}
	bound.rootCall = tools.InvocationIDFromContext(ctx)
	return context.WithValue(ctx, imageAttachmentSessionKey{}, bound)
}

// HasImageAttachmentSession is a cheap permission preflight; authorization and
// checksum validation happen in the reader when the tool actually executes.
func HasImageAttachmentSession(ctx context.Context) bool {
	bound, ok := ctx.Value(imageAttachmentSessionKey{}).(imageAttachmentSession)
	return ok && bound.reader != nil && bound.sessionID != ""
}

func ReadImageAttachment(ctx context.Context, attachmentID string) ([]byte, error) {
	bound, ok := ctx.Value(imageAttachmentSessionKey{}).(imageAttachmentSession)
	if !ok || bound.reader == nil || bound.sessionID == "" {
		return nil, fmt.Errorf("ViewImage: no session attachment store is bound to this invocation")
	}
	return bound.reader.ReadImageAttachment(bound.sessionID, attachmentID)
}

func recordInvocationImageAttachments(ctx context.Context, blocks []llm.ContentBlock) error {
	bound, ok := ctx.Value(imageAttachmentSessionKey{}).(imageAttachmentSession)
	if !ok || bound.reader == nil || bound.sessionID == "" {
		return nil
	}
	writer, ok := bound.reader.(interface {
		RecordImageAttachments(sessionID string, blocks []llm.ContentBlock) error
	})
	if !ok {
		return nil // third-party read-only attachment readers remain compatible
	}
	var annotate func([]llm.ContentBlock, string) []llm.ContentBlock
	annotate = func(parts []llm.ContentBlock, call string) []llm.ContentBlock {
		out := append([]llm.ContentBlock(nil), parts...)
		for i := range out {
			owner := call
			if owner == "" && out[i].Type == "tool_result" {
				owner = out[i].TraceCallID
			}
			if out[i].Type == "image" {
				out[i].TraceCallID = owner
			}
			out[i].ToolResultBlocks = annotate(out[i].ToolResultBlocks, owner)
		}
		return out
	}
	return writer.RecordImageAttachments(bound.sessionID, annotate(blocks, bound.rootCall))
}
