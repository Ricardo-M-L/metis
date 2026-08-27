package runtime

import "context"

// RunWithTraceTurn pins every event emitted by run to sessionID until run has
// completely unwound. In particular, cleanup events emitted by Loop.Run
// defers after EventError/LoopDone must retain the terminal turn's immutable
// origin instead of falling back to whichever session is currently selected.
//
// A missing trace adapter or session ID leaves ctx untouched and skips the
// lifecycle end signal. That matters when callers are embedded under another
// traced operation: ending an unchanged context could otherwise release the
// parent's invocation by mistake.
func RunWithTraceTurn(ctx context.Context, sessionID string, run func(context.Context) error) error {
	boundCtx, origin := BindTraceTurn(ctx, sessionID)
	if origin.InvocationID == "" {
		return run(boundCtx)
	}
	defer EndTraceTurn(boundCtx)
	return run(boundCtx)
}
