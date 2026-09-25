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

	"github.com/Ricardo-M-L/metis/internal/agent"
	"github.com/Ricardo-M-L/metis/internal/desktopipc"
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
	OnEvent   func(agent.Event)
	OnStatus  func(desktopipc.Status)
	Steer     <-chan IsolatedSteerRequest
}

type IsolatedTurnResult struct {
	Text    string
	Stopped bool
	Done    *agent.Event
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
	Executable  string
	MaxParallel int
	// MaxConfigurableParallelism is the highest user-selectable foreground
	// worker count. Zero keeps reduced embedders read-only.
	MaxConfigurableParallelism int
	// MaxTotalAgentSlots keeps root and child workers under one Desktop budget.
	MaxTotalAgentSlots  int
	SubagentSlotDir     string
	MaxSubagentSlots    int
	MaxSubagentsPerRoot int
}

type processIsolatedTurnRunner struct {
	executable          string
	subagentSlotDir     string
	limitsMu            sync.RWMutex
	maxSubagentSlots    int
	maxSubagentsPerRoot int
	command             func(string, ...string) *exec.Cmd
}

// SetMaxSubagentSlots updates the permit count inherited by subsequently
// launched root workers. Existing workers keep the environment they started
// with, which is why Server only resizes while no isolated turn is active.
func (r *processIsolatedTurnRunner) SetMaxSubagentSlots(slots int) {
	if r == nil || slots < 1 {
		return
	}
	r.limitsMu.Lock()
	r.maxSubagentSlots = slots
	r.limitsMu.Unlock()
}

func (r *processIsolatedTurnRunner) subagentLimits() (int, int) {
	if r == nil {
		return 0, 0
	}
	r.limitsMu.RLock()
	defer r.limitsMu.RUnlock()
	return r.maxSubagentSlots, r.maxSubagentsPerRoot
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

// Eligible accepts every recognized permission mode. Interactive decisions
// remain owned by the child's permission gate and are relayed by the private
// bidirectional worker protocol instead of bypassing approval in the parent.
func (r *processIsolatedTurnRunner) Eligible(header *session.Header) bool {
	if r == nil || header == nil {
		return false
	}
	_, ok := permission.ParseMode(header.Mode)
	return ok
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
	cmd := r.command(r.executable, "run", "--resume", request.SessionID, "--desktop-worker", "--streamlined", "--", request.Input)
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
		maxSubagentSlots, maxSubagentsPerRoot := r.subagentLimits()
		env["METIS_DESKTOP_SUBAGENT_SLOT_DIR"] = r.subagentSlotDir
		env["METIS_DESKTOP_SUBAGENT_SLOTS"] = strconv.Itoa(maxSubagentSlots)
		env["METIS_DESKTOP_SUBAGENT_CAP"] = strconv.Itoa(maxSubagentsPerRoot)
	}
	cmd.Env = withIsolatedWorkerEnv(os.Environ(), env)
	jobs.ApplyProcessGroup(cmd)
	cmd.WaitDelay = workerWaitDelay
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return IsolatedTurnResult{}, fmt.Errorf("open isolated turn output: %w", err)
	}
	defer stdout.Close()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return IsolatedTurnResult{}, fmt.Errorf("open isolated turn input: %w", err)
	}
	defer stdin.Close()
	stderr := &automationLogBuffer{limit: isolatedTurnStderrLimit}
	cmd.Stderr = stderr
	if err := ctx.Err(); err != nil {
		return IsolatedTurnResult{Stopped: true}, err
	}
	if err := cmd.Start(); err != nil {
		return IsolatedTurnResult{}, fmt.Errorf("start isolated turn worker: %w", err)
	}

	bridgeCtx, cancelBridge := context.WithCancel(ctx)
	defer cancelBridge()
	replyErrors := make(chan error, 1)
	var replyWorkers sync.WaitGroup
	replies := desktopipc.NewEncoder(stdin)
	controls := newIsolatedWorkerControls(bridgeCtx, replies, request.Steer, replyErrors)
	streamDone := make(chan isolatedWorkerOutput, 1)
	go func() {
		streamDone <- readIsolatedWorkerOutput(bridgeCtx, stdout, replies, request, &replyWorkers, replyErrors, controls)
	}()

	var output isolatedWorkerOutput
	var failure error
	streamRead := false
	select {
	case output = <-streamDone:
		streamRead = true
		failure = output.err
	case failure = <-replyErrors:
	case <-ctx.Done():
		failure = ctx.Err()
	}

	// In the successful path all stdout must be decoded before Wait closes
	// StdoutPipe. Otherwise a fast-exiting worker can lose its final events.
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	var waitErr error
	if failure == nil {
		select {
		case waitErr = <-waitDone:
		case failure = <-replyErrors:
		case <-ctx.Done():
			failure = ctx.Err()
		}
	}
	if failure != nil {
		cancelBridge()
		_ = stdin.Close()
		_ = stdout.Close()
		terminateIsolatedTurnProcess(cmd)
		grace := time.NewTimer(workerTerminateGrace)
		select {
		case waitErr = <-waitDone:
			if !grace.Stop() {
				<-grace.C
			}
		case <-grace.C:
			jobs.KillProcessGroup(cmd.Process)
			waitErr = <-waitDone
		}
		// Even a promptly-exiting leader may leave tool descendants behind.
		jobs.KillProcessGroup(cmd.Process)
	}
	if !streamRead {
		output = <-streamDone
	}
	cancelBridge()
	_ = stdin.Close()
	replyWorkers.Wait()
	<-controls.done
	result := IsolatedTurnResult{Text: output.text, Stopped: ctx.Err() != nil}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if failure == nil {
		failure = waitErr
	}
	if failure == nil {
		select {
		case failure = <-replyErrors:
		default:
		}
	}
	if failure != nil {
		detail := strings.TrimSpace(security.RedactSubprocessText(stderr.String()))
		if detail != "" {
			return result, fmt.Errorf("isolated turn worker: %w: %s", failure, truncateRunError(detail, 1200))
		}
		return result, fmt.Errorf("isolated turn worker: %w", failure)
	}
	result.Done = output.done
	return result, nil
}

