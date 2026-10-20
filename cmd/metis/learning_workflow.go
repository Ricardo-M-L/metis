package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
	"github.com/Ricardo-M-L/metis/internal/workflow"
)

func cmdLearningWorkflow(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("workflow learning subcommand required")
	}
	fs := flag.NewFlagSet("metis learning "+args[0], flag.ContinueOnError)
	var name, file, version, suitePath, fixturePath, evaluation, kind string
	var promote bool
	fs.StringVar(&name, "name", "", "workflow name")
	fs.StringVar(&file, "file", "", "JSON workflow recipe")
	fs.StringVar(&version, "version", "", "exact recipe SHA256")
	fs.StringVar(&suitePath, "suite", "", "host JSON suite")
	fs.StringVar(&fixturePath, "fixture", "", "optional host JSON code fixture suite (isolated system Python tests and artifact assertions)")
	fs.StringVar(&evaluation, "evaluation", "", "passing evaluation ID")
	fs.StringVar(&kind, "kind", "workflow", "artifact kind")
	fs.BoolVar(&promote, "promote", false, "promote a passing candidate")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if kind != "workflow" || len(fs.Args()) != 0 {
		return errors.New("invalid workflow learning arguments")
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	wfStore := workflow.NewStore(filepath.Join(cfg.Session.Dir, "..", "workflows"))
	store := wfStore.LearningStore()
	output := func(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
	switch args[0] {
	case "stage":
		content, err := readLearningBounded(file, learning.MaxContentBytes)
		if err != nil {
			return err
		}
		v, err := wfStore.StageManifest(ctx, name, content, "host-workflow-proposal")
		if err != nil {
			return err
		}
		return output(v)
	case "list":
		if name != "" {
			st, err := store.Read(name)
			if err != nil {
				return err
			}
			return output(st)
		}
		states, err := store.List()
		if err != nil {
			return err
		}
		return output(states)
	case "evaluate":
		suite, hash, err := loadLearningSuite(suitePath)
		if err != nil {
			return err
		}
		fixtures, err := loadWorkflowLearningFixtures(fixturePath, suite)
		if err != nil {
			return err
		}
		runner, identity, err := newWorkflowLearningFixtureQueryRunner(cfg, name, suite, fixtures)
		if err != nil {
			return err
		}
		validateRunner := identity.ValidateInputs
		identity.ValidateInputs = func() error {
			_, now, err := loadLearningSuite(suitePath)
			if err != nil {
				return err
			}
			if now != hash {
				return errors.New("host suite changed")
			}
			if validateRunner != nil {
				return validateRunner()
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
			return fmt.Errorf("workflow evaluation rejected: %s", e.Reason)
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
		if e.Kind != "workflow" || e.SuiteSHA256 != learning.SuiteDigest(suite) {
			return errors.New("workflow host suite differs from the evaluated suite")
		}
		fixtures, err := loadWorkflowLearningFixtures(fixturePath, suite)
		if err != nil {
			return err
		}
		_, identity, err := newWorkflowLearningFixtureQueryRunner(cfg, name, suite, fixtures)
		if err != nil {
			return err
		}
		if identity.BinarySHA256 != e.BinarySHA256 || identity.ProfileSHA256 != e.ProfileSHA256 || identity.Provider != e.Provider || identity.Model != e.Model {
			return errors.New("workflow evaluator CLI, provider or model differs from the passing evaluation")
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
		return fmt.Errorf("unknown workflow learning command %q", args[0])
	}
}
