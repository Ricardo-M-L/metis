package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEvaluationSandboxRejectsUnsafeRootsAndDescriptors(t *testing.T) {
	work, tests := t.TempDir(), t.TempDir()
	for name, roots := range map[string][2]string{
		"empty": {"", tests}, "root": {string(filepath.Separator), tests},
		"same": {work, work}, "nested": {work, filepath.Join(work, "nested")},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "nested" {
				if err := os.Mkdir(roots[1], 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := WrapEvaluation(exec.Command("/bin/sh", "-c", "true"), roots[0], roots[1]); err == nil {
				t.Fatal("unsafe roots accepted")
			}
		})
	}
	if _, err := WrapEvaluation(nil, work, tests); err == nil {
		t.Fatal("nil command accepted")
	}
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.ExtraFiles = []*os.File{os.Stdin}
	if _, err := WrapEvaluation(cmd, work, tests); err == nil {
		t.Fatal("extra descriptor accepted")
	}
}

func evaluationSandboxAvailable(t *testing.T, work, tests string) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("evaluation isolation is unsupported on this platform")
	}
	if runtime.GOOS == "darwin" {
		if _, err := os.Stat(darwinEvaluationExecutable()); err != nil {
			t.Skip(err)
		}
	} else if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip(err)
	}
	cmd, err := WrapEvaluation(exec.Command("/usr/bin/true"), work, tests)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "sandbox_apply: Operation not permitted") ||
			strings.Contains(string(output), "No permissions to create new namespace") ||
			strings.Contains(string(output), "Creating new namespace failed: Operation not permitted") {
			t.Skipf("host prohibits sandbox isolation: %s", output)
		}
		t.Fatalf("sandbox preflight: %v: %s", err, output)
	}
}

// Keep the generic test compilable on every platform.
func darwinEvaluationExecutable() string { return "/usr/bin/sandbox-exec" }

func TestEvaluationSandboxDevelopmentFixtureAndIsolation(t *testing.T) {
	work, tests, outside := t.TempDir(), t.TempDir(), t.TempDir()
	evaluationSandboxAvailable(t, work, tests)
	secret := filepath.Join(outside, "random-secret.txt")
	if err := os.WriteFile(secret, []byte("outside-secret-must-not-be-readable"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(work, "outside-link")); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	checker := fmt.Sprintf(`import importlib.util, json, os, pathlib, socket, sys
workspace = pathlib.Path(%s)
spec = importlib.util.spec_from_file_location("implementation", workspace / "implementation.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
assert module.add(20, 22) == 42
for path in [%s, str(workspace / "outside-link"), "/proc/1/environ"]:
    try:
        pathlib.Path(path).read_bytes()
    except OSError:
        pass
    else:
        raise AssertionError("outside read escaped: " + path)
try:
    pathlib.Path(__file__).write_text("tampered")
except OSError:
    pass
else:
    raise AssertionError("host test was writable")
try:
    linked_test = workspace / "linked-host-test.py"
    os.link(__file__, linked_test)
    linked_test.write_text("tampered through hard link")
except OSError:
    pass
else:
    raise AssertionError("host test writable through hard link")
try:
    pathlib.Path(%s).write_text("escaped")
except OSError:
    pass
else:
    raise AssertionError("outside write escaped")
try:
    sock = socket.socket()
    sock.settimeout(0.3)
    sock.connect(("127.0.0.1", %d))
except OSError:
    pass
else:
    raise AssertionError("network escaped")
assert "METIS_EVALUATION_HOST_SECRET" not in os.environ
if sys.platform == "darwin":
    import ctypes
    libc = ctypes.CDLL(None, use_errno=True)
    name = (ctypes.c_int * 3)(1, 49, %d)  # CTL_KERN, KERN_PROCARGS2, host pid
    size = ctypes.c_size_t(65536)
    data = ctypes.create_string_buffer(size.value)
    assert libc.sysctl(name, 3, data, ctypes.byref(size), None, 0) != 0, "host process arguments/environment leaked"
print(json.dumps({"answer": module.add(20, 22), "isolation": "enforced"}, sort_keys=True))
`, strconv.Quote(work), strconv.Quote(secret), strconv.Quote(filepath.Join(outside, "escaped")), port, os.Getpid())
	checkPath := filepath.Join(tests, "check.py")
	if err := os.WriteFile(checkPath, []byte(checker), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("METIS_EVALUATION_HOST_SECRET", "must-not-be-inherited")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Exercises a genuine source edit, shell redirection, isolated Python
	// execution, immutable host tests and host-side artifact inspection.
	script := "printf 'def add(a, b):\n    return a + b\n' > implementation.py\n/usr/bin/python3 -I -S " + shellEvaluationQuote(checkPath)
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", script)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	wrapped, err := WrapEvaluation(cmd, work, tests)
	if err != nil {
		t.Fatal(err)
	}
	if wrapped != cmd || wrapped.Stdout != &stdout || wrapped.Stderr != &stderr {
		t.Fatal("wrapper replaced command/stdio")
	}
	if err := wrapped.Run(); err != nil {
		t.Fatalf("development fixture: %v\nstdout: %s\nstderr: %s", err, &stdout, &stderr)
	}
	if strings.TrimSpace(stdout.String()) != `{"answer": 42, "isolation": "enforced"}` {
		t.Fatalf("unexpected fixture result %q; stderr %s", stdout.String(), &stderr)
	}
	if got, err := os.ReadFile(checkPath); err != nil || string(got) != checker {
		t.Fatalf("host checker changed: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(work, "implementation.py")); err != nil || string(got) != "def add(a, b):\n    return a + b\n" {
		t.Fatalf("source artifact: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "escaped")); !os.IsNotExist(err) {
		t.Fatalf("outside artifact exists: %v", err)
	}
}

func shellEvaluationQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
