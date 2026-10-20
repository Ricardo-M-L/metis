package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type coordinatorLaunchctlFixture struct {
	loaded                                           bool
	plist                                            string
	bootstrapError, bootoutError, retainAfterBootout bool
	calls                                            [][]string
	stopping                                         bool
	delayedStopPrints                                int
	stopPrints                                       int
	unknownAfterBootout, foreignAfterBootout         bool
	afterBootout                                     func()
}

func (f *coordinatorLaunchctlFixture) run(ctx context.Context, args ...string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "print":
		if f.stopping {
			f.stopPrints++
			if f.unknownAfterBootout {
				return nil, errors.New("launchd status unavailable")
			}
			if f.foreignAfterBootout {
				f.plist = "/foreign-after-bootout.plist"
			}
			if !f.retainAfterBootout && f.delayedStopPrints == 0 {
				f.loaded = false
			} else if f.delayedStopPrints > 0 {
				f.delayedStopPrints--
			}
		}
		if !f.loaded {
			return nil, &coordinatorLaunchctlError{missing: true}
		}
		return []byte(fmt.Sprintf("gui/501/com.metis.coordinator.test = {\n path = %s\n state = running\n pid = 1234\n runs = 2\n last exit code = 0\n}", f.plist)), nil
	case "enable":
		return nil, nil
	case "bootstrap":
		f.loaded, f.plist = true, args[2]
		if f.bootstrapError {
			return nil, errors.New("partial registration error")
		}
		return nil, nil
	case "bootout":
		if f.bootoutError {
			return nil, errors.New("denied")
		}
		f.stopping = true
		if !f.retainAfterBootout && f.delayedStopPrints == 0 {
			f.loaded = false
		}
		if f.afterBootout != nil {
			f.afterBootout()
		}
		return nil, nil
	}
	return nil, errors.New("unexpected launchctl call")
}

func TestCoordinatorSupervisorWaitsForAsynchronousBootout(t *testing.T) {
	for _, rollback := range []bool{false, true} {
		t.Run(map[bool]string{false: "uninstall", true: "rollback"}[rollback], func(t *testing.T) {
			supervisor, opts, fake := coordinatorSupervisorFixture(t)
			fake.delayedStopPrints = 2
			if rollback {
				fake.bootstrapError = true
				if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
					t.Fatal("partial bootstrap was accepted")
				}
			} else {
				if _, err := supervisor.install(context.Background(), "test", opts); err != nil {
					t.Fatal(err)
				}
				if err := supervisor.uninstall(context.Background(), "test"); err != nil {
					t.Fatal("async stop was prematurely rejected", err)
				}
			}
			if fake.loaded || fake.stopPrints != 3 {
				t.Fatalf("asynchronous stop was not confirmed: loaded=%t probes=%d", fake.loaded, fake.stopPrints)
			}
			directory, plist, _, _ := supervisor.paths("test")
			for _, path := range []string{directory, plist} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("confirmed stop retained %s", path)
				}
			}
		})
	}
}

func TestCoordinatorSupervisorCancelledStopRetainsVerifiedFiles(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		supervisor, opts, fake := coordinatorSupervisorFixture(t)
		status, err := supervisor.install(context.Background(), "test", opts)
		if err != nil {
			t.Fatal(err)
		}
		fake.retainAfterBootout = true
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithTimeout(context.Background(), 80*time.Millisecond)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			fake.afterBootout = cancel
		}
		defer cancel()
		err = supervisor.uninstall(ctx, "test")
		want := context.Canceled
		if deadline {
			want = context.DeadlineExceeded
		}
		if !errors.Is(err, want) {
			t.Fatalf("stop lost cancellation identity: %v", err)
		}
		for _, path := range []string{status.Manifest.Plist, status.Manifest.Executable, filepath.Join(status.Manifest.Directory, "ingress-token")} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("canceled stop deleted %s", path)
			}
		}
	}
}

func TestCoordinatorSupervisorUnknownOrForeignStopRetainsFiles(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		supervisor, opts, fake := coordinatorSupervisorFixture(t)
		status, err := supervisor.install(context.Background(), "test", opts)
		if err != nil {
			t.Fatal(err)
		}
		fake.retainAfterBootout = true
		fake.foreignAfterBootout, fake.unknownAfterBootout = foreign, !foreign
		if err := supervisor.uninstall(context.Background(), "test"); err == nil {
			t.Fatal("unknown or foreign service was classified as stopped")
		}
		for _, path := range []string{status.Manifest.Plist, status.Manifest.Executable} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("unsafe stop deleted %s", path)
			}
		}
	}
}

func coordinatorSupervisorFixture(t *testing.T) (*coordinatorSupervisor, coordinatorServiceOptions, *coordinatorLaunchctlFixture) {
	t.Helper()
	root := t.TempDir()
	userHome, metisHome := filepath.Join(root, "用户 & home"), filepath.Join(root, "metis-home")
	for _, path := range []string{userHome, metisHome} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	workspace := filepath.Join(root, "project & <plan>")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "temporary-cli")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(root, "original-token")
	if err := os.WriteFile(token, []byte(coordinatorServiceTestToken), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &coordinatorLaunchctlFixture{}
	supervisor := &coordinatorSupervisor{platform: "darwin", userHome: userHome, metisHome: metisHome, executable: binary, uid: 501, run: fake.run}
	opts := coordinatorServiceOptions{runID: "run-id", workspace: workspace, tokenFile: token, agentProfile: "implementation & plan", verifierProfile: "verify", listen: "127.0.0.1:18997", workers: 2, poll: 500 * time.Millisecond, lease: time.Minute}
	return supervisor, opts, fake
}

