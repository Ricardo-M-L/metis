package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/handoff"
	"github.com/Ricardo-M-L/metis/internal/jobs"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/processutil"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

const coordinatorEventBodyLimit = 1 << 20

type coordinatorServiceOptions struct {
	runID, workspace, listen, tokenFile string
	agentProfile, verifierProfile       string
	usageReceiptDir, usageCommand       string
	usageBillingConfig                  string
	jobIDs                              coordinatorServiceJobIDs
	workers                             int
	poll, lease                         time.Duration
}

func cmdProjectCoordinatorServe(ctx context.Context, args []string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(args) == 0 {
		return errors.New("usage: metis coordinator serve <runID> --agent PROFILE --token-file PATH [--cwd DIR] [--listen 127.0.0.1:0]")
	}
	fs := flag.NewFlagSet("coordinator serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	opts := coordinatorServiceOptions{runID: args[0]}
	fs.StringVar(&opts.workspace, "cwd", "", "fixed project working directory")
	fs.StringVar(&opts.listen, "listen", "127.0.0.1:0", "loopback IP and port (0 selects a free port)")
	fs.StringVar(&opts.tokenFile, "token-file", "", "private file containing a bearer token of at least 32 characters")
	fs.StringVar(&opts.agentProfile, "agent", "", "required saved profile for new event implementation work")
	fs.StringVar(&opts.verifierProfile, "verify-agent", "", "saved verification profile (default: --agent)")
	fs.StringVar(&opts.usageReceiptDir, "usage-receipts", "", "private host receipt directory for automatic request reconciliation")
	fs.StringVar(&opts.usageCommand, "usage-reconcile-command", "", "private host executable which retrieves final provider usage receipts")
	fs.StringVar(&opts.usageBillingConfig, "usage-billing-config", "", "private native provider billing configuration outside the workspace")
	fs.Var(&opts.jobIDs, "job", "allow automatic structured tasks for this saved job (repeatable)")
	fs.IntVar(&opts.workers, "workers", 1, "maximum concurrent CLI children (1-16)")
	fs.DurationVar(&opts.poll, "poll", 500*time.Millisecond, "durable graph scan interval (minimum 50ms)")
	fs.DurationVar(&opts.lease, "lease", 15*time.Minute, "renewable worker claim duration")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("coordinator serve does not accept extra positional arguments")
	}
	if opts.workers < 1 || opts.workers > 16 || opts.poll < 50*time.Millisecond || opts.lease <= 0 {
		return errors.New("coordinator serve requires workers 1-16, poll >= 50ms and a positive lease")
	}
	if opts.lease < time.Second || opts.lease > 24*time.Hour {
		return errors.New("coordinator serve lease must be between 1s and 24h")
	}
	opts.agentProfile = strings.TrimSpace(opts.agentProfile)
	opts.verifierProfile = strings.TrimSpace(opts.verifierProfile)
	if opts.agentProfile == "" {
		return errors.New("coordinator serve requires --agent PROFILE")
	}
	if opts.verifierProfile == "" {
		opts.verifierProfile = opts.agentProfile
	}
	workspace, err := projectCoordinatorWorkspace(opts.workspace)
	if err != nil {
		return err
	}
	opts.workspace = workspace
	if err := validateCoordinatorServiceListen(opts.listen); err != nil {
		return err
	}
	token, err := readCoordinatorServiceToken(opts.tokenFile)
	if err != nil {
		return err
	}
	// Keep the configured workspace fixed before starting HTTP/pool goroutines.
	// Provider fingerprints include project overlays; per-request chdir would
	// race with admission and is deliberately avoided.
	previousCWD, err := os.Getwd()
	if err != nil {
		return err
	}
	if err = os.Chdir(workspace); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(previousCWD) }()
	for _, name := range []string{opts.agentProfile, opts.verifierProfile} {
		if _, err := rtpkg.LoadAgentProfileForWorkspace(name, workspace); err != nil {
			return fmt.Errorf("coordinator serve profile validation: %w", err)
		}
	}
	store := projectCoordinatorStore(projectcoord.WithProbeContext(ctx))
	run, err := store.Get(workspace, opts.runID)
	if err != nil {
		return err
	}
	if len(run.CheckCommands) == 0 || (run.Status == projectcoord.RunCancelled && len(opts.jobIDs) == 0) {
		return errors.New("coordinator serve requires configured host acceptance checks; cancelled projects require --job for receipt reconciliation")
	}
	executable, err := os.Executable()
	if err != nil {
		return errors.New("coordinator serve cannot locate its CLI executable")
	}
	home, err := filepath.Abs(config.Home())
	if err != nil {
		return errors.New("coordinator serve cannot resolve METIS_HOME")
	}
	listener, err := net.Listen("tcp", opts.listen)
	if err != nil {
		return fmt.Errorf("coordinator serve listen: %w", err)
	}
	service := newCoordinatorService(store, opts, token, executable, home, os.Stderr)
	if err := service.initializeHandoffs(); err != nil {
		_ = listener.Close()
		return err
	}
	reconciler, err := newCoordinatorUsageReconciler(store, workspace, opts.runID, home, opts.usageReceiptDir, opts.usageCommand, func(event coordinatorUsageReconcileEvent) {
		service.logger.emit(coordinatorServiceEvent{Kind: event.Kind, ErrorCode: event.ErrorCode})
	})
	if err != nil {
		_ = listener.Close()
		return err
	}
	if err := reconciler.configureBilling(opts.usageBillingConfig); err != nil {
		_ = listener.Close()
		return err
	}
	serviceCtx, cancelService := context.WithCancel(ctx)
	reconcilerDone := make(chan struct{})
	go func() { defer close(reconcilerDone); _ = reconciler.Run(serviceCtx) }()
	err = service.run(serviceCtx, listener)
	cancelService()
	<-reconcilerDone
	return err
}

