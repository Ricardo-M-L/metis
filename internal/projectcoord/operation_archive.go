package projectcoord

// Completed operations are immutable replay receipts. The hot run keeps only
// bounded recent receipts and every unresolved intent. Archives retain full
// results; pruning them would remove the evidence that prevents duplicate I/O.
//
// A segment contains a small authenticated index followed by bounded records.
// Its index hashes every record and the previous index. run.json commits only
// the chain head and counters. Segments are fsynced before the run's atomic
// rename: a crash before that rename leaves an unreachable segment, never a
// forgotten completed operation. New writers may replace only that uncommitted
// next segment. They never rewrite a committed segment.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

const (
	operationArchiveVersion   = 1
	operationArchiveRecords   = maxOperationRecords / 2
	operationArchiveResults   = maxOperationResultsBytes / 2
	operationArchiveMagic     = "METIS-OPERATIONS-1\n"
	maxOperationArchiveHeader = 512 << 10
	// Two 8192-byte notes can expand to almost 96 KiB through JSON HTML
	// escaping. Include that metadata in the shared writer/reader bound.
	maxOperationArchiveRecordBytes = MaxOperationResultBytes + (128 << 10)
)

// OperationArchive is a fixed-size committed manifest, not an in-memory list
// of historical receipts. Hashes cover the index and each referenced result.
type OperationArchive struct {
	Version     int    `json:"version"`
	Segments    uint64 `json:"segments"`
	Records     uint64 `json:"records"`
	ResultBytes uint64 `json:"result_bytes"`
	Bytes       uint64 `json:"bytes"`
	HeadSHA256  string `json:"head_sha256"`
}

type operationArchiveEntry struct {
	ID          string `json:"id"`
	ItemID      string `json:"item_id"`
	ClaimSHA256 string `json:"claim_sha256"`
	Offset      int64  `json:"offset"`
	Length      int    `json:"length"`
	SHA256      string `json:"sha256"`
}

type operationArchiveHeader struct {
	Version        int                     `json:"version"`
	RunID          string                  `json:"run_id"`
	Sequence       uint64                  `json:"sequence"`
	PreviousSHA256 string                  `json:"previous_sha256,omitempty"`
	ResultBytes    uint64                  `json:"result_bytes"`
	DataBytes      int64                   `json:"data_bytes"`
	Entries        []operationArchiveEntry `json:"entries"`
}

// StorageUsage exposes only safe counters. It never contains tool results,
// worker tokens or provider credentials. Archive retention is intentional:
// removing replay receipts makes the corresponding remote writes unsafe.
type StorageUsage struct {
	HotOperations       int      `json:"hot_operations"`
	HotResultBytes      int64    `json:"hot_result_bytes"`
	PendingOperations   int      `json:"pending_operations"`
	HotRecordLimit      int      `json:"hot_record_limit"`
	HotResultByteLimit  int64    `json:"hot_result_byte_limit"`
	ArchivedSegments    uint64   `json:"archived_segments"`
	ArchivedOperations  uint64   `json:"archived_operations"`
	ArchivedResultBytes uint64   `json:"archived_result_bytes"`
	ArchiveBytes        uint64   `json:"archive_bytes"`
	Warnings            []string `json:"warnings"`
}

func (s *Store) StorageUsage(workspace, runID string) (StorageUsage, error) {
	run, err := s.Get(workspace, runID)
	if err != nil {
		return StorageUsage{}, err
	}
	usage := StorageUsage{HotOperations: len(run.Operations), HotRecordLimit: maxOperationRecords, HotResultByteLimit: maxOperationResultsBytes, Warnings: []string{}}
	for _, op := range run.Operations {
		usage.HotResultBytes += int64(len(op.ResultJSON))
		if op.Status == OperationStarted || op.Status == OperationUncertain {
			usage.PendingOperations++
		}
	}
	if archive := run.OperationArchive; archive != nil {
		usage.ArchivedSegments, usage.ArchivedOperations = archive.Segments, archive.Records
		usage.ArchivedResultBytes, usage.ArchiveBytes = archive.ResultBytes, archive.Bytes
	}
	if usage.HotOperations >= maxOperationRecords*4/5 {
		usage.Warnings = append(usage.Warnings, "operation_hot_capacity: unresolved or retryable operations are approaching the in-flight journal limit; inspect and resolve pending outcomes")
	}
	if usage.HotResultBytes >= maxOperationResultsBytes*4/5 {
		usage.Warnings = append(usage.Warnings, "operation_result_capacity: recent completed results are approaching the hot journal limit")
	}
	if usage.ArchiveBytes >= 1<<30 {
		usage.Warnings = append(usage.Warnings, "operation_archive_growth: replay archive exceeds 1 GiB; provision storage and retain the complete run with its archive")
	}
	return usage, nil
}

