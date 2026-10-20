package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Ricardo-M-L/metis/internal/projectcoord"
	"github.com/Ricardo-M-L/metis/pkg/tool"
)

const coordinatorOperationsUsage = "usage: metis coordinator operation list <runID> [--cwd DIR] [--json]; metis coordinator operation resolve <runID> <operationID> --outcome applied|not_applied --note TEXT [--result-file FILE] [--cwd DIR] [--json]"

// cmdProjectCoordinatorOperations exposes resolution only on the trusted CLI.
// Workers receive neither this command nor authority to resolve an uncertain
// side effect. Resolving records evidence; recovering work remains explicit.
func cmdProjectCoordinatorOperations(args []string) error {
	if len(args) == 0 {
		return errors.New(coordinatorOperationsUsage)
	}
	switch args[0] {
	case "list":
		return cmdProjectCoordinatorOperationList(args[1:])
	case "resolve":
		return cmdProjectCoordinatorOperationResolve(args[1:])
	default:
		return errors.New(coordinatorOperationsUsage)
	}
}

// This explicit projection excludes both stored tool results and claim tokens,
// including resolution notes which can contain private remote receipts.
type coordinatorOperationMetadata struct {
	ID                string                       `json:"id"`
	ItemID            string                       `json:"item_id"`
	ToolName          string                       `json:"tool_name"`
	InputSHA256       string                       `json:"input_sha256"`
	Occurrence        int                          `json:"occurrence"`
	Status            projectcoord.OperationStatus `json:"status"`
	Worker            string                       `json:"worker"`
	ExecutionAttempts int                          `json:"execution_attempts"`
	StartedAt         time.Time                    `json:"started_at"`
	UpdatedAt         time.Time                    `json:"updated_at"`
	CompletedAt       time.Time                    `json:"completed_at"`
	ResultStored      bool                         `json:"result_stored"`
	Resolution        string                       `json:"resolution,omitempty"`
	ResolvedAt        time.Time                    `json:"resolved_at"`
}

func coordinatorOperationPublicMetadata(op projectcoord.OperationRecord) coordinatorOperationMetadata {
	occurrence := op.Occurrence
	if occurrence == 0 {
		occurrence = 1 // Legacy operations have the same identity as slot one.
	}
	return coordinatorOperationMetadata{
		ID: op.ID, ItemID: op.ItemID, ToolName: op.ToolName, InputSHA256: op.InputSHA256,
		Occurrence: occurrence, Status: op.Status, Worker: op.Worker, ExecutionAttempts: op.ExecutionAttempts,
		StartedAt: op.StartedAt, UpdatedAt: op.UpdatedAt, CompletedAt: op.CompletedAt,
		ResultStored: len(op.ResultJSON) != 0, Resolution: op.Resolution, ResolvedAt: op.ResolvedAt,
	}
}

