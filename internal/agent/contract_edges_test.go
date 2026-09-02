package agent

import (
	"strings"
	"testing"

	"github.com/Ricardo-M-L/metis/internal/llm"
)

func TestExtractVerdict_RequiresExactFinalNonEmptyLine(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "pass", body: "checks passed\nVERDICT: PASS", want: "PASS"},
		{name: "fail", body: "checks failed\nVERDICT: FAIL\n", want: "FAIL"},
		{name: "partial with trailing blank lines", body: "limited checks\nVERDICT: PARTIAL\n\n", want: "PARTIAL"},
		{name: "last exact verdict wins", body: "VERDICT: FAIL\nreran checks\nVERDICT: PASS\n", want: "PASS"},
		{name: "passing prefix", body: "VERDICT: PASSING", want: "MISSING"},
		{name: "explanation suffix", body: "VERDICT: PASS - all checks passed", want: "MISSING"},
		{name: "quoted verdict", body: "> VERDICT: PASS", want: "MISSING"},
		{name: "embedded verdict", body: "report says VERDICT: PASS", want: "MISSING"},
		{name: "indented verdict", body: "  VERDICT: PASS", want: "MISSING"},
		{name: "space after verdict", body: "VERDICT: PASS ", want: "MISSING"},
		{name: "tab separator", body: "VERDICT:\tPASS", want: "MISSING"},
		{name: "trailing prose", body: "VERDICT: PASS\nbut one check was skipped", want: "MISSING"},
		{name: "trailing quote fence", body: "```\nVERDICT: PASS\n```", want: "MISSING"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractVerdict(tt.body); got != tt.want {
				t.Fatalf("extractVerdict(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

func TestContract_ErrorToolResultCannotPassVerifier(t *testing.T) {
	var ct contractTracker
	observeIndependentRisk(&ct)
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{{
		Type:       "tool_result",
		ToolResult: "provider failed, cached body follows\nVERDICT: PASS",
		IsError:    true,
	}})

	if ct.lastVerifyVerdict == "PASS" {
		t.Fatal("an errored verifier tool result accepted a PASS body")
	}
	if body := ct.shouldGateEnd("done"); body == "" {
		t.Fatal("an errored verifier tool result released the contract gate")
	}
}

func TestContract_MultipleVerifierResultsAggregateConservatively(t *testing.T) {
	tests := []struct {
		name     string
		results  []llm.ContentBlock
		want     string
		wantGate bool
	}{
		{name: "both pass", results: []llm.ContentBlock{makeToolResult("VERDICT: PASS"), makeToolResult("VERDICT: PASS")}, want: "PASS"},
		{name: "fail then pass", results: []llm.ContentBlock{makeToolResult("VERDICT: FAIL"), makeToolResult("VERDICT: PASS")}, want: "FAIL", wantGate: true},
		{name: "partial then pass", results: []llm.ContentBlock{makeToolResult("VERDICT: PARTIAL"), makeToolResult("VERDICT: PASS")}, want: "PARTIAL", wantGate: true},
		{name: "missing then pass", results: []llm.ContentBlock{makeToolResult("no verdict"), makeToolResult("VERDICT: PASS")}, want: "MISSING", wantGate: true},
		{name: "pass then fail", results: []llm.ContentBlock{makeToolResult("VERDICT: PASS"), makeToolResult("VERDICT: FAIL")}, want: "FAIL", wantGate: true},
		{name: "errored pass then pass", results: []llm.ContentBlock{{Type: "tool_result", ToolResult: "VERDICT: PASS", IsError: true}, makeToolResult("VERDICT: PASS")}, want: "MISSING", wantGate: true},
		{name: "second result missing", results: []llm.ContentBlock{makeToolResult("VERDICT: PASS")}, want: "MISSING", wantGate: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var ct contractTracker
			observeIndependentRisk(&ct)
			uses := []llm.ContentBlock{
				makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
				makeToolUse("Agent", map[string]any{"subagent_type": "verify"}),
			}
			ct.observeToolUses(uses)
			ct.observeToolResults(uses, tt.results)
			if ct.lastVerifyVerdict != tt.want {
				t.Fatalf("aggregated verdict = %q, want %q", ct.lastVerifyVerdict, tt.want)
			}
			if body := ct.shouldGateEnd("done"); (body != "") != tt.wantGate {
				t.Fatalf("gate presence = %t, want %t; body=%q", body != "", tt.wantGate, body)
			}
		})
	}
}

func TestContract_NonPassVerdictSurvivesRiskDropAfterValidation(t *testing.T) {
	var ct contractTracker
	for _, path := range []string{"internal/a.go", "internal/a.go", "internal/b.go"} {
		ct.observeToolUses([]llm.ContentBlock{makeToolUse("Edit", map[string]any{"file_path": path})})
	}
	if got := ct.riskScore(); got != contractIndependentRiskThreshold {
		t.Fatalf("test premise risk=%d, want %d", got, contractIndependentRiskThreshold)
	}
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, []llm.ContentBlock{makeToolResult("VERDICT: FAIL")})
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Bash", map[string]any{"command": "go test ./internal/agent"})})
	if got := ct.riskScore(); got != contractIndependentRiskThreshold-1 {
		t.Fatalf("test premise risk after validation=%d, want %d", got, contractIndependentRiskThreshold-1)
	}
	if body := ct.shouldGateEnd("done"); body == "" {
		t.Fatal("validation lowered risk and bypassed an outstanding non-PASS verdict")
	}
}

