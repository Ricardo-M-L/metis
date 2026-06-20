package bash

import (
	"sync"
	"testing"
)

// cappedWriter is shared as both Stdout and Stderr of an exec.Cmd, which
// os/exec drives from two concurrent copier goroutines. This reproduces
// that concurrency; run with -race to confirm Write is now synchronized.
func TestCappedWriter_ConcurrentWriteNoRace(t *testing.T) {
	c := newCappedWriter(1024)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 2000; j++ {
				_, _ = c.Write([]byte("a chunk of command output\n"))
			}
		}()
	}
	wg.Wait()
	_ = c.preview()
}
