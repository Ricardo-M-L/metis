package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

const coordinatorSupervisorSchema = 1

type coordinatorSupervisorManifest struct {
	Schema             int       `json:"schema"`
	ID                 string    `json:"id"`
	Label              string    `json:"label"`
	Domain             string    `json:"domain"`
	CreatedAt          time.Time `json:"created_at"`
	Directory          string    `json:"directory"`
	Plist              string    `json:"plist"`
	PlistSHA256        string    `json:"plist_sha256"`
	Executable         string    `json:"executable"`
	BinarySHA256       string    `json:"binary_sha256"`
	RunID              string    `json:"run_id"`
	Workspace          string    `json:"workspace"`
	MetisHome          string    `json:"metis_home"`
	UserHome           string    `json:"user_home"`
	ToolPath           string    `json:"tool_path"`
	UsageReceiptDir    string    `json:"usage_receipt_dir"`
	UsageCommand       string    `json:"usage_reconcile_command,omitempty"`
	UsageBillingConfig string    `json:"usage_billing_config,omitempty"`
	Listen             string    `json:"listen"`
	JobIDs             []string  `json:"job_ids,omitempty"`
}

type coordinatorSupervisorStatus struct {
	ID             string                         `json:"id"`
	Installed      bool                           `json:"installed"`
	Loaded         bool                           `json:"loaded"`
	State          string                         `json:"state,omitempty"`
	PID            int                            `json:"pid,omitempty"`
	Runs           int                            `json:"launches,omitempty"`
	LastExitCode   *int                           `json:"last_exit_code,omitempty"`
	BinaryVerified bool                           `json:"binary_verified"`
	Manifest       *coordinatorSupervisorManifest `json:"manifest,omitempty"`
}

type coordinatorSupervisor struct {
	platform, userHome, metisHome, executable, toolPath string
	uid                                                 int
	run                                                 func(context.Context, ...string) ([]byte, error)
}

func newCoordinatorSupervisor() (*coordinatorSupervisor, error) {
	if goruntime.GOOS != "darwin" {
		return nil, errors.New("coordinator service host supervision currently supports macOS user LaunchAgents only")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	home, err = supervisorCanonicalDirectory(home)
	if err != nil {
		return nil, err
	}
	metisHome, err := supervisorCanonicalDirectory(config.Home())
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	toolPath, err := coordinatorSupervisorToolPath(os.Getenv("PATH"))
	if err != nil {
		return nil, err
	}
	return &coordinatorSupervisor{platform: goruntime.GOOS, userHome: home, metisHome: metisHome, executable: executable, toolPath: toolPath, uid: os.Getuid(), run: runCoordinatorLaunchctl}, nil
}

func coordinatorSupervisorToolPath(value string) (string, error) {
	if strings.ContainsAny(value, "\x00\r\n") {
		return "", errors.New("coordinator service PATH contains control characters")
	}
	seen := map[string]bool{}
	var entries []string
	for _, entry := range append(filepath.SplitList(value), "/opt/homebrew/bin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin") {
		if entry == "" || !filepath.IsAbs(entry) {
			continue
		}
		entry = filepath.Clean(entry)
		if !seen[entry] {
			entries = append(entries, entry)
			seen[entry] = true
		}
	}
	return strings.Join(entries, string(os.PathListSeparator)), nil
}

func supervisorCanonicalDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("coordinator service requires existing HOME and METIS_HOME directories")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", errors.New("coordinator service home must be a directory")
	}
	return real, nil
}

