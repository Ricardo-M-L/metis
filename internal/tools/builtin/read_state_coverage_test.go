package builtin

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Ricardo-M-L/metis/internal/permission"
	"github.com/Ricardo-M-L/metis/internal/tools"
)

func TestReadCoverageSequentialVisiblePagesGrantWholeWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paged.txt")
	data := []byte(strings.Repeat("source-data-", 1800))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	state := NewReadFileState()
	gate := permission.New(permission.ModeFullAccess)
	read, write := Read{gate: gate, state: state}, Write{gate: gate, state: state}
	ctx := tools.WithResultProjection(context.Background())
	offset := 0
	for offset < len(data) {
		result, err := read.Execute(ctx, map[string]any{"path": path, "byte_offset": offset, "byte_limit": 8192})
		if err != nil || result.IsError {
			t.Fatalf("Read page: %+v %v", result, err)
		}
		page := result.Presentation["read_page"].(map[string]any)
		next := intArg(page, "end", -1)
		if next <= offset {
			t.Fatalf("Read did not advance: %+v", page)
		}
		if entry, _ := state.Get(path); !entry.IsPartialView {
			t.Fatal("unconfirmed final page granted whole-file Write")
		}
		read.ObserveResultProjection(ctx, result.Presentation, true)
		offset = next
		if entry, _ := state.Get(path); offset < len(data) && !entry.IsPartialView {
			t.Fatal("one confirmed page granted whole-file Write before EOF coverage")
		}
	}
	if entry, _ := state.Get(path); entry.IsPartialView {
		t.Fatal("every byte was confirmed model-visible, but complete page coverage remained partial")
	}
	result, err := write.Execute(context.Background(), map[string]any{"path": path, "content": "authorized replacement"})
	if err != nil || result.IsError {
		t.Fatalf("complete confirmed coverage did not permit whole-file Write: %+v %v", result, err)
	}
}

// Existing RecordPartial never asserted that any exact byte range was visible.
// A generic positive observer receipt cannot manufacture that missing evidence.
func TestReadCoverageGenericPartialCannotBeConfirmedAsFull(t *testing.T) {
	s := NewReadFileState()
	key := filepath.Join(t.TempDir(), "source.txt")
	data := []byte("first\nlast\n")
	revision := s.recordPartialFixed(key, time.Now(), data, 1, 1)
	s.confirmVisible(key, hashBytes(data), revision, true)
	if entry, _ := s.getFixed(key); !entry.IsPartialView {
		t.Fatal("generic RecordPartial was upgraded without source-range evidence")
	}
}

func TestReadCoverageLatestRevisionCannotUseOldReceipt(t *testing.T) {
	s := NewReadFileState()
	key := filepath.Join(t.TempDir(), "source.txt")
	data := []byte("first second")
	stamp := time.Now()
	old := s.recordReadPageFixed(key, stamp, data, 0, len(data), true, true)
	first := s.recordReadPageFixed(key, stamp, data, 0, 6, true, true)
	s.confirmVisible(key, hashBytes(data), old, true)
	if e, _ := s.getFixed(key); !e.IsPartialView {
		t.Fatal("older full receipt upgraded a newer partial snapshot")
	}
	s.confirmVisible(key, hashBytes(data), first, true)
	last := s.recordReadPageFixed(key, stamp, data, 6, len(data), true, true)
	s.confirmVisible(key, hashBytes(data), first, false)
	s.confirmVisible(key, hashBytes(data), old, true)
	if e, _ := s.getFixed(key); !e.IsPartialView {
		t.Fatal("unconfirmed last page gained authority through an older receipt")
	}
	s.confirmVisible(key, hashBytes(data), last, true)
	if e, _ := s.getFixed(key); e.IsPartialView {
		t.Fatal("old negative receipt destroyed previously confirmed first-page coverage")
	}
}

func TestReadCoverageHashChangeAndABADiscardPriorCoverage(t *testing.T) {
	s := NewReadFileState()
	key := filepath.Join(t.TempDir(), "source.txt")
	oldData, newData := []byte("old source"), []byte("new source")
	stamp := time.Now()
	first := s.recordReadPageFixed(key, stamp, oldData, 0, 4, true, false)
	current := s.recordReadPageFixed(key, stamp, newData, 4, len(newData), true, true)
	s.confirmVisible(key, hashBytes(oldData), first, true)
	s.confirmVisible(key, hashBytes(newData), current, true)
	if e, _ := s.getFixed(key); !e.IsPartialView {
		t.Fatal("coverage from another hash filled the new snapshot's missing bytes")
	}
	restored := s.recordReadPageFixed(key, stamp, oldData, 4, len(oldData), true, true)
	s.confirmVisible(key, hashBytes(oldData), first, true)
	s.confirmVisible(key, hashBytes(oldData), restored, true)
	if e, _ := s.getFixed(key); !e.IsPartialView {
		t.Fatal("ABA content change revived old revision coverage")
	}
}

func TestReadCoverageGenericRecordsClearPageEvidence(t *testing.T) {
	s := NewReadFileState()
	key := filepath.Join(t.TempDir(), "source.txt")
	data := []byte("first second")
	stamp := time.Now()
	s.recordReadPageFixed(key, stamp, data, 0, 6, true, false)
	partial := s.recordPartialFixed(key, stamp, data, 1, 1)
	s.confirmVisible(key, hashBytes(data), partial, true)
	last := s.recordReadPageFixed(key, stamp, data, 6, len(data), true, true)
	s.confirmVisible(key, hashBytes(data), last, true)
	if e, _ := s.getFixed(key); !e.IsPartialView {
		t.Fatal("generic partial read retained or fabricated earlier page evidence")
	}
	full := s.recordFixed(key, stamp, data)
	s.confirmVisible(key, hashBytes(data), last, false)
	if e, _ := s.getFixed(key); e.IsPartialView || e.Revision != full {
		t.Fatal("stale page receipt downgraded a trusted later full snapshot")
	}
}

func TestReadCoverageOnlyConfirmedEligibleRangesCount(t *testing.T) {
	for _, test := range []struct {
		name              string
		start, end        int
		eligible, visible bool
	}{{"redacted", 0, 8, false, true}, {"spilled", 0, 8, true, false}, {"invalid-negative", -1, 8, true, true}, {"invalid-beyond-end", 0, 9, true, true}, {"empty-range", 4, 4, true, true}} {
		t.Run(test.name, func(t *testing.T) {
			s := NewReadFileState()
			key := filepath.Join(t.TempDir(), "source.txt")
			data := []byte("contents")
			r := s.recordReadPageFixed(key, time.Now(), data, test.start, test.end, test.eligible, true)
			s.confirmVisible(key, hashBytes(data), r, test.visible)
			if e, _ := s.getFixed(key); !e.IsPartialView {
				t.Fatal("unseen source bytes granted whole-file authority")
			}
		})
	}
}

func TestReadCoverageEmptyFileRequiresConfirmation(t *testing.T) {
	s := NewReadFileState()
	key := filepath.Join(t.TempDir(), "empty.txt")
	r := s.recordReadPageFixed(key, time.Now(), nil, 0, 0, true, true)
	if e, _ := s.getFixed(key); !e.IsPartialView {
		t.Fatal("unconfirmed empty-file snapshot granted Write")
	}
	s.confirmVisible(key, hashBytes(nil), r, true)
	if e, _ := s.getFixed(key); e.IsPartialView {
		t.Fatal("confirmed complete empty-file snapshot remained partial")
	}
}
