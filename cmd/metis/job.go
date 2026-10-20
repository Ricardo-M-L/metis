package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/principal"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	rtpkg "github.com/Ricardo-M-L/metis/internal/runtime"
)

func persistentJobStore() *principal.Store {
	return principal.NewStore(filepath.Join(config.Home(), "jobs"))
}

func cmdJob(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(jobHelp)
	}
	switch args[0] {
	case "create":
		return cmdJobCreate(args[1:])
	case "status", "list":
		return cmdJobStatus(args[0], args[1:])
	case "pause":
		return cmdJobLifecycle(args[1:], principal.Paused)
	case "resume":
		return cmdJobLifecycle(args[1:], principal.Active)
	case "retire":
		return cmdJobLifecycle(args[1:], principal.Retired)
	case "run", "rotate":
		return cmdJobRun(ctx, args[1:], args[0] == "rotate")
	default:
		return errors.New(jobHelp)
	}
}

const jobHelp = `usage: metis job create --name NAME --cwd DIR --agent PROFILE [--provider NAME] [--model MODEL] [--max-budget-usd USD] [--max-input-tokens N] [--max-output-tokens N] [--credential-env ENV] [--json]
       metis job list [--json]
       metis job status|pause|resume|retire JOB_ID [--json]
       metis job run JOB_ID --prompt TEXT [--subject TEXT] [--lease 15m] [--json]
       metis job run JOB_ID --run-id RUN_ID [--lease 15m] [--json]
       metis job rotate JOB_ID --prompt TEXT [--subject TEXT] [--json]
Each new assignment owns a separate durable run; the principal, session,
permissions, credential references and lifetime budget remain fixed.
Pause cancels an active assignment and rejects new model dispatches. If the
provider request was interrupted, its unknown consumption stays recorded;
reconcile its receipts with coordinator usage before dispatching after resume.`

func cmdJobCreate(args []string) error {
	fs := flag.NewFlagSet("job create", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "", "independent persistent job name")
	cwd := fs.String("cwd", "", "fixed workspace")
	agent := fs.String("agent", "", "profile to snapshot")
	provider := fs.String("provider", "", "provider profile to bind")
	model := fs.String("model", "", "model to freeze (defaults to profile or provider)")
	maxUSD := fs.Float64("max-budget-usd", 0, "lifetime estimated USD cap for this job")
	maxInput := fs.Int64("max-input-tokens", 0, "lifetime input-token cap, including separately reported cached input")
	maxOutput := fs.Int64("max-output-tokens", 0, "lifetime output-token cap")
	credentialEnv := fs.String("credential-env", "", "credential variable name; must match the selected provider's configured APIKeyEnv")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	*agent = strings.TrimSpace(*agent)
	if *agent == "" {
		return errors.New("job create requires an explicit --agent profile")
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	var definition principal.Definition
	err = inJobWorkspace(workspace, func() error {
		cfg, _, err := config.Load()
		if err != nil {
			return err
		}
		profile, err := rtpkg.LoadAgentProfileForWorkspace(*agent, workspace)
		if err != nil {
			return err
		}
		if profile == nil {
			return errors.New("persistent job profile is unavailable")
		}
		providerName := strings.TrimSpace(*provider)
		if providerName == "" {
			providerName = cfg.Provider.Default
		}
		providerHash, providerModel, apiKeyEnv, err := jobProviderFingerprint(cfg, providerName)
		if err != nil {
			return err
		}
		if *model != "" {
			profile.Model = *model
		} else if profile.Model == "" {
			profile.Model = providerModel
		}
		if profile.PermissionMode == "" {
			profile.PermissionMode = cfg.Permission.Mode
		}
		mode, ok := permission.ParseMode(profile.PermissionMode)
		if !ok {
			return errors.New("persistent job permission mode is invalid")
		}
		profile.PermissionMode = string(mode)
		refs := []principal.CredentialRef{{Kind: "provider_account", Name: providerName}}
		if *credentialEnv != "" {
			if *credentialEnv != apiKeyEnv {
				return errors.New("--credential-env must match the bound provider's api_key_env; it cannot override an unrelated account")
			}
			refs = append(refs, principal.CredentialRef{Kind: "env", Name: *credentialEnv})
		}
		definition = principal.Definition{Workspace: workspace, Provider: providerName, ProviderConfigSHA256: providerHash, Profile: rtpkg.SnapshotAgentProfile(profile), Credentials: refs, Budget: principal.Budget{MaxUSD: *maxUSD, MaxInputTokens: *maxInput, MaxOutputTokens: *maxOutput}}
		return nil
	})
	if err != nil {
		return err
	}
	job, err := persistentJobStore().Create(*name, definition)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(job)
	}
	fmt.Fprintf(os.Stdout, "Created persistent job %s (%s)\nWorkspace: %s\nFrozen model: %s\n", job.ID, job.Name, job.Definition.Workspace, job.Definition.Profile.Model)
	return nil
}

