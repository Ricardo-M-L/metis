package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/sandbox"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/Ricardo-M-L/metis/internal/workflow"
	"github.com/google/uuid"
)

func newWorkflowLearningQueryRunner(cfg *config.Config, name string, suite learning.Suite) (learning.Runner, learning.EvaluatorIdentity, error) {
	return newWorkflowLearningFixtureQueryRunner(cfg, name, suite, nil)
}

func newWorkflowLearningFixtureQueryRunner(cfg *config.Config, name string, suite learning.Suite, fixtures *workflowLearningFixtures) (learning.Runner, learning.EvaluatorIdentity, error) {
	if err := learning.ValidateName(name); err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	providers, model, err := learningProviderConfig(cfg)
	if err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	key, err := cfg.ResolveAPIKey(providers.Default)
	if err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	if d := sandbox.Doctor(); !d.Available {
		return nil, learning.EvaluatorIdentity{}, fmt.Errorf("workflow evaluation requires the OS sandbox: %w", d.Err)
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	content, err := os.ReadFile(binary)
	if err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	identity := learning.EvaluatorIdentity{Kind: "workflow", ProfileSHA256: workflowLearningFixtureProfileDigest(learningProfileDigest(providers), fixtures), BinarySHA256: learning.Digest(content), Provider: providers.Default, Model: model}
	identity.ValidateInputs = func() error {
		now, err := os.ReadFile(binary)
		if err != nil {
			return err
		}
		if learning.Digest(now) != identity.BinarySHA256 {
			return errors.New("workflow evaluator CLI changed")
		}
		current, _, err := config.Load()
		if err != nil {
			return err
		}
		profile, currentModel, err := learningProviderConfig(current)
		if err != nil {
			return err
		}
		if profile.Default != identity.Provider || currentModel != identity.Model || workflowLearningFixtureProfileDigest(learningProfileDigest(profile), fixtures) != identity.ProfileSHA256 {
			return errors.New("workflow evaluator provider profile changed")
		}
		return fixtures.ValidateInputs()
	}
	return func(ctx context.Context, v learning.Version, c learning.Case) (learning.CaseResult, error) {
		r := learning.CaseResult{ID: c.ID, ExitCode: -1}
		if v.ID == "" {
			r.Error = "no active workflow baseline"
			return r, nil
		}
		wf, err := workflow.ParseManifest(name, []byte(v.Content))
		if err != nil {
			return r, err
		}
		now, err := os.ReadFile(binary)
		if err != nil || learning.Digest(now) != identity.BinarySHA256 {
			return r, errors.New("workflow evaluator CLI changed")
		}
		base := filepath.Join(config.Home(), ".learning-runs")
		if err := os.MkdirAll(base, 0o700); err != nil {
			return r, err
		}
		private, err := os.MkdirTemp(base, "workflow-query-*")
		if err != nil {
			return r, err
		}
		defer func() { _ = os.RemoveAll(private); _ = os.Remove(base) }()
		// macOS aliases /var and /tmp through /private. The child's Getwd
		// returns the physical path, so pin that same path before comparing it
		// at the tool boundary rather than rejecting an equivalent cwd.
		resolvedPrivate, err := filepath.EvalSymlinks(private)
		if err != nil {
			return r, err
		}
		private = resolvedPrivate
		home := filepath.Join(private, "metis")
		cwd := filepath.Join(private, "workspace")
		sessions := filepath.Join(home, "sessions")
		recipes := filepath.Join(home, "workflows")
		for _, dir := range []string{home, cwd, sessions, recipes, filepath.Join(home, "skills")} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return r, err
			}
		}
		var fixture *workflow.EvaluationFixtureContext
		var fixtureEnvironment []string
		if fixtures != nil {
			if err := fixtures.ValidateInputs(); err != nil {
				return r, err
			}
			spec, ok := fixtures.Fixtures.Cases[c.ID]
			if !ok {
				return r, errors.New("host fixture case absent")
			}
			fixture, err = workflow.PrepareEvaluationFixture(cwd, filepath.Join(private, "fixture-tests"), spec, fixtures.SHA256)
			if err != nil {
				return r, err
			}
			data, err := json.Marshal(fixture)
			if err != nil {
				return r, err
			}
			path := filepath.Join(private, "fixture-context.json")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				return r, err
			}
			fixtureEnvironment = []string{"METIS_LEARNING_FIXTURE_CONTEXT=" + path, "METIS_LEARNING_FIXTURE_CONTEXT_SHA256=" + learning.Digest(data)}
		}
		if err := workflow.ValidateEvaluationFixtureRecipe(wf, fixture); err != nil {
			return r, err
		}
		if err := os.WriteFile(filepath.Join(recipes, name+".json"), []byte(v.Content), 0o600); err != nil {
			return r, err
		}
		child := config.Config{Provider: providers, Session: config.Session{Dir: sessions, SkillDir: filepath.Join(home, "skills"), MaxIterations: 8}, Permission: config.Permission{Mode: "default", Allow: []config.Rule{{Tool: "Bash"}}}, Tools: config.Tools{Allowed: []string{"Workflow"}, Bash: config.ToolBashSettings{Sandbox: config.SandboxBashSettings{Mode: "permissions", Network: "block"}}}}
		var configText bytes.Buffer
		if err := toml.NewEncoder(&configText).Encode(child); err != nil {
			return r, err
		}
		if err := os.WriteFile(filepath.Join(home, "config.toml"), configText.Bytes(), 0o600); err != nil {
			return r, err
		}
		if data, err := os.ReadFile(filepath.Join(config.Home(), "cache", "models.json")); err == nil && len(data) < 32*1024*1024 {
			if err := os.MkdirAll(filepath.Join(home, "cache"), 0o700); err != nil {
				return r, err
			}
			if err := os.WriteFile(filepath.Join(home, "cache", "models.json"), data, 0o600); err != nil {
				return r, err
			}
		}
		sid := uuid.NewString()
		metrics := filepath.Join(private, "metrics.jsonl")
		prompt := "Invoke the Workflow tool exactly once with operation run_named and name " + name + ". The host grades the actual workflow step outputs, not your final answer. Do not use inline steps or any other workflow. Task:\n" + c.Prompt
		caseCtx, cancel := context.WithTimeout(ctx, time.Duration(suite.MaxDurationSeconds)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(caseCtx, binary, "run", "--bare", "--tools", "Workflow", "--no-auth-wizard", "--max-iter", "8", "--session-id", sid, "--metrics-log", metrics, prompt)
		cmd.Dir = cwd
		cmd.Env = append(learningChildEnvironment(os.Environ(), home, key), "METIS_LEARNING_WORKFLOW_NAME="+name, "METIS_LEARNING_WORKFLOW_VERSION="+v.ID, "METIS_LEARNING_WORKFLOW_CWD="+cwd)
		cmd.Env = append(cmd.Env, fixtureEnvironment...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		started := time.Now()
		queryErr := cmd.Run()
		r.DurationMS = time.Since(started).Milliseconds()
		if cmd.ProcessState != nil {
			r.ExitCode = cmd.ProcessState.ExitCode()
		}
		if queryErr != nil {
			r.Error = "real workflow CLI query did not complete successfully"
		}
		r.Usage, r.UsageReported, err = readLearningMetrics(metrics)
		if err != nil {
			return r, err
		}
		evidence, output, err := readWorkflowLearningFixtureTrace(filepath.Join(home, "traces", sid+".jsonl"), name, v.ID, wf, fixture != nil)
		if err != nil {
			return r, err
		}
		evidence.Provider = identity.Provider
		evidence.Model = identity.Model
		evidence.SessionID = sid
		evidence.ManifestSHA256 = v.ID
		r.Evidence = evidence
		r.Output = output
		r.WorkflowInvoked = len(evidence.WorkflowInvocations) > 0
		r.ModelRequests = len(evidence.TokenEventIDs)
		if fixture != nil {
			report, checkErr := fixture.Verify(caseCtx, fixtures.Fixtures.Cases[c.ID])
			if checkErr != nil {
				report.Error = checkErr.Error()
			}
			data, err := json.Marshal(report)
			if err != nil {
				return r, err
			}
			r.Evidence.FixtureSHA256 = fixtures.SHA256
			r.Evidence.HostChecksReport = data
			r.Evidence.HostChecksSHA256 = learning.EvaluationFixtureDigest(data)
			r.Evidence.HostChecksPassed = checkErr == nil && report.TestsPassed && report.ArtifactsPassed
			if checkErr != nil {
				var assertion *workflow.FixtureAssertionFailure
				if !errors.As(checkErr, &assertion) {
					r.Error = "independent host code fixture checks unavailable: " + checkErr.Error()
					r.ExitCode = 1
				}
			}
		}
		st, err := session.NewStore(sessions)
		if err != nil {
			return r, err
		}
		header, _, err := st.Load(sid)
		if err != nil {
			return r, err
		}
		if header.Provider != identity.Provider || header.Model != identity.Model {
			return r, errors.New("workflow evaluation used a different provider/model")
		}
		r.DurationMS = time.Since(started).Milliseconds()
		return r, nil
	}, identity, nil
}

