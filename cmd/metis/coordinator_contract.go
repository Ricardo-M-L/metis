package main

import (
	"errors"
	"flag"
	"fmt"
	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	"os"
	"time"
)

func cmdProjectCoordinatorRenew(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: metis coordinator renew <runID> <itemID> --worker NAME --lease-token TOKEN [--lease 15m]")
	}
	fs := flag.NewFlagSet("coordinator renew", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "project working directory")
	worker := fs.String("worker", "", "claiming worker identity")
	token := fs.String("lease-token", "", "claim token returned by claim")
	lease := fs.Duration("lease", 15*time.Minute, "claim lease duration")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().Renew(workspace, args[0], args[1], *worker, *token, *lease)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(item)
	}
	fmt.Fprintf(os.Stdout, "Renewed %s until %s\n", item.ID, item.Lease.ExpiresAt.Format(time.RFC3339))
	return nil
}

func cmdProjectCoordinatorEvent(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: metis coordinator event <runID> --source SOURCE --id EVENT --subject TITLE --prompt TEXT [--agent PROFILE]")
	}
	fs := flag.NewFlagSet("coordinator event", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "project working directory")
	source := fs.String("source", "", "external event source")
	id := fs.String("id", "", "stable event identifier used for deduplication")
	profile := fs.String("agent", "", "saved agent profile responsible for this event")
	subject := fs.String("subject", "", "work title")
	prompt := fs.String("prompt", "", "event-specific worker instruction")
	var deliverables coordinatorStrings
	fs.Var(&deliverables, "deliverable", "required workspace-relative output file")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	item, err := projectCoordinatorStore().EnqueueEvent(workspace, args[0], projectcoord.EventInput{Source: *source, ID: *id, AgentProfile: *profile, Subject: *subject, Prompt: *prompt, Deliverables: deliverables})
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(item)
	}
	fmt.Fprintf(os.Stdout, "Event work %s is %s (agent=%s)\n", item.ID, item.Status, item.AgentProfile)
	return nil
}

func cmdProjectCoordinatorConfigure(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: metis coordinator configure <runID> --check COMMAND [--check COMMAND] [--cwd DIR]")
	}
	fs := flag.NewFlagSet("coordinator configure", flag.ContinueOnError)
	cwd := fs.String("cwd", "", "project working directory")
	var checks coordinatorStrings
	fs.Var(&checks, "check", "host acceptance command; repeat for multiple checks")
	jsonOut := fs.Bool("json", false, "emit machine-readable JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	run, err := projectCoordinatorStore().SetChecks(workspace, args[0], checks)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(run)
	}
	fmt.Fprintf(os.Stdout, "Configured %d host acceptance checks for %s\n", len(run.CheckCommands), run.ID)
	return nil
}
