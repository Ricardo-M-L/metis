package workflow

import "testing"

func TestEvaluationRecipeBoundedPrivateCommands(t *testing.T) {
	for _, command := range []string{`printf '{"answer":%s}\n' "$((17 * 19))"`, `printf '%s\n' '{"answer":323}' > answer.json`, `cat answer.json`, `expr 17 '*' 19`, `echo hello`, `true`, `false`} {
		if err := ValidateEvaluationRecipe(Workflow{Steps: []Step{{Name: "case", Command: command}}}); err != nil {
			t.Errorf("safe command %q rejected: %v", command, err)
		}
	}
}
func TestEvaluationRecipeRejectsHostOracleAndExternalEffects(t *testing.T) {
	for _, command := range []string{`cat /tmp/oracle.json`, `cat ../oracle.json`, `cat "$HOME/oracle.json"`, `cat *`, `cat .learning`, `curl http://localhost:8317`, `python3 -c 'print(1)'`, `printf '%s' "$(cat /tmp/oracle)"`, `printf '%s' "$((secret + 1))"`, `printf '%s' "$((1 << 24))"`, `printf '%9999999999s' x`, `printf hi > /tmp/oracle`, `printf hi > ../oracle`, `printf hi > "$file"`, `printf hi 2> out`, `printf hi; cat answer.json`, `printf hi && echo bye`, `PATH=evil printf hi`, `/bin/echo hi`, `echo hi &`, `sh -c 'echo hi'`, `expr /tmp/oracle ':' '.*'`, `printf '%s' ~`, `printf '%s' $'oops'`} {
		if err := ValidateEvaluationRecipe(Workflow{Steps: []Step{{Name: "case", Command: command}}}); err == nil {
			t.Errorf("unsafe command %q accepted", command)
		}
	}
}