func readWorkflowLearningTrace(path, name, version string, wf workflow.Workflow) (learning.QueryEvidence, string, error) {
	return readWorkflowLearningFixtureTrace(path, name, version, wf, false)
}

func readWorkflowLearningFixtureTrace(path, name, version string, wf workflow.Workflow, fixture bool) (learning.QueryEvidence, string, error) {
	data, err := readLearningBounded(path, 8*1024*1024)
	if err != nil {
		return learning.QueryEvidence{}, "", err
	}
	e := learning.QueryEvidence{TraceSHA256: learning.Digest(data)}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)
	calls := map[string]learning.SkillInvocation{}
	output := ""
	attempts := 0
	for scanner.Scan() {
		var row session.TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return e, output, err
		}
		if row.Kind == "tokens" {
			e.TokenEventIDs = append(e.TokenEventIDs, row.ID)
		}
		if row.Kind == "tool_start" {
			if row.ToolName != "Workflow" {
				return e, output, errors.New("workflow query invoked a different tool")
			}
			attempts++
			var in struct{ Operation, Name string }
			if json.Unmarshal([]byte(row.Text), &in) != nil || in.Operation != "run_named" || in.Name != name || row.TraceCallID == "" {
				return e, output, errors.New("workflow query changed the pinned operation")
			}
			calls[row.TraceCallID] = learning.SkillInvocation{InvocationID: row.TraceInvocationID, CallID: row.TraceCallID, Arguments: json.RawMessage(row.Text), RecipeSHA256: version}
		}
		if row.Kind == "tool_result" && row.ToolName == "Workflow" {
			call, ok := calls[row.TraceCallID]
			if !ok || (row.IsError && !fixture) {
				return e, output, errors.New("workflow step execution failed or has no paired call")
			}
			var record workflow.RunRecord
			if err := json.Unmarshal([]byte(row.Text), &record); err != nil {
				return e, output, errors.New("workflow result lacks structured step evidence")
			}
			if record.Name != name || record.RecipeSHA256 != version || len(record.Steps) != len(wf.Steps) {
				return e, output, errors.New("workflow result version/step binding mismatch")
			}
			var b strings.Builder
			for i, step := range record.Steps {
				if step.Name != wf.Steps[i].Name || step.Command != wf.Steps[i].Command {
					return e, output, errors.New("workflow result changed the exact recipe")
				}
				if (step.Status != workflow.StatusOK || step.ExitCode != 0) && !fixture {
					return e, output, errors.New("workflow result did not execute the exact recipe successfully")
				}
				if fixture && step.Status != workflow.StatusOK && step.Status != workflow.StatusFailed && step.Status != workflow.StatusSkipped {
					return e, output, errors.New("invalid fixture step status")
				}
				b.WriteString(step.Output)
			}
			output = b.String()
			call.Result = row.Text
			call.ResultSHA256 = learning.Digest([]byte(row.Text))
			e.WorkflowInvocations = append(e.WorkflowInvocations, call)
			delete(calls, row.TraceCallID)
		}
	}
	if err := scanner.Err(); err != nil {
		return e, output, err
	}
	if attempts != 1 || len(e.WorkflowInvocations) != 1 || len(calls) != 0 {
		return e, output, errors.New("workflow evaluation requires exactly one completed pinned invocation")
	}
	return e, output, nil
}