// The OS manager receives only arguments and three non-secret environment
// values. Provider credentials remain under the fixed METIS_HOME; the ingress
// credential is stored in a private regular file, never in the plist or logs.
func cmdProjectCoordinatorService(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis coordinator service install RUN --id NAME --agent PROFILE --token-file PATH --listen 127.0.0.1:PORT | status NAME [--json] | uninstall NAME")
	}
	switch args[0] {
	case "install":
		return cmdCoordinatorServiceInstall(ctx, args[1:])
	case "status", "uninstall":
		if len(args) < 2 {
			return errors.New("coordinator service status/uninstall requires a service ID")
		}
		fs := flag.NewFlagSet("coordinator service "+args[0], flag.ContinueOnError)
		jsonOut := fs.Bool("json", false, "emit non-secret service metadata")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if len(fs.Args()) != 0 {
			return errors.New("coordinator service does not accept extra arguments")
		}
		supervisor, err := newCoordinatorSupervisor()
		if err != nil {
			return err
		}
		if args[0] == "uninstall" {
			if err := supervisor.uninstall(ctx, args[1]); err != nil {
				return err
			}
			if *jsonOut {
				return writeCoordinatorJSON(map[string]any{"id": args[1], "uninstalled": true})
			}
			fmt.Fprintf(os.Stdout, "Removed coordinator service %s; project state was retained.\n", args[1])
			return nil
		}
		status, err := supervisor.status(ctx, args[1])
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeCoordinatorJSON(status)
		}
		fmt.Fprintf(os.Stdout, "Service %s: installed=%t loaded=%t state=%s pid=%d binary_verified=%t\n", status.ID, status.Installed, status.Loaded, status.State, status.PID, status.BinaryVerified)
		return nil
	default:
		return errors.New("unknown coordinator service action; use install, status or uninstall")
	}
}

func cmdCoordinatorServiceInstall(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("coordinator service install requires a project run ID")
	}
	fs := flag.NewFlagSet("coordinator service install", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var id string
	opts := coordinatorServiceOptions{runID: args[0]}
	fs.StringVar(&id, "id", "", "service name using letters, digits, underscore or hyphen")
	fs.StringVar(&opts.workspace, "cwd", "", "fixed project working directory")
	fs.StringVar(&opts.agentProfile, "agent", "", "required saved implementation profile")
	fs.StringVar(&opts.verifierProfile, "verify-agent", "", "saved verification profile (default: --agent)")
	fs.StringVar(&opts.tokenFile, "token-file", "", "private bearer-token file copied into the managed service")
	fs.StringVar(&opts.listen, "listen", "", "required loopback IP and fixed port")
	fs.Var(&opts.jobIDs, "job", "allow automatic structured tasks for this saved job (repeatable)")
	fs.IntVar(&opts.workers, "workers", 1, "maximum concurrent CLI children (1-16)")
	fs.DurationVar(&opts.poll, "poll", 500*time.Millisecond, "graph scan interval (minimum 50ms)")
	fs.DurationVar(&opts.lease, "lease", 15*time.Minute, "renewable worker claim duration (1s-24h)")
	fs.StringVar(&opts.usageReceiptDir, "usage-receipts", "", "private host receipt directory outside the workspace")
	fs.StringVar(&opts.usageCommand, "usage-reconcile-command", "", "optional private absolute usage adapter outside the workspace")
	fs.StringVar(&opts.usageBillingConfig, "usage-billing-config", "", "private native provider billing configuration outside the workspace")
	jsonOut := fs.Bool("json", false, "emit non-secret service metadata")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("coordinator service install does not accept extra arguments")
	}
	workspace, err := projectCoordinatorWorkspace(opts.workspace)
	if err != nil {
		return err
	}
	opts.workspace = workspace
	opts.agentProfile = strings.TrimSpace(opts.agentProfile)
	opts.verifierProfile = strings.TrimSpace(opts.verifierProfile)
	if opts.verifierProfile == "" {
		opts.verifierProfile = opts.agentProfile
	}
	if err := validateCoordinatorSupervisorOptions(id, opts); err != nil {
		return err
	}
	for _, profile := range []string{opts.agentProfile, opts.verifierProfile} {
		if _, err := rtpkg.LoadAgentProfileForWorkspace(profile, workspace); err != nil {
			return fmt.Errorf("coordinator service profile validation: %w", err)
		}
	}
	run, err := projectCoordinatorStore(projectcoord.WithProbeContext(ctx)).Get(workspace, opts.runID)
	if err != nil {
		return err
	}
	if run.Status == projectcoord.RunCancelled || len(run.CheckCommands) == 0 {
		return errors.New("coordinator service requires a non-cancelled run with host acceptance checks")
	}
	supervisor, err := newCoordinatorSupervisor()
	if err != nil {
		return err
	}
	reconciler, err := newCoordinatorUsageReconciler(projectCoordinatorStore(projectcoord.WithProbeContext(ctx)), workspace, opts.runID, supervisor.metisHome, opts.usageReceiptDir, opts.usageCommand, nil)
	if err != nil {
		return err
	}
	if err := reconciler.configureBilling(opts.usageBillingConfig); err != nil {
		return err
	}
	if err := inJobWorkspace(workspace, func() error { _, err := loadCoordinatorServiceJobs(opts, supervisor.metisHome); return err }); err != nil {
		return err
	}
	opts.usageReceiptDir, opts.usageCommand = reconciler.receiptDir, reconciler.command
	status, err := supervisor.install(ctx, id, opts)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(status)
	}
	fmt.Fprintf(os.Stdout, "Installed coordinator service %s for run %s. launchd will restart it after exit and start it at user login.\nPlist: %s\nManaged CLI: %s\n", id, opts.runID, status.Manifest.Plist, status.Manifest.Executable)
	return nil
}