func TestCoordinatorSupervisorInstallsStablePrivateCLIAndRemovesOnlyOwnedFiles(t *testing.T) {
	supervisor, opts, fake := coordinatorSupervisorFixture(t)
	status, err := supervisor.install(context.Background(), "test", opts)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Loaded || !status.Installed || !status.BinaryVerified || status.PID != 1234 || status.Runs != 2 {
		t.Fatalf("install status: %+v", status)
	}
	manifest := status.Manifest
	if manifest.Executable == supervisor.executable {
		t.Fatal("launchd refers to temporary CLI")
	}
	plist, err := os.ReadFile(manifest.Plist)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plist, []byte(coordinatorServiceTestToken)) || bytes.Contains(plist, []byte(supervisor.executable)) {
		t.Fatal("plist contains credential or temporary binary")
	}
	for _, literal := range []string{"<key>KeepAlive</key><true/>", "<key>RunAtLoad</key><true/>", "<integer>63</integer>", "<key>AbandonProcessGroup</key><false/>", "implementation &amp; plan", "project &amp; &lt;plan&gt;"} {
		if !bytes.Contains(plist, []byte(literal)) {
			t.Fatalf("plist is missing %s", literal)
		}
	}
	decoder := xml.NewDecoder(bytes.NewReader(plist))
	for {
		_, err := decoder.Token()
		if err != nil {
			if err.Error() != "EOF" {
				t.Fatal(err)
			}
			break
		}
	}
	for path, mode := range map[string]os.FileMode{manifest.Executable: 0700, manifest.Plist: 0600, filepath.Join(manifest.Directory, "ingress-token"): 0600, filepath.Join(manifest.Directory, "manifest.json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("unsafe file %s: %v", path, err)
		}
	}
	// Deleting the caller's test CLI does not remove the installed fixed copy.
	if err := os.Remove(supervisor.executable); err != nil {
		t.Fatal(err)
	}
	if status, err := supervisor.status(context.Background(), "test"); err != nil || !status.BinaryVerified {
		t.Fatalf("stable binary disappeared: %+v %v", status, err)
	}
	if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
		t.Fatal("silently replaced installed service")
	}
	if err := supervisor.uninstall(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	if fake.loaded {
		t.Fatal("service is still registered")
	}
	for _, path := range []string{manifest.Directory, manifest.Plist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("owned artifact retained: %s", path)
		}
	}
	if _, err := os.Stat(opts.workspace); err != nil {
		t.Fatal("uninstall destroyed workspace")
	}
	if _, err := os.Stat(opts.tokenFile); err != nil {
		t.Fatal("uninstall destroyed caller token")
	}
	if err := supervisor.uninstall(context.Background(), "test"); err != nil {
		t.Fatal("uninstall is not idempotent", err)
	}
}

func TestCoordinatorSupervisorRollsBackPartialBootstrap(t *testing.T) {
	supervisor, opts, fake := coordinatorSupervisorFixture(t)
	fake.bootstrapError = true
	if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
		t.Fatal("accepted partial bootstrap")
	}
	if fake.loaded {
		t.Fatal("partial registration retained")
	}
	dir, plist, _, _ := supervisor.paths("test")
	for _, path := range []string{dir, plist} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("rollback retained %s", path)
		}
	}
}

func TestCoordinatorSupervisorRetainsFilesWhenStopCannotBeConfirmed(t *testing.T) {
	for _, deny := range []bool{true, false} {
		supervisor, opts, fake := coordinatorSupervisorFixture(t)
		status, err := supervisor.install(context.Background(), "test", opts)
		if err != nil {
			t.Fatal(err)
		}
		fake.bootoutError, fake.retainAfterBootout = deny, !deny
		started := time.Now()
		err = supervisor.uninstall(context.Background(), "test")
		if err == nil {
			t.Fatal("accepted unconfirmed stop")
		}
		if !deny && (!errors.Is(err, context.DeadlineExceeded) || time.Since(started) < 4900*time.Millisecond || time.Since(started) > 6*time.Second || fake.stopPrints <= 1) {
			t.Fatalf("stop timeout did not use bounded polling: probes=%d elapsed=%s error=%v", fake.stopPrints, time.Since(started), err)
		}
		for _, path := range []string{status.Manifest.Executable, status.Manifest.Plist, filepath.Join(status.Manifest.Directory, "ingress-token")} {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("deleted still-needed artifact: %s", path)
			}
		}
	}
}

