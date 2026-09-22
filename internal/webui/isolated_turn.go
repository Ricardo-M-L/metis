package webui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/Ricardo-M-L/metis/internal/security"
	"github.com/Ricardo-M-L/metis/internal/session"
)

// IsolatedTurnRequest is the immutable boundary between the Desktop server
// and a per-turn CLI worker. The child owns its provider, gate, tool registry,
// memory binding and process-global runtime routers, so concurrent sessions
// cannot rebind one another's execution environment.
type IsolatedTurnRequest struct {
	SessionID string
	WorkDir   string
	Input     string
	OnText    func(string)
}

type IsolatedTurnResult struct {
	Text    string
	Stopped bool
}

// IsolatedTurnRunner runs a top-level turn outside the WebUI process. It is an
// interface so server tests can prove admission and cancellation behavior
// without starting a real provider or native binary.
type IsolatedTurnRunner interface {
	Eligible(*session.Header) bool
	Run(context.Context, IsolatedTurnRequest) (IsolatedTurnResult, error)
}

// IsolatedTurnOptions configures the Desktop high-performance worker pool.
// Executable must be the absolute path of the currently-running metis binary.
type IsolatedTurnOptions struct {
	Executable          string
	MaxParallel         int
	SubagentSlotDir     string
	MaxSubagentSlots    int
	MaxSubagentsPerRoot int
}

type processIsolatedTurnRunner struct {
	executable          string
	subagentSlotDir     string
	maxSubagentSlots    int
	maxSubagentsPerRoot int
	command             func(string, ...string) *exec.Cmd
}

func newProcessIsolatedTurnRunner(options IsolatedTurnOptions) (IsolatedTurnRunner, error) {
	if strings.TrimSpace(options.Executable) == "" {
		return nil, errors.New("isolated turn worker requires an executable")
	}
	if options.SubagentSlotDir != "" && (options.MaxSubagentSlots < 1 || options.MaxSubagentsPerRoot < 1) {
		return nil, errors.New("isolated turn worker requires positive sub-agent limits")
	}
	return &processIsolatedTurnRunner{
		executable: options.Executable, subagentSlotDir: options.SubagentSlotDir, maxSubagentSlots: options.MaxSubagentSlots,
		maxSubagentsPerRoot: options.MaxSubagentsPerRoot, command: exec.Command,
	}, nil
}

// Eligible deliberately excludes approval-driven modes. The normal in-process
// loop is retained for those sessions so its existing permission and AskUser
// cards continue to work. dontAsk has no interactive approval path, while
// bypassPermissions and fullAccess were explicitly chosen to execute without
// a browser decision and can safely move to an isolated worker.
func (r *processIsolatedTurnRunner) Eligible(header *session.Header) bool {
	if r == nil || header == nil {
		return false
	}
	mode, ok := permission.ParseMode(header.Mode)
	if !ok {
		return false
	}
	switch mode {
	case permission.ModeDontAsk, permission.ModeBypassPermissions, permission.ModeFullAccess:
		return true
	default:
		return false
	}
}

