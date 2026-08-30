//go:build !windows

package runtime

import (
	"reflect"
	"testing"
)

func TestHookShellCommandUnix(t *testing.T) {
	name, args := hookShellCommand(`printf '%s' ok`)
	if name != "sh" || !reflect.DeepEqual(args, []string{"-c", `printf '%s' ok`}) {
		t.Fatalf("hookShellCommand = (%q, %#v)", name, args)
	}
}