func TestCoordinatorSupervisorRollbackCancellationRetainsOwnedFiles(t *testing.T) {
	supervisor, opts, fake := coordinatorSupervisorFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake.bootstrapError = true
	fake.retainAfterBootout = true
	fake.afterBootout = cancel
	_, err := supervisor.install(ctx, "test", opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("rollback did not retain cancellation identity: %v", err)
	}
	directory, plist, _, _ := supervisor.paths("test")
	for _, path := range []string{plist, filepath.Join(directory, "bin", "metis"), filepath.Join(directory, "ingress-token")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("canceled rollback deleted %s", path)
		}
	}
}

func TestCoordinatorSupervisorRejectsForeignOrTamperedService(t *testing.T) {
	for _, foreign := range []bool{true, false} {
		supervisor, opts, fake := coordinatorSupervisorFixture(t)
		status, err := supervisor.install(context.Background(), "test", opts)
		if err != nil {
			t.Fatal(err)
		}
		if foreign {
			fake.plist = "/foreign.plist"
		} else {
			if err := os.WriteFile(status.Manifest.Plist, []byte("different config"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		before := len(fake.calls)
		if err := supervisor.uninstall(context.Background(), "test"); err == nil {
			t.Fatal("accepted foreign/tampered launch agent")
		}
		for _, args := range fake.calls[before:] {
			if args[0] == "bootout" {
				t.Fatal("stopped a service without ownership proof")
			}
		}
		if _, err := os.Stat(status.Manifest.Executable); err != nil {
			t.Fatal("foreign service rejection removed binary")
		}
	}
}

func TestCoordinatorSupervisorRejectsUnsafeInstallArguments(t *testing.T) {
	supervisor, opts, _ := coordinatorSupervisorFixture(t)
	for _, id := range []string{"", "../foreign", "name.with.dot", strings.Repeat("x", 65)} {
		if _, err := supervisor.install(context.Background(), id, opts); err == nil {
			t.Fatalf("accepted ID %q", id)
		}
	}
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:000", "0.0.0.0:8000", "localhost:8080"} {
		clone := opts
		clone.listen = address
		if _, err := supervisor.install(context.Background(), "test", clone); err == nil {
			t.Fatalf("accepted address %s", address)
		}
	}
	if err := os.Chmod(opts.tokenFile, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
		t.Fatal("accepted public token")
	}
	if err := os.Chmod(opts.tokenFile, 0600); err != nil {
		t.Fatal(err)
	}
	services := filepath.Join(supervisor.metisHome, "coordinator-services")
	if err := os.Symlink(t.TempDir(), services); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
		t.Fatal("accepted service directory symlink")
	}
}

func TestCoordinatorSupervisorToolPathPreservesAbsoluteUserTools(t *testing.T) {
	path, err := coordinatorSupervisorToolPath("/Users/example/.local/bin:.:relative:/Library/Frameworks/Python.framework/bin::/usr/bin:/Users/example/.local/bin")
	if err != nil {
		t.Fatal(err)
	}
	entries := filepath.SplitList(path)
	if entries[0] != "/Users/example/.local/bin" || entries[1] != "/Library/Frameworks/Python.framework/bin" {
		t.Fatalf("lost user tool path: %s", path)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		if !filepath.IsAbs(entry) || seen[entry] {
			t.Fatalf("unsafe or duplicate PATH entry: %s", path)
		}
		seen[entry] = true
	}
	if _, err := coordinatorSupervisorToolPath("/bin\nAPI_KEY=secret"); err == nil {
		t.Fatal("accepted control character in PATH")
	}
}

func TestCoordinatorSupervisorPinsPrivateUsageAdapterAndReceiptDirectory(t *testing.T) {
	supervisor, opts, _ := coordinatorSupervisorFixture(t)
	opts.usageReceiptDir = filepath.Join(supervisor.metisHome, "private-receipts")
	opts.usageCommand = filepath.Join(supervisor.metisHome, "private-adapter")
	if err := os.WriteFile(opts.usageCommand, []byte("#!/bin/sh\nprintf '[]'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	status, err := supervisor.install(context.Background(), "test", opts)
	if err != nil {
		t.Fatal(err)
	}
	plist, err := os.ReadFile(status.Manifest.Plist)
	if err != nil {
		t.Fatal(err)
	}
	expectedCommand, err := filepath.EvalSymlinks(opts.usageCommand)
	if err != nil {
		t.Fatal(err)
	}
	expectedDirectory, err := coordinatorUsageCanonicalFuturePath(opts.usageReceiptDir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(plist, []byte("--usage-receipts")) || !bytes.Contains(plist, []byte("--usage-reconcile-command")) || status.Manifest.UsageCommand != expectedCommand || status.Manifest.UsageReceiptDir != expectedDirectory {
		t.Fatal("usage reconciliation configuration was not pinned")
	}
	if err := supervisor.uninstall(context.Background(), "test"); err != nil {
		t.Fatal(err)
	}
	opts.usageReceiptDir = opts.workspace
	if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
		t.Fatal("accepted workspace receipt directory")
	}
	opts.usageReceiptDir = filepath.Join(supervisor.metisHome, "private-receipts")
	if err := os.Chmod(opts.usageCommand, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.install(context.Background(), "test", opts); err == nil {
		t.Fatal("accepted non-private adapter")
	}
}
