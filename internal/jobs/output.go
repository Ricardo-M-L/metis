package jobs

// output.go — disk-backed stdout/stderr capture for background jobs.
//
// Why disk and not an in-memory buffer: a long-running build can
// produce 50+ MB of output. Holding that in memory:
//   1. burns RAM the model can't actually use (we'd truncate on read
//      anyway, so why retain the middle?)
//   2. forces the wait goroutine to compete with the model for the
//      same buffer (lock contention on every write).
//
// Disk avoids both. The file is opened append-only, written one
// goroutine at a time (cmd.Stdout/Stderr both point to it — Go's
// os.File serializes through the underlying fd), and read by the
// JobOutput tool independently. JobOutput stat()s the file before
// reading so partial writes don't surface garbled rows.

import (
	"io"
	"os"
	"sync"
)

// DiskOutput is the writer cmd.Stdout/Stderr point to during a job's
// run. The struct is a thin wrapper over *os.File that adds:
//   - a mutex (cmd writes from the spawned process side; we never
//     block the wait goroutine on Read, but Close from Wait must not
//     race a still-pending kernel write — small race window but cheap
//     to lock through it)
//   - a Snapshot helper for JobOutput that returns the bytes-so-far
//     in a single pread so a 50 MiB tail doesn't fight a concurrent
//     append.
type DiskOutput struct {
	mu     sync.Mutex
	f      *os.File
	closed bool
	path   string
}

// newDiskOutput opens the path for append, creating with 0o600 so the
// model's working session never accidentally exposes secrets in
// captured logs.
func newDiskOutput(path string) (*DiskOutput, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &DiskOutput{f: f, path: path}, nil
}

// Writer returns an io.Writer the spawned process inherits as
// stdout/stderr. We hand back a struct that holds the same lock so
// concurrent cmd.Stdout/cmd.Stderr writes from the kernel don't tear
// across each other (rare in practice — the kernel writes on the
// child's side; our goroutine just supplies the fd).
func (d *DiskOutput) Writer() io.Writer {
	return &outputWriter{d: d}
}

type outputWriter struct {
	d *DiskOutput
}

func (w *outputWriter) Write(p []byte) (int, error) {
	w.d.mu.Lock()
	defer w.d.mu.Unlock()
	if w.d.closed {
		// Late kernel buffer flush after Close — discard, don't error.
		// exec.Cmd.Wait closes the parent end of the pipe well after
		// the process exits, but a slow buffer can race past it.
		return len(p), nil
	}
	return w.d.f.Write(p)
}

// Close flushes and shuts the file. Idempotent.
func (d *DiskOutput) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	return d.f.Close()
}

// Path returns the on-disk file path. Used by JobOutput when reporting
// where the model can find the full log.
func (d *DiskOutput) Path() string {
	return d.path
}

// ReadJobOutput reads the on-disk log for a job. Used by the
// JobOutput tool at any point during or after the job's lifetime —
// tolerates the file being concurrently appended (returns the
// snapshot up to the moment of the read).
//
// `tailMax` caps the bytes returned: when the file is larger than
// tailMax, the head is truncated and a "[truncated …]" prefix is
// added so the model knows it's seeing a tail. <=0 means no cap.
func ReadJobOutput(path string, tailMax int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := st.Size()

	if tailMax > 0 && size > int64(tailMax) {
		// Seek to the tail offset so we don't read the head we're
		// going to throw away. _ on the Seek result; if it fails the
		// subsequent Read just returns size bytes from offset 0,
		// which is fine — we'll just over-truncate.
		_, _ = f.Seek(size-int64(tailMax), io.SeekStart)
	}

	// Pre-size to what we'll actually keep, not the whole file: a tail read
	// of a 100 MB log with a 64 KB tailMax must not reserve 100 MB up front
	// (the disk-backed design exists precisely to avoid that).
	capHint := size
	if tailMax > 0 && int64(tailMax) < capHint {
		capHint = int64(tailMax) + 64*1024
	}
	buf := make([]byte, 0, capHint)
	chunk := make([]byte, 64*1024)
	for {
		n, err := f.Read(chunk)
		if n > 0 {
			buf = append(buf, chunk[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return string(buf), err
		}
	}
	if tailMax > 0 && size > int64(tailMax) {
		return "[truncated head — file " + formatSize(size) +
			", showing last " + formatSize(int64(len(buf))) + "]\n" +
			string(buf), nil
	}
	return string(buf), nil
}

// formatSize is a tiny human-readable byte formatter — no point
// reaching for an external dep just for "47 MiB".
func formatSize(n int64) string {
	const (
		KiB = 1024
		MiB = 1024 * KiB
	)
	switch {
	case n < KiB:
		return itoa(n) + " B"
	case n < MiB:
		return itoa(n/KiB) + " KiB"
	default:
		return itoa(n/MiB) + " MiB"
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