func validCoordinatorServiceID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func validateCoordinatorSupervisorOptions(id string, opts coordinatorServiceOptions) error {
	if !validCoordinatorServiceID(id) {
		return errors.New("coordinator service ID must use 1-64 letters, digits, underscores or hyphens")
	}
	if opts.runID == "" || opts.agentProfile == "" || opts.verifierProfile == "" || !filepath.IsAbs(opts.workspace) {
		return errors.New("coordinator service requires a run, absolute workspace and saved profiles")
	}
	if err := validateCoordinatorServiceListen(opts.listen); err != nil {
		return err
	}
	_, port, _ := net.SplitHostPort(opts.listen)
	if port == "0" || strings.TrimLeft(port, "0") == "" {
		return errors.New("coordinator service requires a fixed nonzero listening port")
	}
	if opts.workers < 1 || opts.workers > 16 || opts.poll < 50*time.Millisecond || opts.lease < time.Second || opts.lease > 24*time.Hour {
		return errors.New("coordinator service requires workers 1-16, poll >=50ms and lease 1s-24h")
	}
	for _, id := range opts.jobIDs {
		var ids coordinatorServiceJobIDs
		if err := ids.Set(id); err != nil {
			return err
		}
	}
	for _, value := range []string{opts.runID, opts.workspace, opts.agentProfile, opts.verifierProfile} {
		if strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("coordinator service arguments cannot contain control characters")
		}
	}
	return nil
}

func (s *coordinatorSupervisor) paths(id string) (directory, plist, label, domain string) {
	directory = filepath.Join(s.metisHome, "coordinator-services", id)
	label = "com.metis.coordinator." + id
	plist = filepath.Join(s.userHome, "Library", "LaunchAgents", label+".plist")
	domain = "gui/" + strconv.Itoa(s.uid)
	return
}