func TestContract_BashFileMutationsInvalidatePassAndRequireVerifier(t *testing.T) {
	tests := []struct {
		name    string
		command string
	}{
		{name: "sed in place", command: "sed -i 's/old/new/' internal/a.go"},
		{name: "sed combined flags", command: "sed -Ei.bak 's/old/new/' internal/a.go"},
		{name: "sudo sed in place", command: "sudo -u root sed -i 's/old/new/' internal/a.go"},
		{name: "path write text", command: `python -c 'from pathlib import Path; Path("a.txt").write_text("new")'`},
		{name: "path write text heredoc", command: "python - <<'PY'\nfrom pathlib import Path\nPath('a.txt').write_text('new')\nPY"},
		{name: "path open write", command: `python -c 'from pathlib import Path; Path("a.txt").open("w").write("new")'`},
		{name: "python open write", command: `python -c 'open("a.txt", "w").write("new")'`},
		{name: "versioned python open write", command: `python3.12 -c 'open("a.txt", "w").write("new")'`},
		{name: "node promises write", command: `node -e 'fs.promises.writeFile("a.txt", "new")'`},
		{name: "node require write", command: `node -e 'require("fs").writeFileSync("a.txt", "new")'`},
		{name: "nested shell redirect", command: `bash -c 'printf new > internal/a.go'`},
		{name: "tee", command: "printf '%s\\n' new | tee internal/a.go"},
		{name: "touch", command: "touch internal/new.go"},
		{name: "copy", command: "cp source.go internal/copy.go"},
		{name: "move", command: "mv old.go internal/new.go"},
		{name: "remove", command: "rm internal/old.go"},
		{name: "remove directory", command: "rmdir internal/empty"},
		{name: "unlink", command: "unlink internal/link.go"},
		{name: "patch", command: "patch -p1 < fix.patch"},
		{name: "git apply", command: "git apply fix.patch"},
		{name: "git apply stat and apply", command: "git apply --stat --apply fix.patch"},
		{name: "git apply with cwd", command: "git -C worktree apply fix.patch"},
		{name: "git rm", command: "git rm internal/old.go"},
		{name: "git restore", command: "git restore internal/a.go"},
		{name: "git checkout path", command: "git checkout -- internal/a.go"},
		{name: "git reset hard", command: "git reset --hard HEAD"},
		{name: "git clean", command: "git clean -fd"},
		{name: "git cherry pick", command: "git cherry-pick abc123"},
		{name: "git merge", command: "git merge feature"},
		{name: "git rebase", command: "git rebase main"},
		{name: "git switch", command: "git switch feature"},
		{name: "gofmt write", command: "gofmt -w internal/a.go"},
		{name: "prettier write", command: "prettier --write web/app.js"},
		{name: "eslint fix", command: "eslint --fix web/app.js"},
		{name: "node destructured fs promises write", command: `node -e 'const { writeFile } = require("fs/promises"); writeFile("a.txt", "new")'`},
		{name: "node imported fs promises write", command: `node -e 'import { writeFileSync } from "node:fs"; writeFileSync("a.txt", "new")'`},
		{name: "node append", command: `node -e 'fs.appendFileSync("a.txt", "new")'`},
		{name: "node destructured append", command: `node -e 'const { appendFile } = require("fs/promises"); appendFile("a.txt", "new")'`},
		{name: "command substitution", command: `echo "$(touch generated.txt)"`},
		{name: "backtick substitution", command: "echo `rm generated.txt`"},
		{name: "shell redirect", command: "printf '%s\\n' new > internal/a.go"},
		{name: "combined stream redirect", command: "printf new >& output.log"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := contractTracker{verifyDispatched: true, lastVerifyVerdict: "PASS"}
			ct.observeToolUses([]llm.ContentBlock{makeToolUse("Bash", map[string]any{"command": tt.command})})

			if ct.verifyDispatched || ct.lastVerifyVerdict != "" {
				t.Fatalf("Bash mutation retained stale verification: %+v", ct)
			}
			if !ct.thresholdMet() {
				t.Fatalf("Bash mutation did not reach verifier risk threshold; score=%d", ct.riskScore())
			}
			if !ct.shellMutationAction {
				t.Fatal("Bash file mutation did not set the shell mutation signal")
			}
			if ct.highImpactAction {
				t.Fatal("ordinary Bash file mutation was mislabeled high-impact")
			}
		})
	}
}

