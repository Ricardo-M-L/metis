package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/learning"
)

// These are host-only CLI management commands. No model tool can configure a
// quality threshold, submit verifier evidence, or resume a guarded deployment.
func cmdLearningFeedback(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: metis learning quality-configure|quality|feedback-bind|feedback|quality-resume|archive")
	}
	fs := flag.NewFlagSet("metis learning "+args[0], flag.ContinueOnError)
	var name, kind, action, verifier, outcomeID, file, version, note string
	var window, minimum int
	var failureRate float64
	var epoch uint64
	fs.StringVar(&name, "name", "", "artifact name")
	fs.StringVar(&kind, "kind", "skill", "skill or workflow")
	fs.IntVar(&window, "window", 10, "host result window (1..256)")
	fs.IntVar(&minimum, "minimum-samples", 5, "minimum host-verified business results")
	fs.Float64Var(&failureRate, "max-failure-rate", .2, "maximum allowed business failure fraction")
	fs.StringVar(&action, "action", "rollback", "rollback or pause")
	fs.StringVar(&verifier, "verifier", "", "fixed host checker identity")
	fs.StringVar(&outcomeID, "outcome", "", "stable host business execution ID")
	fs.StringVar(&file, "file", "", "host receipt JSON {binding,outcome}")
	fs.StringVar(&version, "version", "", "exact reviewed/loaded activation version")
	fs.Uint64Var(&epoch, "epoch", 0, "exact activation epoch")
	fs.StringVar(&note, "note", "", "host review note for resume")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if len(fs.Args()) != 0 {
		return errors.New("unexpected learning quality arguments")
	}
	if args[0] == "controller-archive" {
		a, err := learning.NewController(filepath.Join(config.Home(), "learning-controller")).ArchiveReceipts(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(a)
	}
	if err := learning.ValidateName(name); err != nil {
		return err
	}
	cfg, _, err := config.Load()
	if err != nil {
		return err
	}
	root, err := learningControllerArtifactRoot(cfg, kind)
	if err != nil {
		return err
	}
	store := learning.NewStore(root)
	output := func(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
	switch args[0] {
	case "quality-configure":
		q, err := store.ConfigureQuality(ctx, name, learning.QualityPolicy{Kind: kind, WindowSize: window, MinimumSamples: minimum, MaxFailureRate: failureRate, Action: action, Verifier: verifier})
		if err != nil {
			return err
		}
		return output(q)
	case "quality":
		st, err := store.Read(name)
		if err != nil {
			return err
		}
		return output(struct {
			ActiveVersion   string                 `json:"active_version"`
			ActivationEpoch uint64                 `json:"activation_epoch"`
			Suspended       bool                   `json:"suspended"`
			Quality         *learning.QualityState `json:"quality"`
		}{st.ActiveVersion, st.ActivationEpoch, st.Suspended, st.Quality})
	case "feedback-bind":
		var b learning.OutcomeBinding
		if version != "" || epoch != 0 {
			b, err = store.BindOutcomeForActivation(ctx, name, outcomeID, version, epoch)
		} else {
			b, err = store.BindOutcome(ctx, name, outcomeID)
		}
		if err != nil {
			return err
		}
		return output(b)
	case "feedback":
		data, err := readLearningBounded(file, 16*1024)
		if err != nil {
			return err
		}
		var required struct {
			Outcome map[string]json.RawMessage `json:"outcome"`
		}
		if json.Unmarshal(data, &required) != nil || required.Outcome["passed"] == nil || (string(required.Outcome["passed"]) != "true" && string(required.Outcome["passed"]) != "false") {
			return errors.New("host feedback requires an explicit boolean passed result")
		}
		var receipt struct {
			Binding learning.OutcomeBinding `json:"binding"`
			Outcome learning.HostOutcome    `json:"outcome"`
		}
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&receipt); err != nil {
			return err
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return errors.New("quality feedback requires one JSON receipt")
		}
		d, err := store.RecordOutcome(ctx, name, receipt.Binding, receipt.Outcome)
		if err != nil {
			return err
		}
		return output(d)
	case "quality-resume":
		if err := store.ResumeQualityCurrent(ctx, name, version, epoch, note); err != nil {
			return err
		}
		st, err := store.Read(name)
		if err != nil {
			return err
		}
		return output(st)
	case "archive":
		a, err := store.Archive(ctx, name)
		if err != nil {
			return err
		}
		return output(a)
	default:
		return errors.New("unknown learning quality command")
	}
}
