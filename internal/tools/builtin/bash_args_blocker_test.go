package builtin

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyBashArgsBlocker_BlockedRules(t *testing.T) {
	cases := []struct {
		cmd        string
		wantSubstr string
	}{
		{`go test -exec "rm -rf /"`, "go test"},
		{`go test ./... -exec ./fake`, "go test"},
		{`npm install --global lodash`, "npm install"},
		{`npm install -g typescript`, "npm install"},
		{`npm i --global thing`, "npm i"},
		{`pnpm add --global typescript`, "pnpm add"},
		{`pnpm add -g typescript`, "pnpm add"},
		{`yarn global add typescript`, "yarn global add"},
		{`pip install --user requests`, "pip install"},
		{`pip3 install --user requests`, "pip3 install"},
		{`go install github.com/foo/bar@latest`, "go install"},
		{`cargo install ripgrep`, "cargo install"},
		{`gem install rails`, "gem install"},
		{`brew install go`, "brew install"},
		{`apt install nginx`, "apt install"},
		{`apt-get install nginx`, "apt-get install"},
		{`apk add nginx`, "apk add"},
		{`pacman -S nginx`, "pacman"},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			err := applyBashArgsBlocker(tc.cmd)
			if err == nil {
				t.Fatalf("should have blocked: %s", tc.cmd)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q; got: %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestApplyBashArgsBlocker_AllowedCommands(t *testing.T) {
	// These should all PASS — false-positive guards. The blocker must
	// not interfere with normal development flow.
	cases := []string{
		"ls -la",
		"git status",
		"go test ./...",
		"go test -v ./...",
		"go build ./...",
		"npm test",
		"npm install",        // local install (no --global) is fine
		"npm install lodash", // ditto
		"pnpm install",
		"yarn install",
		"yarn add lodash",
		"pip install requests", // no --user → fine
		"cargo build",
		"gem list",
		"brew list",
		"apt list --installed",       // not "install"
		"echo --global is just text", // --global appears but not as a flag of npm/pnpm
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			if err := applyBashArgsBlocker(c); err != nil {
				t.Errorf("should NOT block: %q (got %v)", c, err)
			}
		})
	}
}

func TestApplyBashArgsBlocker_FlagsWithEqualsSign(t *testing.T) {
	// `--global=true` should match the same rule as `--global`.
	// crush parity — see splitArgsFlags in bash_args_blocker.go.
	if err := applyBashArgsBlocker("npm install --global=true lodash"); err == nil {
		t.Error("--global=true should block same as --global")
	}
}

func TestTokeniseShellCommand(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`go test -exec "rm -rf /"`, []string{"go", "test", "-exec", "rm -rf /"}},
		{`npm install --global lodash`, []string{"npm", "install", "--global", "lodash"}},
		{`echo 'hello world'`, []string{"echo", "hello world"}},
		{``, nil},
		{`  spaces  before  and  after  `, []string{"spaces", "before", "and", "after"}},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := tokeniseShellCommand(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestSplitArgsFlags(t *testing.T) {
	args, flags := splitArgsFlags([]string{"install", "--global", "-g", "lodash"})
	wantArgs := []string{"install", "lodash"}
	wantFlags := []string{"--global", "-g"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Errorf("args: got %v, want %v", args, wantArgs)
	}
	if !reflect.DeepEqual(flags, wantFlags) {
		t.Errorf("flags: got %v, want %v", flags, wantFlags)
	}
}
