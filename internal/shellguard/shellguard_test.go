package shellguard

import (
	"strings"
	"testing"
)

func TestCheckBlocksProcessTerminationCommands(t *testing.T) {
	t.Parallel()
	cases := []string{
		`kill -9 123`,
		`/bin/kill -TERM 123`,
		`pkill metis`,
		`/usr/bin/killall -9 metis`,
		`ps aux | awk '{print $2}' | xargs kill -9`,
		`printf '1\n2\n' | xargs -n 1 /bin/kill -TERM`,
		`command kill -9 123`,
		`builtin kill -TERM 123`,
		`exec /bin/kill 123`,
		`env FOO=bar pkill metis`,
		`sudo --non-interactive killall metis`,
		`env -u TOKEN sudo -u root /bin/kill 123`,
		`parallel --jobs 2 kill ::: 123 456`,
		`parallel 'kill {}' ::: 123`,
		`sh -c 'kill -9 123'`,
		`bash -lc 'pkill metis'`,
		`env zsh -c 'killall metis'`,
		`sh -c "echo safe; /bin/kill 123"`,
		`echo before && pkill metis`,
		`echo before || killall metis`,
		`(echo before; kill 123)`,
		`if true; then pkill metis; fi`,
		`for pid in 1 2; do kill "$pid"; done`,
		`echo "pid=$(kill -0 123)"`,
		`timeout 10 kill -9 123`,
		`nice -n 5 /bin/kill 123`,
		`find . -exec kill -9 {} \;`,
		`find . -execdir /bin/kill {} +`,
		`busybox kill -9 123`,
		`toybox kill 123`,
		`eval 'kill -9 123'`,
		`bash -O extglob -c 'kill -9 123'`,
		`cmd.exe /c taskkill /PID 123`,
		`pwsh -Command 'Stop-Process -Id 123'`,
		`pwsh -Command '& { Stop-Process -Id 123 }'`,
		`cmd.exe /c 'if exist marker (taskkill /PID 123)'`,
		`powershell -EncodedCommand Zm9v`,
		`killer=kill; "$killer" -9 123`,
		`bash -c "$payload"`,
		`\kill -9 123`,
		`k\ill -9 123`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			err := Check(command)
			if err == nil {
				t.Fatalf("Check(%q) allowed a process termination command", command)
			}
			if !strings.Contains(err.Error(), "BashKill(job_id)") {
				t.Fatalf("Check(%q) error %q does not direct the model to BashKill(job_id)", command, err)
			}
		})
	}
}

func TestCheckAllowsProcessWordsUsedAsData(t *testing.T) {
	t.Parallel()
	cases := []string{
		`echo 'kill -9 123'`,
		`printf '%s\n' "pkill metis"`,
		`grep -R "killall" .`,
		`rg 'kill -9|pkill|killall' internal`,
		`command -v kill`,
		`command -V pkill`,
		`type killall`,
		`which kill`,
		`ps aux | grep metis`,
		`parallel echo kill ::: one two`,
		`sh -c 'echo "kill -9"'`,
		`docker kill container-name`,
		`systemctl kill example.service`,
		`/tmp/kill-helper --version`,
		`killjoy --version`,
		`exec -a kill echo ok`,
		`sudo -u kill echo ok`,
		`xargs -I kill echo kill`,
		`grep -R '\kill' .`,
		`printf '%s' 'k\ill'`,
		`echo '$((1 + 2))'`,
	}
	for _, command := range cases {
		command := command
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			if err := Check(command); err != nil {
				t.Fatalf("Check(%q) = %v, want allowed text/search use", command, err)
			}
		})
	}
}