func validateCoordinatorServiceListen(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return errors.New("coordinator serve --listen must be a loopback IP and port")
	}
	ip := net.ParseIP(host)
	n, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || n < 0 || n > 65535 {
		return errors.New("coordinator serve accepts only loopback IP addresses and ports 0-65535")
	}
	return nil
}

func readCoordinatorServiceToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("coordinator serve requires --token-file PATH")
	}
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() > 8192 {
		return "", errors.New("coordinator serve token file must be a regular private file of at most 8192 bytes")
	}
	if goruntime.GOOS != "windows" && before.Mode().Perm() != 0600 {
		return "", errors.New("coordinator serve token file must have permissions 0600")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("coordinator serve cannot open token file")
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() ||
		(goruntime.GOOS != "windows" && after.Mode().Perm() != 0600) {
		return "", errors.New("coordinator serve token file changed while being opened")
	}
	data, err := io.ReadAll(io.LimitReader(file, 8193))
	if err != nil || len(data) > 8192 {
		return "", errors.New("coordinator serve cannot read token file")
	}
	token := strings.TrimSpace(string(data))
	if !utf8.ValidString(token) || utf8.RuneCountInString(token) < 32 || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("coordinator serve token must contain at least 32 characters without whitespace")
	}
	return token, nil
}

