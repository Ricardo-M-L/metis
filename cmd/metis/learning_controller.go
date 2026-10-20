package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

// Only this host command configures checker policy. Model-facing SkillSynth,
// Dream and Workflow save paths merely stage content for a later watch tick.
func cmdLearningController(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis learning configure|policy|watch")
	}
	fs := flag.NewFlagSet("metis learning "+args[0], flag.ContinueOnError)
	var kind, name, suitePath, fixturePath string
	var autoPromote, once bool
	var maxEvaluations int
	var maxTokens int64
	var interval time.Duration
	fs.StringVar(&kind, "kind", "skill", "artifact kind: skill or workflow")
	fs.StringVar(&name, "name", "", "host-configured artifact name")
	fs.StringVar(&suitePath, "suite", "", "host checker JSON frozen at configuration")
	fs.StringVar(&fixturePath, "fixture", "", "optional workflow code fixture JSON frozen at configuration")
	fs.BoolVar(&autoPromote, "auto-promote", false, "activate candidates only after a passing host evaluation")
	fs.IntVar(&maxEvaluations, "max-evaluations", 0, "maximum attempts in this policy revision, including unknown attempts")
	fs.Int64Var(&maxTokens, "max-total-tokens", 0, "evaluation admission allowance, including uncertain reservations")
	fs.BoolVar(&once, "once", false, "attempt at most one new candidate and exit")
	fs.DurationVar(&interval, "interval", 5*time.Second, "idle poll interval")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("unexpected learning controller arguments")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	c := learning.NewController(filepath.Join(config.Home(), "learning-controller"))
	output := func(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
	switch args[0] {
	case "configure":
		if once {
			return errors.New("--once applies only to learning watch")
		}
		suite, _, err := loadLearningSuite(suitePath)
		if err != nil {
			return err
		}
		identity, err := learningControllerIdentity(cfg)
		if err != nil {
			return err
		}
		artifactRoot, err := learningControllerArtifactRoot(cfg, kind)
		if err != nil {
			return err
		}
		var frozenFixture json.RawMessage
		if fixturePath != "" {
			if kind != "workflow" {
				return errors.New("code fixtures apply only to workflow policies")
			}
			data, err := readLearningBounded(fixturePath, workflow.MaxEvaluationFixtureBytes)
			if err != nil {
				return err
			}
			if _, err := decodeWorkflowLearningFixtures(data, suite); err != nil {
				return err
			}
			// JSON RawMessage is compacted by controller persistence. Freeze its
			// canonical encoding now so manual evaluation and watcher digests agree.
			var compact bytes.Buffer
			if err := json.Compact(&compact, data); err != nil {
				return err
			}
			frozenFixture = append(json.RawMessage(nil), compact.Bytes()...)
		}
		p, err := c.Configure(ctx, learning.ControllerPolicySpec{Kind: kind, Name: name, ArtifactRoot: artifactRoot, Suite: suite, WorkflowFixture: frozenFixture, Identity: identity, AutoPromote: autoPromote, MaxEvaluations: maxEvaluations, MaxTotalTokens: maxTokens})
		if err != nil {
			return err
		}
		return output(p)
	case "policy":
		if suitePath != "" || fixturePath != "" || once || autoPromote || maxEvaluations != 0 || maxTokens != 0 {
			return errors.New("learning policy is read-only")
		}
		st, err := c.Read()
		if err != nil {
			return err
		}
		if name == "" {
			return output(st)
		}
		if err := learning.ValidateName(name); err != nil {
			return err
		}
		p, ok := st.Policies[kind+"/"+name]
		if !ok {
			return errors.New("learning controller policy absent")
		}
		return output(p)
	case "watch":
		if suitePath != "" || fixturePath != "" || autoPromote || maxEvaluations != 0 || maxTokens != 0 {
			return errors.New("learning watch consumes frozen policies; configure a policy first")
		}
		if interval < 250*time.Millisecond || interval > time.Hour {
			return errors.New("watch interval must be between 250ms and one hour")
		}
		backend := learningControllerBackend(cfg)
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var result learning.ControllerTick
			var err error
			if name != "" {
				result, err = c.RunPolicyOnce(ctx, backend, kind, name)
			} else {
				result, err = c.RunOnce(ctx, backend)
			}
			if err != nil {
				return err
			}
			if err := output(result); err != nil {
				return err
			}
			if once {
				return nil
			}
			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	default:
		return fmt.Errorf("unknown learning controller command %q", args[0])
	}
}

