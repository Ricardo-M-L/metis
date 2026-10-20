package projectcoord

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestOperationProcessHelper(t *testing.T) {
	action := os.Getenv("METIS_COORD_OPERATION_PROCESS_ACTION")
	if action == "" {
		return
	}
	fmt.Fprintln(os.Stdout, "ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	store := NewStore(os.Getenv("METIS_COORD_OPERATION_PROCESS_ROOT"))
	op, err := store.BeginOperation(os.Getenv("METIS_COORD_OPERATION_PROCESS_WORKSPACE"), os.Getenv("METIS_COORD_OPERATION_PROCESS_RUN"), os.Getenv("METIS_COORD_OPERATION_PROCESS_ITEM"), os.Getenv("METIS_COORD_OPERATION_PROCESS_WORKER"), os.Getenv("METIS_COORD_OPERATION_PROCESS_TOKEN"), operationTestInput("process remote write"))
	if action == "hold" {
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(os.Stdout, "prepared")
		// Simulate the unsafe crash window after the durable prepare and before
		// the write returns. Process.Kill below cannot run cleanup or finish.
		time.Sleep(time.Hour)
		return
	}
	if action != "begin" {
		fmt.Fprintln(os.Stderr, "unknown operation process action")
		os.Exit(2)
	}
	result := operationProcessResult{Operation: op, Uncertain: errors.Is(err, ErrOperationUncertain)}
	if err != nil {
		result.Error = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

type operationProcessResult struct {
	Operation OperationRecord `json:"operation"`
	Uncertain bool            `json:"uncertain"`
	Error     string          `json:"error"`
}

type operationProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	stderr bytes.Buffer
}

func startOperationProcess(t *testing.T, f *operationFixture, action string) *operationProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := &operationProcess{cmd: exec.Command(executable, "-test.run=^TestOperationProcessHelper$")}
	p.cmd.Env = append(os.Environ(), "METIS_COORD_OPERATION_PROCESS_ACTION="+action, "METIS_COORD_OPERATION_PROCESS_ROOT="+f.store.Root(), "METIS_COORD_OPERATION_PROCESS_WORKSPACE="+f.run.Workspace, "METIS_COORD_OPERATION_PROCESS_RUN="+f.run.ID, "METIS_COORD_OPERATION_PROCESS_ITEM="+f.item.ID, "METIS_COORD_OPERATION_PROCESS_WORKER="+f.item.Owner, "METIS_COORD_OPERATION_PROCESS_TOKEN="+f.item.Lease.Token)
	p.cmd.Stderr = &p.stderr
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	p.stdout = bufio.NewReader(stdout)
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.stdin.Close(); _ = p.cmd.Process.Kill(); _ = p.cmd.Wait() })
	readOperationProcessLine(t, p, "ready\n")
	return p
}

func readOperationProcessLine(t *testing.T, p *operationProcess, expected string) {
	t.Helper()
	ready := make(chan string, 1)
	go func() { line, _ := p.stdout.ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != expected {
			_ = p.cmd.Wait()
			t.Fatalf("operation process state = %q, want %q: %s", line, expected, p.stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("operation process did not reach its expected state")
	}
}

func TestOperationPrepareSurvivesWorkerProcessKillAndHoldsItem(t *testing.T) {
	f := newOperationFixture(t, 2)
	var err error
	f.item, err = f.store.Renew(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	p := startOperationProcess(t, f, "hold")
	if _, err := io.WriteString(p.stdin, "go\n"); err != nil {
		t.Fatal(err)
	}
	readOperationProcessLine(t, p, "prepared\n")
	path, err := f.store.runPath(f.run.Workspace, f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var prepared Run
	if err := json.Unmarshal(bytes, &prepared); err != nil || len(prepared.Operations) != 1 || prepared.Operations[0].Status != OperationStarted {
		t.Fatalf("prepare was not durable before the child reached execution: %v, %+v", err, prepared.Operations)
	}
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err == nil {
		t.Fatal("killed writing process exited successfully")
	}
	f.now = f.now.Add(2 * time.Minute)
	run := f.get(t)
	item := itemByID(t, run, f.item.ID)
	if len(run.Operations) != 1 || run.Operations[0].Status != OperationUncertain || item.Status != ItemFailed || item.Failure == nil || item.Failure.Code != FailureOperationUncertain || item.Failure.AutoRecovered {
		t.Fatalf("killed writing process allowed unsafe automatic replay: operations=%+v item=%+v", run.Operations, item)
	}
	if _, err := f.store.ClaimNext(f.run.Workspace, f.run.ID, ClaimOptions{Worker: "replacement"}); !errors.Is(err, ErrNoReadyWork) {
		t.Fatalf("replacement worker claimed an uncertain remote write: %v", err)
	}
}

func TestOperationSeparateWorkerProcessesPrepareExactlyOneWrite(t *testing.T) {
	f := newOperationFixture(t, 2)
	var err error
	f.item, err = f.store.Renew(f.run.Workspace, f.run.ID, f.item.ID, f.item.Owner, f.item.Lease.Token, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	contenders := []*operationProcess{startOperationProcess(t, f, "begin"), startOperationProcess(t, f, "begin")}
	for _, p := range contenders {
		if _, err := io.WriteString(p.stdin, "go\n"); err != nil {
			t.Fatal(err)
		}
	}
	started, held := 0, 0
	for _, p := range contenders {
		var result operationProcessResult
		if err := json.NewDecoder(p.stdout).Decode(&result); err != nil {
			t.Fatalf("decode process result: %v", err)
		}
		if err := p.cmd.Wait(); err != nil {
			t.Fatalf("operation process failed: %v: %s", err, p.stderr.String())
		}
		switch {
		case result.Error == "" && result.Operation.Status == OperationStarted:
			started++
		case result.Uncertain && result.Error != "":
			held++
		default:
			t.Fatalf("unexpected concurrent operation result: %+v", result)
		}
	}
	if started != 1 || held != 1 || len(f.get(t).Operations) != 1 {
		t.Fatalf("separate writing processes started=%d held=%d", started, held)
	}
}