func cmdJobStatus(mode string, args []string) error {
	fs := flag.NewFlagSet("job "+mode, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	id := ""
	if mode == "status" {
		if len(args) == 0 {
			return errors.New("job status requires a job ID")
		}
		id = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if mode == "list" {
		jobs, err := persistentJobStore().List()
		if err != nil {
			return err
		}
		if *jsonOut {
			return writeCoordinatorJSON(map[string]any{"jobs": jobs})
		}
		for _, j := range jobs {
			fmt.Fprintf(os.Stdout, "%s  %s  %s\n", j.ID, j.Status, j.Name)
		}
		return nil
	}
	j, err := persistentJobStore().Get(id)
	if err != nil {
		return err
	}
	if err = syncPersistentJobUsage(projectCoordinatorStore(), j.Binding()); err != nil {
		return err
	}
	j, err = persistentJobStore().Get(id)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(j)
	}
	fmt.Fprintf(os.Stdout, "%s (%s): %s\nRuns: %d, input: %d, output: %d, estimated spend: $%.6f\n", j.ID, j.Name, j.Status, j.RunCount(), j.InputTokens, j.OutputTokens, j.SpentUSD)
	return nil
}

func cmdJobLifecycle(args []string, status principal.Status) error {
	if len(args) == 0 {
		return errors.New("job lifecycle command requires a job ID")
	}
	id := args[0]
	fs := flag.NewFlagSet("job "+string(status), flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	j, err := persistentJobStore().SetStatus(id, status)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(j)
	}
	fmt.Fprintf(os.Stdout, "%s: %s\n", j.ID, j.Status)
	return nil
}

func cmdJobRun(ctx context.Context, args []string, rotate bool) error {
	if len(args) == 0 {
		return errors.New("job run requires a job ID")
	}
	id := args[0]
	fs := flag.NewFlagSet("job run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	prompt := fs.String("prompt", "", "assignment for this job")
	subject := fs.String("subject", "Persistent job assignment", "assignment title")
	runID := fs.String("run-id", "", "resume an already registered run")
	lease := fs.Duration("lease", 15*time.Minute, "worker and principal execution lease")
	jsonOut := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *lease < time.Second || *lease > 24*time.Hour {
		return errors.New("--lease must be between 1s and 24h")
	}
	j, err := persistentJobStore().Get(id)
	if err != nil {
		return err
	}
	if err = syncPersistentJobUsage(projectCoordinatorStore(), j.Binding()); err != nil {
		return err
	}
	j, err = persistentJobStore().Get(id)
	if err != nil {
		return err
	}
	if j.Status != principal.Active {
		return principal.ErrInactive
	}
	if j.UnresolvedRequests > 0 {
		return principal.ErrUsageUnknown
	}
	b := j.Definition.Budget
	if b.MaxUSD > 0 && j.SpentUSD >= b.MaxUSD || b.MaxInputTokens > 0 && j.InputTokens >= b.MaxInputTokens || b.MaxOutputTokens > 0 && j.OutputTokens >= b.MaxOutputTokens {
		return principal.ErrBudget
	}
	if err = inJobWorkspace(j.Definition.Workspace, func() error { return validatePersistentJobRoute(j.Binding()) }); err != nil {
		return err
	}
	store := projectCoordinatorStore()
	if *runID == "" {
		if strings.TrimSpace(*prompt) == "" {
			return errors.New("new job assignment requires --prompt")
		}
		// Archived usage corrections can return an older run to the hot tail.
		// Every unfinished run remains hot, so eligibility must inspect all
		// of them rather than treating slice order as assignment order.
		for _, previous := range j.Runs {
			if previous.Status != "completed" && previous.Status != "cancelled" {
				return fmt.Errorf("previous job run %s is %s; resume it with --run-id before rotating", previous.RunID, previous.Status)
			}
		}
		binding := j.Binding()
		run, err := store.Create(projectcoord.CreateInput{Workspace: j.Definition.Workspace, Goal: *prompt, Items: []projectcoord.WorkItemInput{{ID: "assignment", Phase: projectcoord.PhaseCustom, Subject: *subject, Prompt: *prompt, MaxAttempts: 10, AgentProfile: binding.Definition.Profile.Name, PrincipalBinding: &binding}}})
		if err != nil {
			return err
		}
		if _, err = persistentJobStore().RegisterRun(binding, run.ID); err != nil {
			return err
		}
		*runID = run.ID
	} else {
		if rotate {
			return errors.New("job rotate creates a new assignment; --run-id is only for job run")
		}
		if *prompt != "" {
			return errors.New("--prompt cannot replace an existing frozen run")
		}
		if _, err := persistentJobStore().GetRunUsage(j.ID, *runID); err != nil {
			return errors.New("run does not belong to this persistent job")
		}
		run, err := store.Get(j.Definition.Workspace, *runID)
		if err != nil {
			return err
		}
		for _, item := range run.Items {
			if item.PrincipalBinding == nil || item.PrincipalBinding.JobID != id || item.PrincipalBinding.ConfigSHA256 != j.ConfigSHA256 {
				return errors.New("run contains work not bound to this job definition")
			}
		}
	}
	fmt.Fprintf(os.Stderr, "job %s executing run %s (session %s)\n", id, *runID, j.SessionID)
	workerArgs := []string{*runID, "--cwd", j.Definition.Workspace, "--worker", fmt.Sprintf("job-%s-%d", id, os.Getpid()), "--lease", lease.String(), "--until-idle"}
	if *jsonOut {
		workerArgs = append(workerArgs, "--json")
	}
	err = cmdProjectCoordinatorRun(ctx, workerArgs)
	syncErr := syncPersistentJobUsage(store, j.Binding())
	return errors.Join(err, syncErr)
}

func inJobWorkspace(workspace string, fn func() error) error {
	previous, err := os.Getwd()
	if err != nil {
		return err
	}
	if err = os.Chdir(workspace); err != nil {
		return err
	}
	defer func() { _ = os.Chdir(previous) }()
	return fn()
}

// jobProviderFingerprint covers the selected route and host permission/tool
// policy while removing the credential value before serialization. Rotating
// an API key is allowed; silently switching endpoints or privileges is not.
func jobProviderFingerprint(cfg *config.Config, name string) (digest, model, apiKeyEnv string, err error) {
	var route any
	switch name {
	case "anthropic":
		p := cfg.Provider.Anthropic
		p.APIKey = ""
		route = p
		model = p.Model
		apiKeyEnv = p.APIKeyEnv
	case "openai":
		p := cfg.Provider.OpenAI
		p.APIKey = ""
		route = p
		model = p.Model
		apiKeyEnv = p.APIKeyEnv
	case "openai-codex":
		route = cfg.Provider.OpenAICodex
		model = cfg.Provider.OpenAICodex.Model
	case "gemini":
		p := cfg.Provider.Gemini
		p.APIKey = ""
		route = p
		model = p.Model
		apiKeyEnv = p.APIKeyEnv
	default:
		p, ok := cfg.Provider.Custom[name]
		if !ok {
			return "", "", "", fmt.Errorf("persistent job provider %q is not configured", name)
		}
		p.APIKey = ""
		route = p
		model = p.Model
		apiKeyEnv = p.APIKeyEnv
	}
	data, err := json.Marshal(struct {
		Provider   string
		Route      any
		Permission config.Permission
		Tools      config.Tools
		MCP        config.MCP
	}{name, route, cfg.Permission, cfg.Tools, cfg.MCP})
	if err != nil {
		return "", "", "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), model, apiKeyEnv, nil
}

func validatePersistentJobRoute(binding principal.Binding) error {
	if err := principal.ValidateBinding(&binding, binding.Definition.Workspace); err != nil {
		return err
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	hash, _, apiKeyEnv, err := jobProviderFingerprint(cfg, binding.Definition.Provider)
	if err != nil {
		return err
	}
	if hash != binding.Definition.ProviderConfigSHA256 {
		return errors.New("persistent job provider or host permission/tool policy changed; create a successor job with an explicit new definition")
	}
	for _, ref := range binding.Definition.Credentials {
		if ref.Kind == "env" {
			if ref.Name != apiKeyEnv {
				return errors.New("persistent job credential reference differs from selected provider")
			}
			if os.Getenv(ref.Name) == "" {
				return fmt.Errorf("persistent job credential environment %s is unavailable", ref.Name)
			}
		}
	}
	return nil
}