// The log schema is deliberately a whitelist: child JSON includes private
// lease tokens, prompts and output and must never be forwarded to this stream.
type coordinatorServiceEvent struct {
	At         time.Time `json:"at"`
	Kind       string    `json:"kind"`
	ServiceID  string    `json:"service_id"`
	RunID      string    `json:"run_id"`
	Listen     string    `json:"listen,omitempty"`
	Workers    int       `json:"workers,omitempty"`
	Slot       int       `json:"slot,omitempty"`
	PID        int       `json:"pid,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Outcomes   int       `json:"outcomes,omitempty"`
	ItemID     string    `json:"item_id,omitempty"`
	ErrorCode  string    `json:"error_code,omitempty"`
}

type coordinatorServiceLogger struct {
	mu               sync.Mutex
	writer           io.Writer
	serviceID, runID string
}

func (l *coordinatorServiceLogger) emit(event coordinatorServiceEvent) {
	if l == nil || l.writer == nil {
		return
	}
	event.At, event.ServiceID, event.RunID = time.Now().UTC(), l.serviceID, l.runID
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = json.NewEncoder(l.writer).Encode(event)
}

type coordinatorEventHandler struct {
	store       *projectcoord.Store
	jobBindings map[string]principal.Binding
	opts        coordinatorServiceOptions
	token       string
	closing     atomic.Bool
	wake        func()
	active      func() int
	logger      *coordinatorServiceLogger
}

func newCoordinatorEventHandler(store *projectcoord.Store, opts coordinatorServiceOptions, token string) *coordinatorEventHandler {
	return &coordinatorEventHandler{store: store, opts: opts, token: token}
}

func (h *coordinatorEventHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") || len(provided) != len(h.token) ||
		subtle.ConstantTimeCompare([]byte(provided), []byte(h.token)) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		h.reject(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if strings.EqualFold(r.Header.Get("Sec-Fetch-Site"), "cross-site") || !coordinatorServiceSameOrigin(r) {
		h.reject(w, http.StatusForbidden, "cross_origin_denied")
		return
	}
	if h.closing.Load() {
		h.reject(w, http.StatusServiceUnavailable, "service_stopping")
		return
	}
	switch r.URL.Path {
	case "/v1/events":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			h.reject(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.enqueue(w, r)
	case "/v1/status", "/v1/health":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			h.reject(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		h.status(w)
	default:
		h.reject(w, http.StatusNotFound, "not_found")
	}
}

func coordinatorServiceSameOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Scheme == "http" && strings.EqualFold(u.Host, r.Host) && u.Path == "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (h *coordinatorEventHandler) reject(w http.ResponseWriter, status int, code string) {
	h.logger.emit(coordinatorServiceEvent{Kind: "event_rejected", ErrorCode: code})
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

type coordinatorInboundEvent struct {
	JobID        string   `json:"job_id,omitempty"`
	Source       string   `json:"source"`
	ID           string   `json:"id"`
	Subject      string   `json:"subject"`
	Prompt       string   `json:"prompt"`
	Deliverables []string `json:"deliverables"`
}

func (h *coordinatorEventHandler) enqueue(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, coordinatorEventBodyLimit)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input coordinatorInboundEvent
	if err := decoder.Decode(&input); err != nil {
		h.rejectDecode(w, err)
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		h.rejectDecode(w, err)
		return
	}
	input.Source, input.ID = strings.TrimSpace(input.Source), strings.TrimSpace(input.ID)
	input.Subject, input.Prompt = strings.TrimSpace(input.Subject), strings.TrimSpace(input.Prompt)
	if input.Source == "" || len(input.Source) > 128 || input.ID == "" || len(input.ID) > 256 || input.Subject == "" || input.Prompt == "" {
		h.reject(w, http.StatusBadRequest, "invalid_event")
		return
	}
	run, err := h.store.Get(h.opts.workspace, h.opts.runID)
	if err != nil || len(run.CheckCommands) == 0 {
		h.reject(w, http.StatusServiceUnavailable, "project_unavailable")
		return
	}
	var binding *principal.Binding
	profile := h.opts.agentProfile
	if input.JobID != "" {
		frozen, allowed := h.jobBindings[input.JobID]
		if !allowed {
			h.reject(w, http.StatusForbidden, "job_not_allowed")
			return
		}
		if err := validatePersistentJobRoute(frozen); err != nil {
			h.reject(w, http.StatusConflict, "job_definition_changed")
			return
		}
		current, err := persistentJobStore().Get(input.JobID)
		if err != nil || current.ConfigSHA256 != frozen.ConfigSHA256 {
			h.reject(w, http.StatusConflict, "job_definition_changed")
			return
		}
		binding = &frozen
		profile = frozen.Definition.Profile.Name
	}
	item, err := h.store.EnqueueEvent(h.opts.workspace, h.opts.runID, projectcoord.EventInput{
		Source: input.Source, ID: input.ID, Subject: input.Subject, Prompt: input.Prompt,
		Deliverables: input.Deliverables, PrincipalBinding: binding, AgentProfile: profile, VerifierAgentProfile: h.opts.verifierProfile,
	})
	if err != nil {
		status, code := http.StatusBadRequest, "invalid_event"
		if strings.Contains(err.Error(), "event ID already exists") {
			status, code = http.StatusConflict, "event_conflict"
		} else {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) || strings.Contains(err.Error(), "lock") {
				status, code = http.StatusServiceUnavailable, "storage_unavailable"
			}
		}
		h.reject(w, status, code)
		return
	}
	// The graph was persisted before acknowledgement or dispatch notification.
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"item_id": item.ID, "status": item.Status, "source": input.Source, "event_id": input.ID})
	h.logger.emit(coordinatorServiceEvent{Kind: "event_accepted", ItemID: item.ID})
	if h.wake != nil {
		h.wake()
	}
}

func (h *coordinatorEventHandler) rejectDecode(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		h.reject(w, http.StatusRequestEntityTooLarge, "event_too_large")
	} else {
		h.reject(w, http.StatusBadRequest, "invalid_json")
	}
}

func (h *coordinatorEventHandler) status(w http.ResponseWriter) {
	run, err := h.store.Get(h.opts.workspace, h.opts.runID)
	if err != nil {
		h.reject(w, http.StatusServiceUnavailable, "project_unavailable")
		return
	}
	counts := map[projectcoord.ItemStatus]int{}
	for _, item := range run.Items {
		counts[item.Status]++
	}
	active := 0
	if h.active != nil {
		active = h.active()
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"run_id": run.ID, "status": run.Status, "workers": h.opts.workers,
		"active_children": active, "counts": counts, "spent_usd": run.SpentUSD, "max_budget_usd": run.MaxBudgetUSD,
	})
}

type coordinatorService struct {
	store                 *projectcoord.Store
	handoffStore          *handoff.Store
	jobBindings           map[string]principal.Binding
	jobCursors            map[string]string
	opts                  coordinatorServiceOptions
	handler               *coordinatorEventHandler
	logger                *coordinatorServiceLogger
	executable, home      string
	serviceID             string
	wake                  chan struct{}
	completed             chan *coordinatorServiceChild
	active                atomic.Int32
	command               func(string, ...string) *exec.Cmd
	env                   []string
	shutdownGrace         time.Duration
	children              map[int]*coordinatorServiceChild
	slotAvailable         []time.Time
	slotFailures          []int
	sequence              uint64
	storageAvailableAfter time.Time
	storageFailures       int
}

type coordinatorServiceChild struct {
	cmd            *exec.Cmd
	slot           int
	started        time.Time
	done           chan struct{}
	stdout         *coordinatorServiceBuffer
	stderr         *coordinatorServiceBuffer
	err            error
	exit, outcomes int
	validResponse  bool
}

func newCoordinatorService(store *projectcoord.Store, opts coordinatorServiceOptions, token, executable, home string, writer io.Writer) *coordinatorService {
	id := fmt.Sprintf("serve-%d-%x", os.Getpid(), time.Now().UnixNano())
	s := &coordinatorService{
		store: store, opts: opts, executable: executable, home: home, serviceID: id,
		logger: &coordinatorServiceLogger{writer: writer, serviceID: id, runID: opts.runID},
		wake:   make(chan struct{}, 1), completed: make(chan *coordinatorServiceChild, opts.workers),
		command: exec.Command, env: os.Environ(), shutdownGrace: 5 * time.Second,
		children: make(map[int]*coordinatorServiceChild), slotAvailable: make([]time.Time, opts.workers), slotFailures: make([]int, opts.workers),
	}
	s.handler = newCoordinatorEventHandler(store, opts, token)
	s.handler.logger, s.handler.wake = s.logger, s.nudge
	s.handler.active = func() int { return int(s.active.Load()) }
	return s
}

func (s *coordinatorService) nudge() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *coordinatorService) run(ctx context.Context, listener net.Listener) error {
	defer listener.Close()
	server := &http.Server{Handler: s.handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute}
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(listener) }()
	poolCtx, cancelPool := context.WithCancel(ctx)
	poolDone := make(chan struct{})
	go func() { s.runPool(poolCtx); close(poolDone) }()
	s.logger.emit(coordinatorServiceEvent{Kind: "service_ready", Listen: listener.Addr().String(), Workers: s.opts.workers})
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serverDone:
	}
	s.handler.closing.Store(true)
	cancelPool()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	if err := server.Shutdown(shutdownCtx); err != nil {
		_ = server.Close()
	}
	cancelShutdown()
	<-poolDone
	s.logger.emit(coordinatorServiceEvent{Kind: "service_stopped"})
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return errors.New("coordinator serve listener stopped unexpectedly")
	}
	return nil
}

func (s *coordinatorService) runPool(ctx context.Context) {
	ticker := time.NewTicker(s.opts.poll)
	defer ticker.Stop()
	s.dispatch(ctx, false)
	for {
		select {
		case <-ctx.Done():
			s.stopChildren()
			return
		case child := <-s.completed:
			delete(s.children, child.slot)
			s.active.Add(-1)
			s.observeChild(child)
			s.dispatch(ctx, false)
		case <-s.wake:
			// A newly persisted event may use an otherwise idle slot immediately.
			for i := range s.slotAvailable {
				if s.slotFailures[i] == 0 {
					s.slotAvailable[i] = time.Time{}
				}
			}
			s.dispatch(ctx, true)
		case <-ticker.C:
			s.dispatch(ctx, false)
		}
	}
}

func (s *coordinatorService) dispatch(ctx context.Context, eventWake bool) {
	if ctx.Err() != nil {
		return
	}
	if err := s.reconcileHandoffs(ctx); err != nil {
		s.logger.emit(coordinatorServiceEvent{Kind: "handoff_reconcile_deferred", ErrorCode: "handoff_storage_or_contract_unavailable"})
	}
	if err := reconcileCoordinatorLearning(ctx, s.store, s.opts.workspace, s.opts.runID); err != nil {
		s.logger.emit(coordinatorServiceEvent{Kind: "learning_feedback_deferred", ErrorCode: "learning_evidence_unavailable"})
	}
	if len(s.children) >= s.opts.workers || (!eventWake && time.Now().Before(s.storageAvailableAfter)) {
		return
	}
	run, err := s.store.Get(s.opts.workspace, s.opts.runID)
	if err != nil {
		s.storageFailures++
		s.storageAvailableAfter = time.Now().Add(coordinatorServiceBackoff(s.opts.poll, s.storageFailures))
		s.logger.emit(coordinatorServiceEvent{Kind: "dispatch_backoff", ErrorCode: "storage_unavailable"})
		return
	}
	s.storageFailures, s.storageAvailableAfter = 0, time.Time{}
	ready := 0
	for _, item := range run.Items {
		if item.Status == projectcoord.ItemReady {
			ready++
		}
	}
	for slot := 0; slot < s.opts.workers && ready > 0 && ctx.Err() == nil; slot++ {
		if s.children[slot] != nil || time.Now().Before(s.slotAvailable[slot]) {
			continue
		}
		if err := s.startChild(slot); err != nil {
			s.slotFailures[slot]++
			s.slotAvailable[slot] = time.Now().Add(coordinatorServiceBackoff(s.opts.poll, s.slotFailures[slot]))
			s.logger.emit(coordinatorServiceEvent{Kind: "dispatch_backoff", Slot: slot + 1, ErrorCode: "worker_start_failed"})
			continue
		}
		ready--
	}
}

func (s *coordinatorService) startChild(slot int) error {
	s.sequence++
	worker := fmt.Sprintf("%s-%d-%d", s.serviceID, slot+1, s.sequence)
	cmd := s.command(s.executable, "coordinator", "run", s.opts.runID, "--cwd", s.opts.workspace, "--worker", worker, "--lease", s.opts.lease.String(), "--max-items", "1", "--summary-json")
	cmd.Dir = s.opts.workspace
	cmd.Env = coordinatorServiceWorkerEnv(s.env, s.home, s.handler.token)
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
		cmd.Env = append(cmd.Env, coordinatorParentEnvironment()...)
	}
	jobs.ApplyProcessGroup(cmd)
	cmd.WaitDelay = time.Second
	child := &coordinatorServiceChild{cmd: cmd, slot: slot, started: time.Now(), done: make(chan struct{}), stdout: &coordinatorServiceBuffer{limit: 1 << 20}, stderr: &coordinatorServiceBuffer{limit: 64 << 10}}
	cmd.Stdout, cmd.Stderr = child.stdout, child.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	s.children[slot] = child
	s.active.Add(1)
	s.logger.emit(coordinatorServiceEvent{Kind: "worker_started", Slot: slot + 1, PID: cmd.Process.Pid})
	go func() {
		child.err = cmd.Wait()
		jobs.KillProcessGroup(cmd.Process)
		child.exit = cmd.ProcessState.ExitCode()
		var result struct {
			OutcomesCount *int   `json:"outcomes_count"`
			RunID         string `json:"run_id"`
		}
		if !child.stdout.truncated && json.Unmarshal(child.stdout.data(), &result) == nil && result.OutcomesCount != nil && *result.OutcomesCount >= 0 && *result.OutcomesCount <= 1 && result.RunID == s.opts.runID {
			child.validResponse, child.outcomes = true, *result.OutcomesCount
		}
		close(child.done)
		s.completed <- child
	}()
	return nil
}

func (s *coordinatorService) observeChild(child *coordinatorServiceChild) {
	event := coordinatorServiceEvent{Kind: "worker_finished", Slot: child.slot + 1, PID: child.cmd.Process.Pid, ExitCode: &child.exit, DurationMS: time.Since(child.started).Milliseconds(), Outcomes: child.outcomes}
	failure := child.exit != 0 && child.exit != 11 && child.exit != 130
	if child.exit == 0 && child.validResponse && child.outcomes == 0 {
		event.Kind = "worker_idle"
	} else if child.exit == 11 {
		event.ErrorCode = "project_incomplete"
	} else if child.exit == 130 {
		event.ErrorCode = "worker_interrupted"
	} else if child.exit != 0 {
		event.ErrorCode = "worker_exit_failed"
	} else if !child.validResponse {
		event.ErrorCode, failure = "worker_response_invalid", true
	}
	if failure {
		s.slotFailures[child.slot]++
	} else {
		s.slotFailures[child.slot] = 0
	}
	s.slotAvailable[child.slot] = time.Now().Add(coordinatorServiceBackoff(s.opts.poll, s.slotFailures[child.slot]))
	s.logger.emit(event)
}

func (s *coordinatorService) stopChildren() {
	var group sync.WaitGroup
	for _, child := range s.children {
		_ = processutil.Terminate(child.cmd.Process.Pid)
		group.Add(1)
		go func(child *coordinatorServiceChild) {
			defer group.Done()
			timer := time.NewTimer(s.shutdownGrace)
			defer timer.Stop()
			select {
			case <-child.done:
			case <-timer.C:
				jobs.KillProcessGroup(child.cmd.Process)
				<-child.done
			}
		}(child)
	}
	group.Wait()
	for _, child := range s.children {
		s.observeChild(child)
	}
	clear(s.children)
	s.active.Store(0)
}

func coordinatorServiceBackoff(poll time.Duration, failures int) time.Duration {
	if failures > 6 {
		failures = 6
	}
	delay := poll * time.Duration(1<<failures)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func coordinatorServiceWorkerEnv(parent []string, home, token string) []string {
	env := make([]string, 0, len(parent)+1)
	for _, entry := range parent {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "METIS_HOME" || key == "METIS_DESKTOP_FRAME_TOKEN" || key == coordinatorParentPIDEnv || key == coordinatorParentIdentityEnv ||
			(token != "" && strings.Contains(value, token)) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "METIS_HOME="+home)
}

type coordinatorServiceBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

func (b *coordinatorServiceBuffer) Write(data []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.limit - b.buffer.Len()
	if remaining < len(data) {
		b.truncated = true
		if remaining > 0 {
			_, _ = b.buffer.Write(data[:remaining])
		}
	} else {
		_, _ = b.buffer.Write(data)
	}
	return len(data), nil
}

func (b *coordinatorServiceBuffer) data() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}
