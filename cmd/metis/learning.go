package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/Ricardo-M-L/metis/internal/agent/skills"
	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/llm"
	"github.com/Ricardo-M-L/metis/internal/session"
	"github.com/google/uuid"
)

func cmdLearning(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis learning stage|list|evaluate|promote|rollback --name NAME [--file MANIFEST] [--version SHA] [--suite FILE] [--evaluation ID] [--promote]")
	}
	if args[0] == "configure" || args[0] == "policy" || args[0] == "watch" {
		return cmdLearningController(ctx, args)
	}
	switch args[0] {
	case "quality-configure", "quality", "feedback-bind", "feedback", "quality-resume", "archive", "controller-archive":
		return cmdLearningFeedback(ctx, args)
	}
	fs := flag.NewFlagSet("metis learning "+args[0], flag.ContinueOnError)
	var name, file, version, suitePath, evaluation, kind, fixturePath string
	var promote bool
	fs.StringVar(&name, "name", "", "user skill name")
	fs.StringVar(&kind, "kind", "skill", "skill or workflow")
	fs.StringVar(&file, "file", "", "candidate markdown manifest")
	fs.StringVar(&version, "version", "", "exact content SHA256")
	fs.StringVar(&suitePath, "suite", "", "host JSON suite")
	fs.StringVar(&evaluation, "evaluation", "", "passing evaluation ID")
	fs.StringVar(&fixturePath, "fixture", "", "optional host-owned workflow code fixture suite")
	fs.BoolVar(&promote, "promote", false, "promote after a passing evaluation")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("unexpected learning arguments")
	}
	if kind == "workflow" {
		return cmdLearningWorkflow(ctx, args)
	}
	if kind != "skill" {
		return fmt.Errorf("unsupported learning kind %q", kind)
	}
	if fixturePath != "" {
		return errors.New("--fixture requires --kind workflow")
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	store := learning.NewStore(cfg.Session.SkillDir)
	output := func(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
	switch args[0] {
	case "list":
		if name != "" {
			st, err := store.Read(name)
			if err != nil {
				return err
			}
			return output(st)
		}
		st, err := store.List()
		if err != nil {
			return err
		}
		return output(st)
	case "stage":
		if err := learning.ValidateName(name); err != nil {
			return err
		}
		content, err := readLearningBounded(file, learning.MaxContentBytes)
		if err != nil {
			return err
		}
		if err := skills.ValidateLearningManifest(name, content); err != nil {
			return err
		}
		baseline, err := skills.LearningBaseline(cfg.Session.SkillDir, name)
		if err != nil {
			return err
		}
		v, err := store.Stage(ctx, name, baseline, content, "host-proposal")
		if err != nil {
			return err
		}
		return output(v)
	case "evaluate":
		suite, initialHash, err := loadLearningSuite(suitePath)
		if err != nil {
			return err
		}
		runner, identity, err := newLearningQueryRunner(cfg, name, suite)
		if err != nil {
			return err
		}
		identity.ValidateInputs = func() error {
			_, now, err := loadLearningSuite(suitePath)
			if err != nil {
				return err
			}
			if now != initialHash {
				return errors.New("suite changed")
			}
			binary, err := os.Executable()
			if err != nil {
				return err
			}
			data, err := os.ReadFile(binary)
			if err != nil {
				return err
			}
			if learning.Digest(data) != identity.BinarySHA256 {
				return errors.New("evaluator changed")
			}
			current, _, err := config.Load()
			if err != nil {
				return err
			}
			profile, currentModel, err := learningProviderConfig(current)
			if err != nil {
				return err
			}
			if profile.Default != identity.Provider || currentModel != identity.Model || learningProfileDigest(profile) != identity.ProfileSHA256 {
				return errors.New("provider profile changed")
			}
			return nil
		}
		e, err := store.Evaluate(ctx, name, version, suite, identity, runner)
		if err != nil {
			return err
		}
		if promote && e.Passed {
			if err := store.Promote(ctx, name, version, e.ID); err != nil {
				return err
			}
		}
		if err := output(e); err != nil {
			return err
		}
		if !e.Passed {
			return fmt.Errorf("learning evaluation rejected: %s", e.Reason)
		}
		return nil
	case "promote":
		suite, _, err := loadLearningSuite(suitePath)
		if err != nil {
			return err
		}
		e, err := store.Evaluation(name, evaluation)
		if err != nil {
			return err
		}
		if e.SuiteSHA256 != learning.SuiteDigest(suite) {
			return errors.New("host suite differs from the evaluated suite")
		}
		_, identity, err := newLearningQueryRunner(cfg, name, suite)
		if err != nil {
			return err
		}
		if identity.BinarySHA256 != e.BinarySHA256 || identity.ProfileSHA256 != e.ProfileSHA256 || identity.Provider != e.Provider || identity.Model != e.Model {
			return errors.New("evaluator CLI, provider or model differs from the passing evaluation")
		}
		if err := store.Promote(ctx, name, version, evaluation); err != nil {
			return err
		}
		st, err := store.Read(name)
		if err != nil {
			return err
		}
		return output(st)
	case "rollback":
		if err := store.Rollback(ctx, name, version); err != nil {
			return err
		}
		st, err := store.Read(name)
		if err != nil {
			return err
		}
		return output(st)
	default:
		return fmt.Errorf("unknown learning command %q", args[0])
	}
}

func readLearningBounded(path string, max int) ([]byte, error) {
	if path == "" {
		return nil, errors.New("file path required")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > max {
		return nil, errors.New("learning input exceeds size limit")
	}
	return data, nil
}
func loadLearningSuite(path string) (learning.Suite, string, error) {
	data, err := readLearningBounded(path, 1024*1024)
	if err != nil {
		return learning.Suite{}, "", err
	}
	var s learning.Suite
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&s); err != nil {
		return s, "", err
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return s, "", errors.New("suite must contain one JSON object")
	}
	return s, learning.Digest(data), s.Validate()
}

// Copy only the selected provider. Other secrets and execution policy are not
// inherited by an evaluator query. The target key is a private env input.
func learningProviderConfig(cfg *config.Config) (config.ProviderSet, string, error) {
	name := cfg.Provider.Default
	set := config.ProviderSet{Default: name}
	model := ""
	const keyEnv = "METIS_LEARNING_QUERY_API_KEY"
	switch name {
	case "anthropic":
		p := cfg.Provider.Anthropic
		p.APIKey = ""
		p.APIKeyEnv = keyEnv
		set.Anthropic = p
		model = p.Model
	case "openai":
		p := cfg.Provider.OpenAI
		p.APIKey = ""
		p.APIKeyEnv = keyEnv
		p.HostedTools = nil
		p.ResponsesStateMode = "local"
		set.OpenAI = p
		model = p.Model
	case "gemini":
		p := cfg.Provider.Gemini
		p.APIKey = ""
		p.APIKeyEnv = keyEnv
		set.Gemini = p
		model = p.Model
	default:
		p, ok := cfg.Provider.Custom[name]
		if !ok {
			return set, "", errors.New("learning evaluator requires an API-key provider profile")
		}
		if p.Transport != "anthropic_messages" && p.Transport != "openai_chat" && p.Transport != "openai_responses" && p.Transport != "gemini_native" {
			return set, "", errors.New("learning evaluator requires an HTTP API-key transport")
		}
		p.APIKey = ""
		p.APIKeyEnv = keyEnv
		p.HostedTools = nil
		p.ResponsesStateMode = "local"
		set.Custom = map[string]config.ProviderRaw{name: p}
		model = p.Model
	}
	return set, model, nil
}

func newLearningQueryRunner(cfg *config.Config, name string, suite learning.Suite) (learning.Runner, learning.EvaluatorIdentity, error) {
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
	binary, err := os.Executable()
	if err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	binaryContent, err := os.ReadFile(binary)
	if err != nil {
		return nil, learning.EvaluatorIdentity{}, err
	}
	identity := learning.EvaluatorIdentity{ProfileSHA256: learningProfileDigest(providers), BinarySHA256: learning.Digest(binaryContent), Provider: providers.Default, Model: model}
	return func(ctx context.Context, v learning.Version, c learning.Case) (learning.CaseResult, error) {
		r := learning.CaseResult{ID: c.ID, ExitCode: -1}
		if v.ID != "" {
			if err := skills.ValidateLearningManifest(name, []byte(v.Content)); err != nil {
				return r, err
			}
		}
		privateRoot := filepath.Join(config.Home(), ".learning-runs")
		if err := os.MkdirAll(privateRoot, 0o700); err != nil {
			return r, err
		}
		private, err := os.MkdirTemp(privateRoot, "query-*")
		if err != nil {
			return r, err
		}
		defer func() { _ = os.RemoveAll(private); _ = os.Remove(privateRoot) }()
		home := filepath.Join(private, "metis")
		cwd := filepath.Join(private, "workspace")
		skillsDir := filepath.Join(home, "skills")
		sessions := filepath.Join(home, "sessions")
		for _, dir := range []string{home, cwd, skillsDir, sessions} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return r, err
			}
		}
		if v.ID != "" {
			if err := os.WriteFile(filepath.Join(skillsDir, name+".md"), []byte(v.Content), 0o600); err != nil {
				return r, err
			}
			selected, err := skills.NewLoader(skillsDir, "", nil).Get(name)
			if err != nil {
				return r, err
			}
			if selected == nil || selected.LocalPath != filepath.Join(skillsDir, name+".md") {
				return r, errors.New("evaluation candidate is not the selected user skill")
			}
			selectedContent, err := os.ReadFile(selected.LocalPath)
			if err != nil {
				return r, err
			}
			if learning.Digest(selectedContent) != v.ID {
				return r, errors.New("evaluation selected skill digest mismatch")
			}
		}
		childConfig := config.Config{Provider: providers, Session: config.Session{Dir: sessions, SkillDir: skillsDir, MaxIterations: 8}, Permission: config.Permission{Mode: "default", Allow: []config.Rule{{Tool: "Skill"}}}}
		var configText bytes.Buffer
		if err := toml.NewEncoder(&configText).Encode(childConfig); err != nil {
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
		prompt := "Use the Skill tool with action invoke and name " + name + " before answering. Follow that skill for this task. Do not explain the invocation. Task:\n" + c.Prompt
		caseCtx, cancel := context.WithTimeout(ctx, time.Duration(suite.MaxDurationSeconds)*time.Second)
		defer cancel()
		cmd := exec.CommandContext(caseCtx, binary, "run", "--bare", "--tools", "Skill", "--no-auth-wizard", "--max-iter", "8", "--session-id", sid, "--metrics-log", metrics, prompt)
		cmd.Dir = cwd
		cmd.Env = learningChildEnvironment(os.Environ(), home, key)
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
			r.Error = "real CLI query did not complete successfully"
		}
		usage, reported, err := readLearningMetrics(metrics)
		if err != nil {
			return r, err
		}
		r.Usage = usage
		r.UsageReported = reported
		evidence, err := readLearningTrace(filepath.Join(home, "traces", sid+".jsonl"), name)
		if err != nil {
			return r, err
		}
		evidence.Provider = identity.Provider
		evidence.Model = identity.Model
		evidence.SessionID = sid
		evidence.ManifestSHA256 = v.ID
		r.Evidence = evidence
		r.SkillInvoked = len(evidence.SkillInvocations) > 0
		r.ModelRequests = len(evidence.TokenEventIDs)
		store, err := session.NewStore(sessions)
		if err != nil {
			return r, err
		}
		header, messages, err := store.Load(sid)
		if err != nil {
			return r, err
		}
		if header.Provider != identity.Provider || header.Model != identity.Model {
			return r, errors.New("evaluation query used a different model or provider")
		}
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role != llm.RoleAssistant {
				continue
			}
			var text strings.Builder
			hasTool := false
			for _, b := range messages[i].Content {
				if b.Type == "tool_use" {
					hasTool = true
				}
				if b.Type == "text" {
					text.WriteString(b.Text)
				}
			}
			if !hasTool {
				r.Output = text.String()
				break
			}
		}
		return r, nil
	}, identity, nil
}