func (s *coordinatorSupervisor) install(ctx context.Context, id string, opts coordinatorServiceOptions) (_ coordinatorSupervisorStatus, resultErr error) {
	if s.platform != "darwin" {
		return coordinatorSupervisorStatus{}, errors.New("host supervision currently requires macOS launchd")
	}
	if err := validateCoordinatorSupervisorOptions(id, opts); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(opts.workspace)
	if err != nil {
		return coordinatorSupervisorStatus{}, coordinatorUsageFailure("invalid_workspace")
	}
	opts.workspace = canonicalWorkspace
	if opts.usageReceiptDir == "" {
		opts.usageReceiptDir = filepath.Join(s.metisHome, "coordinator-usage-receipts", opts.runID)
	}
	if !filepath.IsAbs(opts.usageReceiptDir) {
		return coordinatorSupervisorStatus{}, coordinatorUsageFailure("receipt_directory_requires_absolute_path")
	}
	receiptDir, err := coordinatorUsageCanonicalFuturePath(opts.usageReceiptDir)
	if err != nil || !coordinatorUsageOutsideWorkspace(receiptDir, opts.workspace) {
		return coordinatorSupervisorStatus{}, coordinatorUsageFailure("receipt_directory_must_be_outside_workspace")
	}
	if _, err := os.Lstat(receiptDir); err == nil {
		if _, err := coordinatorUsagePrivateDirectory(receiptDir, nil); err != nil {
			return coordinatorSupervisorStatus{}, err
		}
	} else if !os.IsNotExist(err) {
		return coordinatorSupervisorStatus{}, coordinatorUsageFailure("unsafe_receipt_directory")
	}
	opts.usageReceiptDir = receiptDir
	if opts.usageCommand != "" {
		if _, _, err := coordinatorUsageReadPrivateFile(opts.usageCommand, opts.workspace, 128<<20, true); err != nil {
			return coordinatorSupervisorStatus{}, err
		}
		opts.usageCommand, err = filepath.EvalSymlinks(opts.usageCommand)
		if err != nil {
			return coordinatorSupervisorStatus{}, coordinatorUsageFailure("invalid_adapter_path")
		}
	}
	token, err := readCoordinatorServiceToken(opts.tokenFile)
	if err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	directory, plist, label, domain := s.paths(id)
	if err := supervisorEnsureDirectory(filepath.Dir(directory), 0700, true); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return coordinatorSupervisorStatus{}, errors.New("coordinator service ID already exists or its private directory cannot be created")
	}
	ownedPlist, bootstrapped := false, false
	defer func() {
		if resultErr == nil {
			return
		}
		// Never remove a binary/token while launchd could still restart it.
		if bootstrapped {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			output, probeErr := s.run(cleanupCtx, "print", domain+"/"+label)
			if probeErr != nil && !supervisorServiceMissing(probeErr) || probeErr == nil && !supervisorLaunchctlOwnsPath(output, plist) {
				resultErr = fmt.Errorf("%w; rollback could not verify owned launchd service; files were retained", resultErr)
				return
			}
			if probeErr == nil {
				if _, err := s.run(cleanupCtx, "bootout", domain+"/"+label); err != nil && !supervisorServiceMissing(err) {
					resultErr = fmt.Errorf("%w; launchd rollback could not stop the service; retain files and run service uninstall %s", resultErr, id)
					return
				}
				if err := s.waitForStop(ctx, domain+"/"+label, plist); err != nil {
					resultErr = errors.Join(resultErr, fmt.Errorf("rollback stop was not confirmed; files were retained: %w", err))
					return
				}
			}
		}
		if ownedPlist {
			_ = os.Remove(plist)
		}
		_ = os.RemoveAll(directory)
	}()
	if err := supervisorEnsureDirectory(filepath.Dir(plist), 0755, false); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	if _, err := os.Lstat(plist); !os.IsNotExist(err) {
		return coordinatorSupervisorStatus{}, errors.New("coordinator service launch-agent path already exists or cannot be inspected")
	}
	if output, err := s.run(ctx, "print", domain+"/"+label); err == nil || !supervisorServiceMissing(err) {
		_ = output
		return coordinatorSupervisorStatus{}, errors.New("coordinator service label already exists or launchd domain is unavailable")
	}
	binDirectory := filepath.Join(directory, "bin")
	if err := os.Mkdir(binDirectory, 0700); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	managedBinary := filepath.Join(binDirectory, "metis")
	binaryHash, err := supervisorCopyBinary(s.executable, managedBinary)
	if err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	tokenPath := filepath.Join(directory, "ingress-token")
	if err := supervisorWritePrivateExclusive(tokenPath, []byte(token+"\n"), 0600); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		if err := supervisorWritePrivateExclusive(filepath.Join(directory, name), nil, 0600); err != nil {
			return coordinatorSupervisorStatus{}, err
		}
	}
	manifest := coordinatorSupervisorManifest{Schema: coordinatorSupervisorSchema, ID: id, Label: label, Domain: domain, CreatedAt: time.Now().UTC(), Directory: directory, Plist: plist, Executable: managedBinary, BinarySHA256: binaryHash, RunID: opts.runID, Workspace: opts.workspace, MetisHome: s.metisHome, UserHome: s.userHome, Listen: opts.listen, JobIDs: append([]string(nil), opts.jobIDs...)}
	manifest.UsageReceiptDir, manifest.UsageCommand = opts.usageReceiptDir, opts.usageCommand
	manifest.UsageBillingConfig = opts.usageBillingConfig
	manifest.ToolPath, err = coordinatorSupervisorToolPath(s.toolPath)
	if err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	content, err := coordinatorSupervisorPlist(manifest, opts, tokenPath)
	if err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	sum := sha256.Sum256(content)
	manifest.PlistSHA256 = hex.EncodeToString(sum[:])
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	if err := supervisorWritePrivateExclusive(filepath.Join(directory, "manifest.json"), manifestBytes, 0600); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	if err := supervisorWritePrivateExclusive(plist, content, 0600); err != nil {
		return coordinatorSupervisorStatus{}, err
	}
	ownedPlist = true
	if _, err := s.run(ctx, "enable", domain+"/"+label); err != nil {
		return coordinatorSupervisorStatus{}, errors.New("launchctl could not enable the coordinator service")
	}
	bootstrapped = true
	if _, err := s.run(ctx, "bootstrap", domain, plist); err != nil {
		return coordinatorSupervisorStatus{}, errors.New("launchctl could not bootstrap the coordinator service")
	}
	status, err := s.status(ctx, id)
	if err != nil || !status.Loaded {
		return coordinatorSupervisorStatus{}, errors.New("coordinator service bootstrap was not confirmed by launchd")
	}
	return status, nil
}