func (r *processIsolatedTurnRunner) Run(ctx context.Context, request IsolatedTurnRequest) (IsolatedTurnResult, error) {
	if r == nil || strings.TrimSpace(r.executable) == "" {
		return IsolatedTurnResult{}, errors.New("isolated turn worker is unavailable")
	}
	if request.SessionID == "" || request.WorkDir == "" {
		return IsolatedTurnResult{}, errors.New("isolated turn worker received an incomplete session")
	}
	// `run --resume` uses the session's durable provider/model/system/mode and
	// persists the user and assistant messages itself. The parent only relays
	// the streaming presentation events and owns Desktop-level scheduling.
	cmd := r.command(r.executable, "run", "--resume", request.SessionID, "--streamlined", "--", request.Input)
	cmd.Dir = request.WorkDir
	// A worker's parent request must be able to return as soon as its answer is
	// durable. Auto-memory's secondary model call is deliberately kept out of
	// this critical path; the existing durable execution-memory recorder still
	// captures tool/environment failures for later recovery.
	env := map[string]string{
		"METIS_AUTO_MEMORY":             "0",
		"METIS_DESKTOP_ISOLATED_WORKER": "1",
	}
	if r.subagentSlotDir != "" {
		env["METIS_DESKTOP_SUBAGENT_SLOT_DIR"] = r.subagentSlotDir
		env["METIS_DESKTOP_SUBAGENT_SLOTS"] = strconv.Itoa(r.maxSubagentSlots)
		env["METIS_DESKTOP_SUBAGENT_CAP"] = strconv.Itoa(r.maxSubagentsPerRoot)
	}
	cmd.Env = withIsolatedWorkerEnv(os.Environ(), env)
	jobs.ApplyProcessGroup(cmd)
	cmd.WaitDelay = workerWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return IsolatedTurnResult{}, fmt.Errorf("open isolated turn output: %w", err)
	}
	stderr := &automationLogBuffer{limit: isolatedTurnStderrLimit}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return IsolatedTurnResult{}, fmt.Errorf("start isolated turn worker: %w", err)
	}

	stream := &isolatedTextStream{onText: request.OnText}
	streamDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stream, stdout)
		stream.flush()
		close(streamDone)
	}()
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-ctx.Done():
		terminateIsolatedTurnProcess(cmd)
		grace := time.NewTimer(workerTerminateGrace)
		select {
		case waitErr = <-waitDone:
			if !grace.Stop() {
				<-grace.C
			}
		case <-grace.C:
			// The child may have already forked Bash/sub-agent descendants.
			// They share the process group installed before Start, so this is
			// the bounded fallback after cooperative SIGTERM has had a chance.
			if cmd.Process != nil {
				jobs.KillProcessGroup(cmd.Process)
			}
			waitErr = <-waitDone
		}
		<-streamDone
		return IsolatedTurnResult{Text: stream.text(), Stopped: true}, ctx.Err()
	}
	<-streamDone
	if waitErr != nil {
		detail := strings.TrimSpace(security.RedactSubprocessText(stderr.String()))
		if detail != "" {
			detail = truncateRunError(detail, 1200)
			return IsolatedTurnResult{Text: stream.text()}, fmt.Errorf("isolated turn worker: %w: %s", waitErr, detail)
		}
		return IsolatedTurnResult{Text: stream.text()}, fmt.Errorf("isolated turn worker: %w", waitErr)
	}
	return IsolatedTurnResult{Text: stream.text()}, nil
}

const (
	workerWaitDelay         = 2 * time.Second
	workerTerminateGrace    = 2 * time.Second
	isolatedTurnStderrLimit = 32 << 10
)

func terminateIsolatedTurnProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = processutil.Terminate(cmd.Process.Pid)
}

type isolatedTextStream struct {
	mu      sync.Mutex
	pending []byte
	textBuf strings.Builder
	onText  func(string)
}

func (s *isolatedTextStream) Write(data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = append(s.pending, data...)
	s.emitCompleteLocked(false)
	return len(data), nil
}

func (s *isolatedTextStream) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emitCompleteLocked(true)
}

func (s *isolatedTextStream) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.textBuf.String()
}

func (s *isolatedTextStream) emitCompleteLocked(final bool) {
	if len(s.pending) == 0 {
		return
	}
	n := len(s.pending)
	if !utf8.Valid(s.pending) && !final {
		// io.Copy may split a UTF-8 code point. Preserve at most three trailing
		// bytes until the next write so browser SSE is always valid Unicode.
		for n > 0 && !utf8.Valid(s.pending[:n]) {
			n--
		}
	}
	if n == 0 && !final {
		return
	}
	part := string(s.pending[:n])
	s.pending = append(s.pending[:0], s.pending[n:]...)
	if final && len(s.pending) > 0 {
		part += string(s.pending)
		s.pending = s.pending[:0]
	}
	if part == "" {
		return
	}
	s.textBuf.WriteString(part)
	if s.onText != nil {
		s.onText(part)
	}
}

func truncateRunError(text string, limit int) string {
	if len([]rune(text)) <= limit {
		return text
	}
	return string([]rune(text)[:limit]) + "…"
}

func withIsolatedWorkerEnv(base []string, updates map[string]string) []string {
	result := make([]string, 0, len(base)+len(updates))
	for _, entry := range base {
		key, _, found := strings.Cut(entry, "=")
		if found {
			if _, replaced := updates[key]; replaced {
				continue
			}
		}
		result = append(result, entry)
	}
	for key, value := range updates {
		result = append(result, key+"="+value)
	}
	return result
}