func learningProfileDigest(providers config.ProviderSet) string {
	data, _ := json.Marshal(providers)
	return learning.Digest(data)
}

func learningChildEnvironment(parent []string, home, key string) []string {
	var env []string
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if name == "HOME" || strings.HasPrefix(name, "METIS_") || strings.HasPrefix(name, "OTEL_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "HOME="+filepath.Dir(home), "METIS_HOME="+home, "METIS_LEARNING_QUERY_API_KEY="+key, "METIS_LEARNING_EVALUATION=1", "METIS_LEARNING_TRACE_FULL_RESULTS=1", "METIS_AUTO_MEMORY=0", "METIS_NO_UPDATE_CHECK=1", "ENABLE_TOOL_SEARCH=false")
}
func readLearningMetrics(path string) (learning.Usage, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return learning.Usage{}, false, err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	var total learning.Usage
	reported := false
	for scanner.Scan() {
		var row struct {
			Tokens struct {
				In     *int `json:"in"`
				Out    *int `json:"out"`
				Read   *int `json:"cache_read"`
				Create *int `json:"cache_create"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return total, false, err
		}
		if row.Tokens.In == nil || row.Tokens.Out == nil || row.Tokens.Read == nil || row.Tokens.Create == nil {
			return total, false, errors.New("query usage fields absent")
		}
		if *row.Tokens.In < 0 || *row.Tokens.Out < 0 || *row.Tokens.Read < 0 || *row.Tokens.Create < 0 {
			return total, false, errors.New("query usage negative")
		}
		total.Input += *row.Tokens.In
		total.Output += *row.Tokens.Out
		total.CacheRead += *row.Tokens.Read
		total.CacheCreate += *row.Tokens.Create
		reported = true
	}
	return total, reported, scanner.Err()
}
func readLearningTrace(path, name string) (learning.QueryEvidence, error) {
	data, err := readLearningBounded(path, 8*1024*1024)
	if err != nil {
		return learning.QueryEvidence{}, err
	}
	evidence := learning.QueryEvidence{TraceSHA256: learning.Digest(data)}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)
	calls := map[string]learning.SkillInvocation{}
	for scanner.Scan() {
		var e session.TraceEvent
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			return evidence, err
		}
		if e.Kind == "tokens" {
			evidence.TokenEventIDs = append(evidence.TokenEventIDs, e.ID)
		}
		if e.Kind == "tool_start" && e.ToolName == "Skill" {
			var input struct{ Action, Name string }
			if json.Unmarshal([]byte(e.Text), &input) == nil && input.Action == "invoke" && input.Name == name && e.TraceCallID != "" {
				calls[e.TraceCallID] = learning.SkillInvocation{InvocationID: e.TraceInvocationID, CallID: e.TraceCallID, Arguments: json.RawMessage(e.Text)}
			}
		}
		if e.Kind == "tool_result" && e.ToolName == "Skill" && !e.IsError {
			if call, ok := calls[e.TraceCallID]; ok {
				call.Result = e.Text
				call.ResultSHA256 = learning.Digest([]byte(e.Text))
				evidence.SkillInvocations = append(evidence.SkillInvocations, call)
				delete(calls, e.TraceCallID)
			}
		}
	}
	return evidence, scanner.Err()
}