// GetOperation can retrieve an archived result without loading other results.
func (s *Store) GetOperation(workspace, runID, id string) (OperationRecord, error) {
	run, err := s.Get(workspace, runID)
	if err != nil {
		return OperationRecord{}, err
	}
	op, err := run.operation(id)
	if err != nil {
		return OperationRecord{}, err
	}
	return cloneOperation(*op), nil
}

// ListOperations explicitly loads the complete history for trusted management
// callers. Normal scheduling and individual replay read indexes and one result.
func (s *Store) ListOperations(workspace, runID string) ([]OperationRecord, error) {
	run, err := s.Get(workspace, runID)
	if err != nil {
		return nil, err
	}
	operations := []OperationRecord{}
	err = s.walkOperationArchive(run.Workspace, run.ID, run.OperationArchive, func(path string, header operationArchiveHeader, dataStart int64) error {
		for _, entry := range header.Entries {
			op, err := readArchivedOperation(path, dataStart, entry)
			if err != nil {
				return err
			}
			if err := validateArchivedOperation(run.ID, op); err != nil {
				return err
			}
			operations = append(operations, op)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	operations = append(operations, cloneOperations(run.Operations)...)
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].StartedAt.Equal(operations[j].StartedAt) {
			return operations[i].ID < operations[j].ID
		}
		return operations[i].StartedAt.Before(operations[j].StartedAt)
	})
	return operations, nil
}

func (s *Store) compactOperations(workspace string, run *Run) (bool, error) {
	resultBytes := 0
	for _, op := range run.Operations {
		resultBytes += len(op.ResultJSON)
	}
	if len(run.Operations) < operationArchiveRecords && resultBytes < operationArchiveResults {
		return false, nil
	}
	var completed, pending []OperationRecord
	for _, op := range run.Operations {
		if op.Status == OperationCompleted {
			completed = append(completed, op)
		} else {
			pending = append(pending, op)
		}
	}
	if len(completed) == 0 {
		return false, nil
	}
	for len(completed) > 0 {
		count, size := 0, 0
		for count < len(completed) && count < operationArchiveRecords {
			next := len(completed[count].ResultJSON)
			if count > 0 && size+next > operationArchiveResults {
				break
			}
			size += next
			count++
		}
		archive, err := s.writeOperationSegment(workspace, *run, completed[:count])
		if err != nil {
			return false, err
		}
		run.OperationArchive = archive
		completed = completed[count:]
	}
	run.Operations = pending
	return true, nil
}

func (s *Store) operationArchivePath(workspace, runID string, sequence uint64) (string, error) {
	path, err := s.runPath(workspace, runID)
	if err != nil {
		return "", err
	}
	return filepath.Join(path+".operations", fmt.Sprintf("segment-%020d.archive", sequence)), nil
}

func hashArchiveBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func validArchiveHash(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && hex.EncodeToString(decoded) == value
}