func supervisorEnsureDirectory(path string, mode os.FileMode, private bool) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0022 != 0 || (private && info.Mode().Perm()&0077 != 0) {
		return errors.New("coordinator service directory must be regular and have safe permissions")
	}
	return nil
}

func supervisorWritePrivateExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func supervisorCopyBinary(source, target string) (string, error) {
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0111 == 0 || before.Mode().Perm()&0022 != 0 || before.Size() > 512<<20 {
		return "", errors.New("coordinator service executable must be a regular non-writable executable of at most 512 MiB")
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	observed, err := input.Stat()
	if err != nil || !os.SameFile(before, observed) {
		return "", errors.New("coordinator service executable changed while opening")
	}
	output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return "", err
	}
	defer output.Close()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, (512<<20)+1))
	if err != nil || n != before.Size() {
		return "", errors.New("coordinator service executable changed while copying")
	}
	after, err := input.Stat()
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("coordinator service executable changed while copying")
	}
	if err := output.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func coordinatorSupervisorPlist(manifest coordinatorSupervisorManifest, opts coordinatorServiceOptions, tokenPath string) ([]byte, error) {
	var out bytes.Buffer
	out.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n<plist version=\"1.0\"><dict>\n")
	writeString := func(tag, value string) error {
		out.WriteString("<" + tag + ">")
		if err := xml.EscapeText(&out, []byte(value)); err != nil {
			return err
		}
		out.WriteString("</" + tag + ">\n")
		return nil
	}
	if err := writeString("key", "Label"); err != nil {
		return nil, err
	}
	if err := writeString("string", manifest.Label); err != nil {
		return nil, err
	}
	out.WriteString("<key>ProgramArguments</key><array>\n")
	arguments := []string{manifest.Executable, "coordinator", "serve", opts.runID, "--cwd", opts.workspace, "--agent", opts.agentProfile, "--verify-agent", opts.verifierProfile, "--token-file", tokenPath, "--listen", opts.listen, "--workers", strconv.Itoa(opts.workers), "--poll", opts.poll.String(), "--lease", opts.lease.String()}
	if opts.usageReceiptDir != "" {
		arguments = append(arguments, "--usage-receipts", opts.usageReceiptDir)
	}
	if opts.usageCommand != "" {
		arguments = append(arguments, "--usage-reconcile-command", opts.usageCommand)
	}
	if opts.usageBillingConfig != "" {
		arguments = append(arguments, "--usage-billing-config", opts.usageBillingConfig)
	}
	for _, id := range opts.jobIDs {
		arguments = append(arguments, "--job", id)
	}
	for _, argument := range arguments {
		if err := writeString("string", argument); err != nil {
			return nil, err
		}
	}
	out.WriteString("</array>\n<key>WorkingDirectory</key>\n")
	if err := writeString("string", opts.workspace); err != nil {
		return nil, err
	}
	out.WriteString("<key>EnvironmentVariables</key><dict>\n")
	for _, entry := range [][2]string{{"HOME", manifest.UserHome}, {"METIS_HOME", manifest.MetisHome}, {"PATH", manifest.ToolPath}} {
		if err := writeString("key", entry[0]); err != nil {
			return nil, err
		}
		if err := writeString("string", entry[1]); err != nil {
			return nil, err
		}
	}
	out.WriteString("</dict>\n<key>KeepAlive</key><true/>\n<key>RunAtLoad</key><true/>\n<key>ThrottleInterval</key><integer>5</integer>\n<key>ExitTimeOut</key><integer>10</integer>\n<key>AbandonProcessGroup</key><false/>\n<key>Umask</key><integer>63</integer>\n<key>ProcessType</key><string>Background</string>\n")
	for _, entry := range [][2]string{{"StandardOutPath", filepath.Join(manifest.Directory, "stdout.log")}, {"StandardErrorPath", filepath.Join(manifest.Directory, "stderr.log")}} {
		if err := writeString("key", entry[0]); err != nil {
			return nil, err
		}
		if err := writeString("string", entry[1]); err != nil {
			return nil, err
		}
	}
	out.WriteString("</dict></plist>\n")
	return out.Bytes(), nil
}