type isolatedWorkerOutput struct {
	text string
	done *agent.Event
	err  error
}

func readIsolatedWorkerOutput(ctx context.Context, reader io.Reader, replies *desktopipc.Encoder, request IsolatedTurnRequest, workers *sync.WaitGroup, failures chan<- error, controls *isolatedWorkerControls) (result isolatedWorkerOutput) {
	decoder := desktopipc.NewDecoder(reader)
	var text strings.Builder
	defer func() { result.text = text.String() }()
	seenRequests := make(map[string]struct{})
	for {
		message, err := decoder.Decode()
		if errors.Is(err, io.EOF) {
			if result.done == nil {
				result.err = errors.New("desktop worker output closed before loop completion")
			}
			return result
		}
		if err != nil {
			result.err = fmt.Errorf("decode desktop worker output: %w", err)
			return result
		}
		if message.Type == desktopipc.TypeSteerResult {
			if err := controls.acknowledge(message); err != nil {
				result.err = err
				return result
			}
			continue
		}
		if message.Type == desktopipc.TypeStatus {
			if request.OnStatus != nil {
				request.OnStatus(*message.Status)
			}
			continue
		}
		if message.Type != desktopipc.TypeEvent {
			result.err = errors.New("desktop worker output expected an event or status")
			return result
		}
		event := message.Event.ToEvent()
		if event.Kind < agent.EventTextDelta || event.Kind > agent.EventContextInjected {
			result.err = errors.New("desktop worker output has an unknown event kind")
			return result
		}
		if event.Kind == agent.EventLoopDone {
			if result.done != nil {
				result.err = errors.New("desktop worker output repeated loop completion")
				return result
			}
			result.done = &event
			continue
		}
		if event.Kind == agent.EventPermissionRequest || event.Kind == agent.EventAskUser {
			if message.ID == "" || request.OnEvent == nil {
				result.err = errors.New("desktop worker interaction requires a request ID and event handler")
				return result
			}
			if _, exists := seenRequests[message.ID]; exists {
				result.err = errors.New("desktop worker interaction reused a request ID")
				return result
			}
			seenRequests[message.ID] = struct{}{}
			if event.Kind == agent.EventPermissionRequest {
				event.PermissionReply = make(chan agent.PermissionDecision, 1)
			} else {
				event.AskUserReply = make(chan string, 1)
			}
			workers.Add(1)
			go func(id string, event agent.Event) {
				defer workers.Done()
				reply := desktopipc.Message{Version: desktopipc.Version, Type: desktopipc.TypeReply, ID: id, Decision: agent.PermissionDecisionDeny}
				select {
				case decision, ok := <-event.PermissionReply:
					if !ok {
						reportIsolatedReplyFailure(failures, errors.New("desktop worker permission reply channel closed"))
						return
					}
					reply.Decision = decision
				case answer, ok := <-event.AskUserReply:
					if !ok {
						reportIsolatedReplyFailure(failures, errors.New("desktop worker answer channel closed"))
						return
					}
					reply.Answer = answer
				case <-ctx.Done():
					return
				}
				if ctx.Err() != nil {
					return
				}
				if err := replies.Encode(reply); err != nil {
					reportIsolatedReplyFailure(failures, fmt.Errorf("write desktop worker reply: %w", err))
				}
			}(message.ID, event)
		}
		if event.Kind == agent.EventTextDelta {
			text.WriteString(event.TextDelta)
		}
		if request.OnEvent != nil {
			request.OnEvent(event)
		} else if event.Kind == agent.EventTextDelta && request.OnText != nil {
			request.OnText(event.TextDelta)
		}
	}
}

func reportIsolatedReplyFailure(failures chan<- error, err error) {
	select {
	case failures <- err:
	default:
	}
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