func (s *Store) writeOperationSegment(workspace string, run Run, operations []OperationRecord) (*OperationArchive, error) {
	archive := OperationArchive{Version: operationArchiveVersion}
	if run.OperationArchive != nil {
		archive = *run.OperationArchive
	}
	if archive.Segments == ^uint64(0) {
		return nil, errors.New("operation archive sequence overflow")
	}
	header := operationArchiveHeader{Version: operationArchiveVersion, RunID: run.ID, Sequence: archive.Segments + 1, PreviousSHA256: archive.HeadSHA256}
	var data bytes.Buffer
	seen := map[string]bool{}
	for _, op := range operations {
		if seen[op.ID] {
			return nil, errors.New("duplicate operation in archive segment")
		}
		seen[op.ID] = true
		if err := validateArchivedOperation(run.ID, op); err != nil {
			return nil, err
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, op.ResultJSON); err != nil {
			return nil, err
		}
		op.ResultJSON = append(json.RawMessage(nil), compact.Bytes()...)
		encoded, err := json.Marshal(op)
		if err != nil {
			return nil, err
		}
		if len(encoded) > maxOperationArchiveRecordBytes {
			return nil, fmt.Errorf("operation %q metadata exceeds the %d-byte archive record limit; the hot journal remains unchanged", op.ID, maxOperationArchiveRecordBytes)
		}
		header.Entries = append(header.Entries, operationArchiveEntry{ID: op.ID, ItemID: op.ItemID, ClaimSHA256: hashArchiveBytes([]byte(op.ClaimToken)), Offset: int64(data.Len()), Length: len(encoded), SHA256: hashArchiveBytes(encoded)})
		_, _ = data.Write(encoded)
		_ = data.WriteByte('\n')
		header.ResultBytes += uint64(len(op.ResultJSON))
	}
	if len(operations) == 0 || len(operations) > maxOperationRecords || header.ResultBytes > maxOperationResultsBytes {
		return nil, errors.New("operation archive segment exceeds its bounds")
	}
	header.DataBytes = int64(data.Len())
	index, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	if len(index) > maxOperationArchiveHeader {
		return nil, errors.New("operation archive index exceeds its bounds")
	}
	prefix := []byte(fmt.Sprintf("%s%08x\n", operationArchiveMagic, len(index)))
	segment := make([]byte, 0, len(prefix)+len(index)+1+data.Len())
	segment = append(segment, prefix...)
	segment = append(segment, index...)
	segment = append(segment, '\n')
	segment = append(segment, data.Bytes()...)
	path, err := s.operationArchivePath(workspace, run.ID, header.Sequence)
	if err != nil {
		return nil, err
	}
	if err := ensureProjectCoordinatorLockDirectory(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := writeOperationArchiveFile(path, segment); err != nil {
		return nil, err
	}
	if ^uint64(0)-archive.Records < uint64(len(operations)) || ^uint64(0)-archive.ResultBytes < header.ResultBytes || ^uint64(0)-archive.Bytes < uint64(len(segment)) {
		return nil, errors.New("operation archive counter overflow")
	}
	archive.Segments = header.Sequence
	archive.Records += uint64(len(operations))
	archive.ResultBytes += header.ResultBytes
	archive.Bytes += uint64(len(segment))
	archive.HeadSHA256 = hashArchiveBytes(index)
	return &archive, nil
}

func writeOperationArchiveFile(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing a non-regular operation archive segment")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".segment-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncOperationArchiveDirectory(filepath.Dir(path))
}

func (s *Store) validateOperationArchive(workspace string, run Run) error {
	if run.OperationArchive == nil {
		return nil
	}
	if run.Version != stateVersion {
		return errors.New("legacy journal cannot contain operation archive references")
	}
	return s.walkOperationArchive(workspace, run.ID, run.OperationArchive, nil)
}

// walkOperationArchive validates the bounded indexes and the whole committed
// chain before treating an absent ID as new. Payloads are read only on demand.
// Missing/corrupt indexes therefore cannot silently turn a replay into a write.
func (s *Store) walkOperationArchive(workspace, runID string, archive *OperationArchive, visit func(string, operationArchiveHeader, int64) error) error {
	if archive == nil {
		return nil
	}
	if archive.Version != operationArchiveVersion || archive.Segments == 0 || archive.Records < archive.Segments || !validArchiveHash(archive.HeadSHA256) {
		return errors.New("invalid operation archive manifest")
	}
	expected := archive.HeadSHA256
	var records, resultBytes, storedBytes uint64
	for sequence := archive.Segments; sequence > 0; sequence-- {
		path, err := s.operationArchivePath(workspace, runID, sequence)
		if err != nil {
			return err
		}
		header, start, size, err := readOperationArchiveHeader(path, runID, sequence, expected)
		if err != nil {
			return fmt.Errorf("read committed operation segment %d: %w", sequence, err)
		}
		records += uint64(len(header.Entries))
		resultBytes += header.ResultBytes
		storedBytes += uint64(size)
		expected = header.PreviousSHA256
		if visit != nil {
			if err := visit(path, header, start); err != nil {
				return err
			}
		}
	}
	if expected != "" || records != archive.Records || resultBytes != archive.ResultBytes || storedBytes != archive.Bytes {
		return errors.New("operation archive chain totals do not match its committed manifest")
	}
	return nil
}

func readOperationArchiveHeader(path, runID string, sequence uint64, expected string) (operationArchiveHeader, int64, int64, error) {
	var header operationArchiveHeader
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return header, 0, 0, err
	}
	if !dir.IsDir() || dir.Mode()&os.ModeSymlink != 0 {
		return header, 0, 0, errors.New("operation archive directory is not a regular directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return header, 0, 0, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return header, 0, 0, errors.New("operation archive segment is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return header, 0, 0, err
	}
	defer file.Close()
	prefix := make([]byte, len(operationArchiveMagic)+9)
	if _, err := io.ReadFull(file, prefix); err != nil {
		return header, 0, 0, err
	}
	if string(prefix[:len(operationArchiveMagic)]) != operationArchiveMagic || prefix[len(prefix)-1] != '\n' {
		return header, 0, 0, errors.New("invalid operation archive prefix")
	}
	length, err := strconv.ParseInt(string(prefix[len(operationArchiveMagic):len(prefix)-1]), 16, 32)
	if err != nil || length < 1 || length > maxOperationArchiveHeader {
		return header, 0, 0, errors.New("invalid operation archive index size")
	}
	index := make([]byte, length+1)
	if _, err := io.ReadFull(file, index); err != nil {
		return header, 0, 0, err
	}
	if index[len(index)-1] != '\n' || hashArchiveBytes(index[:len(index)-1]) != expected {
		return header, 0, 0, errors.New("operation archive index hash mismatch")
	}
	decoder := json.NewDecoder(bytes.NewReader(index[:len(index)-1]))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&header); err != nil {
		return header, 0, 0, err
	}
	if header.Version != operationArchiveVersion || header.RunID != runID || header.Sequence != sequence || len(header.Entries) == 0 || len(header.Entries) > maxOperationRecords || header.ResultBytes > maxOperationResultsBytes || (sequence == 1 && header.PreviousSHA256 != "") || (sequence > 1 && !validArchiveHash(header.PreviousSHA256)) {
		return header, 0, 0, errors.New("invalid operation archive index identity or bounds")
	}
	var offset int64
	seen := map[string]bool{}
	for _, entry := range header.Entries {
		if len(entry.ID) != 67 || entry.ID[:3] != "op-" || !validArchiveHash(entry.ID[3:]) || entry.ItemID == "" || !validArchiveHash(entry.ClaimSHA256) || seen[entry.ID] || !validArchiveHash(entry.SHA256) || entry.Offset != offset || entry.Length < 1 || entry.Length > maxOperationArchiveRecordBytes {
			return header, 0, 0, errors.New("invalid operation archive record index")
		}
		seen[entry.ID] = true
		offset += int64(entry.Length) + 1
	}
	start := int64(len(prefix) + len(index))
	if header.DataBytes != offset || start+offset != info.Size() {
		return header, 0, 0, errors.New("operation archive data length mismatch")
	}
	return header, start, info.Size(), nil
}