func cmdProjectCoordinatorOperationList(args []string) error {
	if len(args) < 1 || !coordinatorOperationIdentifier(args[0]) {
		return errors.New(coordinatorOperationsUsage)
	}
	fs := flag.NewFlagSet("coordinator operation list", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	jsonOut := fs.Bool("json", false, "emit operation metadata without results or claim tokens")
	if err := parseCoordinatorOperationFlags(fs, args[1:]); err != nil {
		return err
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	store := projectCoordinatorStore()
	run, err := store.Get(workspace, args[0])
	if err != nil {
		return err
	}
	records, err := store.ListOperations(workspace, run.ID)
	if err != nil {
		return err
	}
	operations := make([]coordinatorOperationMetadata, 0, len(records))
	for _, op := range records {
		operations = append(operations, coordinatorOperationPublicMetadata(op))
	}
	if *jsonOut {
		return writeCoordinatorJSON(map[string]any{"run_id": run.ID, "operations": operations})
	}
	if len(operations) == 0 {
		_, err = fmt.Fprintln(os.Stdout, "(no writing operations for this project run)")
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, "OPERATION ID                                                         ITEM                 STATUS        OCCURRENCE  TOOL"); err != nil {
		return err
	}
	for _, op := range operations {
		if _, err := fmt.Fprintf(os.Stdout, "%-67s  %-20s  %-12s  %-10d  %s\n", op.ID, oneLine(op.ItemID, 20), op.Status, op.Occurrence, oneLine(op.ToolName, 80)); err != nil {
			return err
		}
	}
	return nil
}

func cmdProjectCoordinatorOperationResolve(args []string) error {
	if len(args) < 2 || !coordinatorOperationIdentifier(args[0]) || !coordinatorOperationIdentifier(args[1]) {
		return errors.New(coordinatorOperationsUsage)
	}
	fs := flag.NewFlagSet("coordinator operation resolve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cwd := fs.String("cwd", "", "project working directory (default: current directory)")
	outcome := fs.String("outcome", "", "confirmed remote outcome: applied or not_applied")
	resultFile := fs.String("result-file", "", "private successful pkg/tool.Result JSON; required for applied")
	note := fs.String("note", "", "required evidence supporting the checked remote outcome")
	jsonOut := fs.Bool("json", false, "emit resolution metadata without results or claim tokens")
	if err := parseCoordinatorOperationFlags(fs, args[2:]); err != nil {
		return err
	}
	if *outcome != "applied" && *outcome != "not_applied" {
		return errors.New("--outcome must be applied or not_applied")
	}
	if strings.TrimSpace(*note) == "" || len(*note) > 8192 || strings.ContainsRune(*note, '\x00') {
		return errors.New("--note must be nonempty, contain no NUL and fit within 8192 bytes")
	}
	var result json.RawMessage
	if *outcome == "applied" {
		if strings.TrimSpace(*resultFile) == "" {
			return errors.New("--result-file is required for an applied resolution")
		}
		var err error
		result, err = readCoordinatorOperationResult(*resultFile)
		if err != nil {
			return err
		}
	} else if *resultFile != "" || operationFlagWasSet(fs, "result-file") {
		return errors.New("--result-file must not be supplied for a not_applied resolution")
	}
	workspace, err := projectCoordinatorWorkspace(*cwd)
	if err != nil {
		return err
	}
	op, err := projectCoordinatorStore().ResolveOperation(workspace, args[0], args[1], *outcome, result, *note)
	if err != nil {
		return err
	}
	if *jsonOut {
		return writeCoordinatorJSON(map[string]any{"run_id": args[0], "operation": coordinatorOperationPublicMetadata(op), "work_recovery_required": true})
	}
	_, err = fmt.Fprintf(os.Stdout, "Resolved %s as %s. Work item %s still requires explicit coordinator recover.\n", op.ID, op.Resolution, op.ItemID)
	return err
}

func coordinatorOperationIdentifier(value string) bool {
	return strings.TrimSpace(value) != "" && !strings.HasPrefix(value, "-")
}

type coordinatorOperationOnceFlag struct {
	value flag.Value
	name  string
	set   bool
}

func (v *coordinatorOperationOnceFlag) String() string {
	// flag.PrintDefaults compares defaults with a reflect-created zero Value.
	// That wrapper has no delegate, even though registered flags always do.
	if v == nil || v.value == nil {
		return ""
	}
	return v.value.String()
}
func (v *coordinatorOperationOnceFlag) IsBoolFlag() bool {
	b, ok := v.value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}
func (v *coordinatorOperationOnceFlag) Set(value string) error {
	if v.set {
		return fmt.Errorf("flag --%s cannot be repeated", v.name)
	}
	v.set = true
	return v.value.Set(value)
}

func parseCoordinatorOperationFlags(fs *flag.FlagSet, args []string) error {
	fs.VisitAll(func(f *flag.Flag) { f.Value = &coordinatorOperationOnceFlag{value: f.Value, name: f.Name} })
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional argument after operation identifiers")
	}
	return nil
}

func operationFlagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) { found = found || f.Name == name })
	return found
}

// Walk every original path component instead of silently following a symlink
// into another directory. Callers may explicitly provide a canonical path.
func coordinatorOperationResultPath(path string) (string, []os.FileInfo, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", nil, err
	}
	abs = filepath.Clean(abs)
	paths := []string{abs}
	for parent := filepath.Dir(abs); ; parent = filepath.Dir(parent) {
		paths = append(paths, parent)
		if filepath.Dir(parent) == parent {
			break
		}
	}
	infos := make([]os.FileInfo, 0, len(paths))
	for i, part := range paths {
		info, err := os.Lstat(part)
		if err != nil {
			return "", nil, fmt.Errorf("inspect operation result path: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || (i > 0 && !info.IsDir()) {
			return "", nil, errors.New("operation result file and all path ancestors must be real paths without symlinks; supply its canonical path")
		}
		infos = append(infos, info)
	}
	if err := validateCoordinatorOperationResultFile(infos[0]); err != nil {
		return "", nil, err
	}
	return abs, infos, nil
}

func validateCoordinatorOperationResultFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm()&^os.FileMode(0600) != 0 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return errors.New("operation result must be a regular private file with permissions 0600 or narrower")
	}
	if info.Size() > projectcoord.MaxOperationResultBytes {
		return fmt.Errorf("operation result exceeds the %d-byte limit", projectcoord.MaxOperationResultBytes)
	}
	return nil
}

func readCoordinatorOperationResult(path string) (json.RawMessage, error) {
	abs, before, err := coordinatorOperationResultPath(path)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, fmt.Errorf("open operation result file: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before[0], opened) {
		return nil, errors.New("operation result file identity changed while opening")
	}
	if err := validateCoordinatorOperationResultFile(opened); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, projectcoord.MaxOperationResultBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read operation result file: %w", err)
	}
	if len(data) > projectcoord.MaxOperationResultBytes {
		return nil, fmt.Errorf("operation result exceeds the %d-byte limit", projectcoord.MaxOperationResultBytes)
	}
	_, after, err := coordinatorOperationResultPath(abs)
	if err != nil || len(before) != len(after) {
		return nil, errors.New("operation result path changed while reading")
	}
	for i := range before {
		if !os.SameFile(before[i], after[i]) {
			return nil, errors.New("operation result path identity changed while reading")
		}
	}
	if before[0].Size() != after[0].Size() || !before[0].ModTime().Equal(after[0].ModTime()) {
		return nil, errors.New("operation result file changed while reading")
	}
	return decodeCoordinatorOperationResult(data)
}

func decodeCoordinatorOperationResult(data []byte) (json.RawMessage, error) {
	if len(data) == 0 || len(data) > projectcoord.MaxOperationResultBytes {
		return nil, errors.New("operation result must be nonempty JSON within the size limit")
	}
	// Check every object, including free-form metadata, before typed decoding.
	// Otherwise two consumers can disagree about a duplicate-key receipt.
	scan := json.NewDecoder(bytes.NewReader(data))
	scan.UseNumber()
	if err := scanCoordinatorOperationJSON(scan, 0); err != nil {
		return nil, err
	}
	if _, err := scan.Token(); err != io.EOF {
		return nil, errors.New("operation result must contain exactly one JSON value")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || fields == nil {
		return nil, errors.New("operation result must be a pkg/tool.Result JSON object")
	}
	for name, raw := range fields {
		switch name {
		case "Output", "Display", "IsError":
			if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil, errors.New("operation result scalar fields cannot be null")
			}
		case "Meta", "Presentation":
		case "Images":
			if err := validateCoordinatorOperationImages(raw); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("operation result contains an unknown field; use pkg/tool.Result field names")
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var result *tool.Result
	if err := decoder.Decode(&result); err != nil || result == nil {
		return nil, errors.New("operation result fields must match pkg/tool.Result types")
	}
	if result.IsError {
		return nil, errors.New("an error tool result cannot resolve an operation as applied")
	}
	canonical, err := json.Marshal(result)
	if err != nil || len(canonical) > projectcoord.MaxOperationResultBytes {
		return nil, errors.New("complete operation result exceeds the JSON size limit")
	}
	return canonical, nil
}

func validateCoordinatorOperationImages(raw json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil // A nil Result.Images slice is encoded as null by Go.
	}
	var images []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &images); err != nil {
		return errors.New("operation result Images must be an array of image objects or null")
	}
	for _, image := range images {
		if image == nil {
			return errors.New("operation result image cannot be null")
		}
		for name, value := range image {
			if name != "MediaType" && name != "Data" {
				return errors.New("operation result image contains an unknown field")
			}
			var text string
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) || json.Unmarshal(value, &text) != nil {
				return errors.New("operation result image fields must be strings")
			}
		}
	}
	return nil
}

func scanCoordinatorOperationJSON(decoder *json.Decoder, depth int) error {
	if depth > 128 {
		return errors.New("operation result JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return errors.New("operation result must be valid JSON")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return errors.New("operation result must be valid JSON")
			}
			if seen[name] {
				return errors.New("operation result must not contain duplicate JSON keys")
			}
			seen[name] = true
			if err := scanCoordinatorOperationJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := scanCoordinatorOperationJSON(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("operation result must be valid JSON")
	}
	end, err := decoder.Token()
	want := json.Delim('}')
	if delim == '[' {
		want = ']'
	}
	if err != nil || end != want {
		return errors.New("operation result must be valid JSON")
	}
	return nil
}
