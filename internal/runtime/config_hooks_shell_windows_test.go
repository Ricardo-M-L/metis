//go:build windows

package runtime

import (
	"reflect"
	"testing"
)

func TestHookShellCommandWindows(t *testing.T) {
	name, args := hookShellCommand(`echo ok`)
	if name != "cmd.exe" || !reflect.DeepEqual(args, []string{"/S", "/C", `echo ok`}) {
		t.Fatalf("hookShellCommand = (%q, %#v)", name, args)
	}
}