type coordinatorLaunchctlError struct{ missing bool }

func (e *coordinatorLaunchctlError) Error() string { return "launchctl command failed" }
func supervisorServiceMissing(err error) bool {
	var commandError *coordinatorLaunchctlError
	return errors.As(err, &commandError) && commandError.missing
}

func runCoordinatorLaunchctl(parent context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/launchctl", args...)
	command.WaitDelay = time.Second
	output, err := command.CombinedOutput()
	if err == nil {
		return output, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	text := strings.ToLower(string(output))
	return nil, &coordinatorLaunchctlError{missing: strings.Contains(text, "could not find service") || strings.Contains(text, "could not find specified service") || strings.Contains(text, "no such process")}
}

func (s *coordinatorSupervisor) readManifest(id string) (*coordinatorSupervisorManifest, error) {
	if !validCoordinatorServiceID(id) {
		return nil, errors.New("invalid coordinator service ID")
	}
	directory, plist, label, domain := s.paths(id)
	for _, path := range []string{filepath.Dir(directory), directory} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return nil, err
		}
		if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("coordinator service metadata directory is unsafe")
		}
	}
	manifestPath := filepath.Join(directory, "manifest.json")
	info, err := os.Lstat(manifestPath)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return nil, errors.New("coordinator service manifest is unsafe")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, err
	}
	var manifest coordinatorSupervisorManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, errors.New("coordinator service manifest is invalid")
	}
	if manifest.Schema != coordinatorSupervisorSchema || manifest.ID != id || manifest.Label != label || manifest.Domain != domain || manifest.Directory != directory || manifest.Plist != plist || manifest.Executable != filepath.Join(directory, "bin", "metis") || manifest.MetisHome != s.metisHome || manifest.UserHome != s.userHome {
		return nil, errors.New("coordinator service manifest ownership does not match this user and home")
	}
	return &manifest, nil
}