func learningControllerIdentity(cfg *config.Config) (learning.ControllerIdentity, error) {
	providers, model, err := learningProviderConfig(cfg)
	if err != nil {
		return learning.ControllerIdentity{}, err
	}
	binary, err := os.Executable()
	if err != nil {
		return learning.ControllerIdentity{}, err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return learning.ControllerIdentity{}, err
	}
	content, err := os.ReadFile(binary)
	if err != nil {
		return learning.ControllerIdentity{}, err
	}
	return learning.ControllerIdentity{BinaryPath: binary, BinarySHA256: learning.Digest(content), Provider: providers.Default, Model: model, ProviderConfigSHA256: learningProfileDigest(providers)}, nil
}

func learningControllerBackend(cfg *config.Config) learning.ControllerBackend {
	return learning.ControllerBackend{
		Store: func(p learning.ControllerPolicy) (*learning.Store, error) {
			if p.Kind == "skill" || p.Kind == "workflow" {
				return learning.NewStore(p.ArtifactRoot), nil
			}
			return nil, errors.New("unsupported controller artifact kind")
		},
		Evaluate: func(ctx context.Context, p learning.ControllerPolicy, version string) (learning.Evaluation, error) {
			// Re-load before every paid attempt. A watcher does not continue with a
			// silently changed route merely because it started under an old config.
			current, _, err := config.Load()
			if err != nil {
				return learning.Evaluation{}, err
			}
			identity, err := learningControllerIdentity(current)
			if err != nil {
				return learning.Evaluation{}, err
			}
			if identity != p.Identity {
				return learning.Evaluation{}, errors.New("controller CLI or provider route differs from the configured policy")
			}
			artifactRoot, err := learningControllerArtifactRoot(current, p.Kind)
			if err != nil || artifactRoot != p.ArtifactRoot {
				return learning.Evaluation{}, errors.New("controller artifact root differs from the configured policy")
			}
			var runner learning.Runner
			var evaluator learning.EvaluatorIdentity
			var store *learning.Store
			var fixtures *workflowLearningFixtures
			if p.Kind == "workflow" {
				if len(p.WorkflowFixture) > 0 {
					fixtures, err = decodeWorkflowLearningFixtures(p.WorkflowFixture, p.Suite)
					if err != nil {
						return learning.Evaluation{}, err
					}
				}
				runner, evaluator, err = newWorkflowLearningFixtureQueryRunner(current, p.Name, p.Suite, fixtures)
				store = workflow.NewStore(filepath.Join(current.Session.Dir, "..", "workflows")).LearningStore()
			} else {
				runner, evaluator, err = newLearningQueryRunner(current, p.Name, p.Suite)
				store = learning.NewStore(current.Session.SkillDir)
			}
			if err != nil {
				return learning.Evaluation{}, err
			}
			if evaluator.BinarySHA256 != p.Identity.BinarySHA256 || evaluator.ProfileSHA256 != workflowLearningFixtureProfileDigest(p.Identity.ProviderConfigSHA256, fixtures) || evaluator.Provider != p.Identity.Provider || evaluator.Model != p.Identity.Model {
				return learning.Evaluation{}, errors.New("controller evaluator identity differs from the configured policy")
			}
			validateRunner := evaluator.ValidateInputs
			evaluator.Kind = p.Kind
			evaluator.ValidateInputs = func() error {
				nowCfg, _, err := config.Load()
				if err != nil {
					return err
				}
				now, err := learningControllerIdentity(nowCfg)
				if err != nil {
					return err
				}
				if now != p.Identity {
					return errors.New("controller evaluator changed during evaluation")
				}
				nowRoot, err := learningControllerArtifactRoot(nowCfg, p.Kind)
				if err != nil || nowRoot != p.ArtifactRoot {
					return errors.New("controller artifact root changed during evaluation")
				}
				if validateRunner != nil {
					return validateRunner()
				}
				return nil
			}
			return store.Evaluate(ctx, p.Name, version, p.Suite, evaluator, runner)
		},
	}
}

func learningControllerArtifactRoot(cfg *config.Config, kind string) (string, error) {
	switch kind {
	case "skill":
		return filepath.Abs(cfg.Session.SkillDir)
	case "workflow":
		return filepath.Abs(filepath.Join(cfg.Session.Dir, "..", "workflows"))
	default:
		return "", errors.New("controller kind must be skill or workflow")
	}
}
