package processutil

import (
	"os"
	"testing"
)

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("current process should be alive")
	}
	if Alive(-1) {
		t.Fatal("negative pid should not be alive")
	}
}