func (s *coordinatorSupervisor) status(ctx context.Context, id string) (coordinatorSupervisorStatus, error) {
	status := coordinatorSupervisorStatus{ID: id}
	manifest, err := s.readManifest(id)
	if os.IsNotExist(err) {
		return status, nil
	}
	if err != nil {
		return status, err
	}
	status.Installed, status.Manifest = true, manifest
	if err := supervisorVerifyPlist(manifest); err != nil {
		return status, err
	}
	if data, err := os.ReadFile(manifest.Executable); err == nil {
		sum := sha256.Sum256(data)
		status.BinaryVerified = hex.EncodeToString(sum[:]) == manifest.BinarySHA256
	}
	output, err := s.run(ctx, "print", manifest.Domain+"/"+manifest.Label)
	if supervisorServiceMissing(err) {
		return status, nil
	}
	if err != nil {
		return status, errors.New("coordinator service launchd status could not be queried")
	}
	if !supervisorLaunchctlOwnsPath(output, manifest.Plist) {
		return status, errors.New("launchd service label points to a different plist; refusing to manage it")
	}
	status.Loaded = true
	for _, line := range strings.Split(string(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "state":
			status.State = value
		case "pid":
			status.PID, _ = strconv.Atoi(value)
		case "runs":
			status.Runs, _ = strconv.Atoi(value)
		case "last exit code":
			if n, err := strconv.Atoi(value); err == nil {
				status.LastExitCode = &n
			}
		}
	}
	return status, nil
}

func supervisorLaunchctlOwnsPath(output []byte, path string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), " = "); ok && key == "path" {
			return value == path
		}
	}
	return false
}

func supervisorVerifyPlist(manifest *coordinatorSupervisorManifest) error {
	info, err := os.Lstat(manifest.Plist)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 64<<10 {
		return errors.New("coordinator service plist is missing or unsafe")
	}
	data, err := os.ReadFile(manifest.Plist)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != manifest.PlistSHA256 {
		return errors.New("coordinator service plist was modified; refusing to manage it")
	}
	return nil
}

func (s *coordinatorSupervisor) uninstall(ctx context.Context, id string) error {
	status, err := s.status(ctx, id)
	if err != nil {
		return err
	}
	if !status.Installed {
		return nil
	}
	if status.Loaded {
		if _, err := s.run(ctx, "bootout", status.Manifest.Domain+"/"+status.Manifest.Label); err != nil && !supervisorServiceMissing(err) {
			return errors.New("launchctl could not stop the coordinator service; managed files were retained")
		}
		if err := s.waitForStop(ctx, status.Manifest.Domain+"/"+status.Manifest.Label, status.Manifest.Plist); err != nil {
			return fmt.Errorf("coordinator service stop was not confirmed; managed files were retained: %w", err)
		}
	}
	if err := supervisorVerifyPlist(status.Manifest); err != nil {
		return err
	}
	if err := os.Remove(status.Manifest.Plist); err != nil {
		return err
	}
	return os.RemoveAll(status.Manifest.Directory)
}

// bootout can return before launchd has removed its service registration.
// Only an explicitly missing owned label authorizes deleting its executable,
// credential and plist. Polling uses a deadline and cancellable timer, while
// unknown manager failures or a retargeted label fail closed immediately.
func (s *coordinatorSupervisor) waitForStop(parent context.Context, target, plist string) error {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		output, err := s.run(ctx, "print", target)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if supervisorServiceMissing(err) {
			return nil
		}
		if err != nil {
			return errors.New("launchd stop status could not be queried")
		}
		if !supervisorLaunchctlOwnsPath(output, plist) {
			return errors.New("launchd service label changed ownership during stop")
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
