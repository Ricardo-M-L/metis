package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/config"
	"github.com/Ricardo-M-L/metis/internal/handoff"
)

func openHandoffStore() (*handoff.Store, error) {
	home, err := config.VerifiedHome()
	if err != nil {
		return nil, err
	}
	return handoff.NewStore(filepath.Join(home, "handoff"))
}

// cmdHandoff provides transport inspection and offline sends. It deliberately
// has no manual "ack": only the receiving query's durable history checkpoint
// may acknowledge delivery. Raw claims are useful for worker health checks.
func cmdHandoff(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Print(handoffHelp)
		return nil
	}
	switch args[0] {
	case "send":
		return cmdHandoffSend(ctx, args[1:])
	case "list":
		return cmdHandoffList(ctx, args[1:])
	case "status":
		if len(args) != 2 {
			return errors.New("usage: metis handoff status <messageID>")
		}
		s, err := openHandoffStore()
		if err != nil {
			return err
		}
		r, err := s.Get(ctx, args[1])
		if err != nil {
			return err
		}
		return writeHandoffJSON(r)
	case "claim":
		return cmdHandoffClaim(ctx, args[1:])
	case "renew", "release":
		return cmdHandoffLease(ctx, args[0], args[1:])
	default:
		return fmt.Errorf("unknown handoff command %q", args[0])
	}
}

func cmdHandoffSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("handoff send", flag.ContinueOnError)
	scope := fs.String("scope", "", "mailbox scope; defaults to jobs for job:<ID>")
	from := fs.String("from", "human", "sender address")
	to := fs.String("to", "", "recipient address; persistent jobs use job:<ID>")
	body := fs.String("body", "", "message body")
	bodyFile := fs.String("body-file", "", "UTF-8 message body file")
	key := fs.String("key", "", "logical idempotency key")
	taskFile := fs.String("task-file", "", "optional structured task JSON")
	_ = fs.Bool("json", true, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("handoff send accepts flags only")
	}
	if *bodyFile != "" {
		if *body != "" {
			return errors.New("provide body or body-file, not both")
		}
		b, err := readHandoffInput(*bodyFile, 128<<10)
		if err != nil {
			return err
		}
		*body = string(b)
	}
	if *scope == "" {
		if strings.HasPrefix(*to, "job:") {
			*scope = "jobs"
		} else {
			return errors.New("--scope is required for a session teammate; persistent jobs use --to job:<ID>")
		}
	}
	if *to == "job:" {
		return errors.New("job ID is required")
	}
	if strings.HasPrefix(*to, "job:") {
		if err := validateJobHandoffRecipient(ctx, *to); err != nil {
			return err
		}
		if *scope != "jobs" {
			return errors.New("persistent job recipients use scope jobs")
		}
	}
	var task *handoff.Task
	if *taskFile != "" {
		b, err := readHandoffInput(*taskFile, 128<<10)
		if err != nil {
			return err
		}
		task = &handoff.Task{}
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(task); err != nil {
			return err
		}
		var extra any
		if err = dec.Decode(&extra); err != io.EOF {
			return errors.New("task file must contain one JSON object")
		}
	}
	s, err := openHandoffStore()
	if err != nil {
		return err
	}
	r, duplicate, err := s.Send(ctx, handoff.SendRequest{Scope: *scope, From: *from, To: *to, Body: *body, Task: task, IdempotencyKey: *key})
	if err != nil {
		return err
	}
	return writeHandoffJSON(struct {
		handoff.Record
		Duplicate bool `json:"duplicate"`
	}{r, duplicate})
}
func cmdHandoffList(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("handoff list", flag.ContinueOnError)
	scope := fs.String("scope", "jobs", "mailbox scope")
	to := fs.String("to", "", "optional recipient")
	_ = fs.Bool("json", true, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("handoff list accepts flags only")
	}
	s, err := openHandoffStore()
	if err != nil {
		return err
	}
	rows, err := s.List(ctx, *scope, *to)
	if err != nil {
		return err
	}
	return writeHandoffJSON(rows)
}
func cmdHandoffClaim(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("handoff claim", flag.ContinueOnError)
	scope := fs.String("scope", "jobs", "mailbox scope")
	to := fs.String("to", "", "recipient address")
	consumer := fs.String("consumer", "", "worker identity")
	lease := fs.Duration("lease", handoff.DefaultLease, "delivery lease, at most 5m")
	_ = fs.Bool("json", true, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("handoff claim accepts flags only")
	}
	s, err := openHandoffStore()
	if err != nil {
		return err
	}
	r, err := s.Claim(ctx, *scope, *to, *consumer, *lease)
	if err != nil {
		return err
	}
	return writeHandoffJSON(r)
}
func cmdHandoffLease(ctx context.Context, op string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: metis handoff %s <messageID> --token TOKEN [--lease 30s]", op)
	}
	id := args[0]
	fs := flag.NewFlagSet("handoff "+op, flag.ContinueOnError)
	token := fs.String("token", "", "claim fencing token")
	lease := fs.Duration("lease", 30*time.Second, "delivery lease, at most 5m")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional handoff lease argument")
	}
	s, err := openHandoffStore()
	if err != nil {
		return err
	}
	if op == "renew" {
		err = s.Renew(ctx, id, *token, *lease)
	} else {
		err = s.Release(ctx, id, *token)
	}
	if err != nil {
		return err
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	return writeHandoffJSON(r)
}
func readHandoffInput(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("handoff input exceeds size limit")
	}
	return b, nil
}
func writeHandoffJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

const handoffHelp = `metis handoff — durable peer and persistent-job mailboxes

  metis handoff send --to job:<ID> --body "<message>" [--key KEY] [--task-file JSON]
  metis handoff send --scope <sessionID> --to <teammate> --body "<message>"
  metis handoff list [--scope jobs] [--to job:<ID>]
  metis handoff status <messageID>
  metis handoff claim --to job:<ID> --consumer NAME [--scope jobs] [--lease 30s]
  metis handoff renew <messageID> --token TOKEN [--lease 30s]
  metis handoff release <messageID> --token TOKEN

The receiving query automatically claims mail before its model request and
acknowledges only after saving its transcript. Queued/acknowledged are delivery
states; neither means the requested task completed. Unacknowledged claims are
redelivered after expiry. Explicit job:<ID> addresses remain valid across sessions.

To automatically execute task messages, run a host-configured coordinator serve
with --job <ID> (repeat for each allowed job). A --task-file contains id,
objective, optional input artifacts with SHA-256, output deliverables as relative
paths, and natural-language acceptance requirements. Executable acceptance checks
come only from the host's project configuration, never from a message.

Before admission, handoff status/list shows admission.waiting_reason,
last_checked_at and recovery_hint when a service cannot start a task. Checks with
an unchanged reason are persisted at most once per minute. Resuming a paused job
or restoring its route lets the service admit the task and clear this queue hold;
checking a hold does not reset lifetime budgets or consume a delivery attempt.

handoff status/list includes a separate business receipt once a service reserves
the task. business.state=completed requires the matching message checkpoint,
finished implementation and a successful host verification. A delivery ACK alone
does not satisfy that contract. Ordinary messages without a task remain notices.
`