func readArchivedOperation(path string, start int64, entry operationArchiveEntry) (OperationRecord, error) {
	var op OperationRecord
	file, err := os.Open(path)
	if err != nil {
		return op, err
	}
	defer file.Close()
	data := make([]byte, entry.Length+1)
	if _, err := file.ReadAt(data, start+entry.Offset); err != nil {
		return op, err
	}
	if data[len(data)-1] != '\n' || hashArchiveBytes(data[:len(data)-1]) != entry.SHA256 {
		return op, errors.New("archived operation result hash mismatch")
	}
	if err := json.Unmarshal(data[:len(data)-1], &op); err != nil {
		return op, err
	}
	if op.ID != entry.ID || op.ItemID != entry.ItemID || hashArchiveBytes([]byte(op.ClaimToken)) != entry.ClaimSHA256 {
		return op, errors.New("archived operation result identity mismatch")
	}
	return op, nil
}

func validateArchivedOperation(runID string, op OperationRecord) error {
	if op.Status != OperationCompleted {
		return errors.New("only completed operations can be archived")
	}
	// Reuse the full hot-ledger validation without requiring the historical
	// item's active claim or loading the rest of the project graph.
	return validateOperations(Run{ID: runID, Items: []WorkItem{{ID: op.ItemID}}, Operations: []OperationRecord{op}})
}

func (s *Store) archivedOperation(workspace, runID string, archive *OperationArchive, id string) (*OperationRecord, error) {
	var found *OperationRecord
	err := s.walkOperationArchive(workspace, runID, archive, func(path string, header operationArchiveHeader, start int64) error {
		for _, entry := range header.Entries {
			if entry.ID != id {
				continue
			}
			if found != nil {
				return errors.New("duplicate archived operation identity")
			}
			op, err := readArchivedOperation(path, start, entry)
			if err != nil {
				return err
			}
			if err := validateArchivedOperation(runID, op); err != nil {
				return err
			}
			found = &op
		}
		return nil
	})
	return found, err
}

func (s *Store) archivedClaimOperation(workspace, runID string, archive *OperationArchive, itemID, token string) (bool, error) {
	found := false
	claimHash := hashArchiveBytes([]byte(token))
	err := s.walkOperationArchive(workspace, runID, archive, func(_ string, header operationArchiveHeader, _ int64) error {
		for _, entry := range header.Entries {
			if entry.ItemID == itemID && entry.ClaimSHA256 == claimHash {
				found = true
			}
		}
		return nil
	})
	return found, err
}