func TestContract_ReadOnlyBashDoesNotInvalidatePass(t *testing.T) {
	for _, command := range []string{
		"git diff -- internal/a.go",
		"git status --short",
		"git rm -n internal/a.go",
		"git rm -nq internal/a.go",
		"git restore --help",
		"git checkout",
		"git reset --help",
		"git clean -n",
		"git clean -nd",
		"git cherry-pick --help",
		"git merge --help",
		"git rebase --show-current-patch",
		"git switch --help",
		"rm --help",
		"rmdir --help",
		"unlink --help",
		"gofmt internal/a.go",
		"prettier --check web/app.js",
		"eslint web/app.js",
		"eslint --fix-dry-run web/app.js",
		"sed -n '1,20p' internal/a.go",
		"go test ./internal/agent 2>&1",
		"printf new >&1",
		"printf new >&-",
		"go test ./internal/agent >/dev/null",
		`python -c 'print(2 > 1)'`,
		`python3.12 -c 'print(2 > 1)'`,
		`node -e 'console.log(2 > 1)'`,
		`node -e 'const { readFile } = require("fs/promises"); readFile("a.txt")'`,
		`bash -c 'printf new 2>&1'`,
		`echo "$(git status --short)"`,
		`printf '%s\n' '$(touch not-executed)'`,
		"cat <<<x",
		"((2 > 1))",
		`awk 'BEGIN { print (2 > 1) }'`,
		`printf '%s\n' 'a | tee output.txt'`,
		`rg '\.write_text\(' internal`,
		`printf '%s\n' 'open("a.txt", "w")'`,
		"tee",
		"git apply --check fix.patch",
		"git apply --stat fix.patch",
		"patch --dry-run -p1 < fix.patch",
	} {
		t.Run(command, func(t *testing.T) {
			ct := contractTracker{verifyDispatched: true, lastVerifyVerdict: "PASS"}
			ct.observeToolUses([]llm.ContentBlock{makeToolUse("Bash", map[string]any{"command": command})})
			if !ct.verifyDispatched || ct.lastVerifyVerdict != "PASS" {
				t.Fatalf("read-only Bash command invalidated verification: %+v", ct)
			}
		})
	}
}

func TestContract_HighImpactGitPushUsesCommandPosition(t *testing.T) {
	ct := contractTracker{verifyDispatched: true, lastVerifyVerdict: "PASS"}
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Bash", map[string]any{"command": "git -C . push origin main"})})
	if !ct.highImpactAction || ct.verifyDispatched || ct.lastVerifyVerdict != "" {
		t.Fatalf("git -C push did not invalidate stale PASS as high impact: %+v", ct)
	}

	ct = contractTracker{verifyDispatched: true, lastVerifyVerdict: "PASS"}
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Bash", map[string]any{"command": `printf '%s\n' 'git push'`})})
	if ct.highImpactAction || !ct.verifyDispatched || ct.lastVerifyVerdict != "PASS" {
		t.Fatalf("quoted git-push text was treated as an executed command: %+v", ct)
	}
}

func TestContract_BashMutationReminderIsNotLabeledHighImpact(t *testing.T) {
	var ct contractTracker
	ct.observeToolUses([]llm.ContentBlock{makeToolUse("Bash", map[string]any{"command": "touch generated.txt"})})
	body := ct.shouldFireMidTurnReminder()
	if body == "" {
		t.Fatal("Bash file mutation did not reach the reminder threshold")
	}
	if !strings.Contains(body, "Bash file mutation: true") {
		t.Fatalf("reminder omitted Bash mutation risk: %q", body)
	}
	if !strings.Contains(body, "high-impact action: false") {
		t.Fatalf("reminder mislabeled ordinary file mutation: %q", body)
	}
}

func TestContract_MissingVerifierToolResultCannotReleaseGate(t *testing.T) {
	var ct contractTracker
	observeIndependentRisk(&ct)
	verifyUse := makeToolUse("Agent", map[string]any{"subagent_type": "verify"})
	ct.observeToolUses([]llm.ContentBlock{verifyUse})
	ct.observeToolResults([]llm.ContentBlock{verifyUse}, nil)

	if ct.lastVerifyVerdict != "MISSING" {
		t.Fatalf("missing verifier tool result verdict = %q, want MISSING", ct.lastVerifyVerdict)
	}
	if body := ct.shouldGateEnd("done"); body == "" {
		t.Fatal("missing verifier tool result released the contract gate")
	}
}

func TestContract_ImplementationProfileClassificationIsConservative(t *testing.T) {
	for _, profile := range []string{"explore", "plan", "verify"} {
		t.Run("read-only_"+profile, func(t *testing.T) {
			if isImplementationAgent(profile) {
				t.Fatalf("read-only/verification profile %q counted as implementation", profile)
			}
		})
	}

	for _, profile := range []string{"", "general", "creator", "teammate", "worker", "go-reviewer", "mcp-debugger", "custom-writable-profile"} {
		t.Run("writable_"+profile, func(t *testing.T) {
			if !isImplementationAgent(profile) {
				t.Fatalf("potentially writable profile %q escaped implementation accounting", profile)
			}
		})
	}
}
