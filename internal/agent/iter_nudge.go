package agent

// iter_nudge.go — soft nudges injected at 50% / 75% / 90% of
// MaxIters so the model paces itself before the hard cap, plus a
// final-summary rescue when the cap exhausts. Mirrors claude-code's
// tokenBudget continuation nudge (query.ts:1308-1355 + tokenBudget.ts)
// — there it's token-based, here we're iter-based because metis
// budgets by iter, not token.
//
// The nudge is fired AT MOST ONCE per threshold per turn. State
// lives on a small in-Loop counter so we don't need a separate
// tracker struct.
//
// Why nudge instead of hard-cap-and-die: in the screenshot from
// 2026-05-14 (`budget exhausted (50 iters, 1 grace used)`) the
// model was doing useful work right up to iter 50; metis killed it
// silently. claude-code's nudge wording is "Stopped at N% of token
// target. Keep working — do not summarize." — they want the model
// to continue. We want the OPPOSITE shape: as iters climb, push
// the model toward closure ("wrap up open threads; don't start new
// explorations") so it spends its last few iters writing the
// answer, not opening another rabbit hole.

import "fmt"

// iterNudgeThreshold is a single firing point: when curIter crosses
// pct% of MaxIters, emit the nudge with the given body.
//
// Ordered list lives in iterNudges; the loop walks it once per
// iteration and fires the highest threshold not yet seen.
type iterNudgeThreshold struct {
	pct  float64 // 0.5 / 0.75 / 0.9
	body string
}

var iterNudges = []iterNudgeThreshold{
	{
		pct: 0.5,
		body: "You've used about half your iteration budget. " +
			"Pace yourself — finish open threads before starting new investigations.",
	},
	{
		pct: 0.75,
		body: "You're at 75% of your iteration budget. " +
			"Wrap up what you have. Don't start new explorations; " +
			"write up the answer based on what you already know.",
	},
	{
		pct: 0.9,
		body: "You're at 90% of your iteration budget. Pick the single most " +
			"important remaining task and finish it now. Then write " +
			"the final answer for the user — even a one-line summary " +
			"is better than running out of budget mid-investigation.\n\n" +
			"If substantial work still remains, your better option is to " +
			"dispatch it: `Agent({subagent_type: \"plan\", prompt: \"<the " +
			"remaining work in one paragraph>\"})` returns a fresh iter " +
			"budget AND a stepped plan you can hand to implementer agents. " +
			"Single-threading the last 10% guarantees the failure mode " +
			"where you stop mid-task with nothing checked in — fan out instead.",
	},
}

// shouldFireNudge returns the threshold body to inject (or "") given
// the current iter, the cap, and which thresholds have already fired
// this turn. When it returns non-empty, the caller marks the index
// fired so the same nudge doesn't fire twice.
//
// Skips when MaxIters <= 0 (treated as "unlimited") or when the
// cap is so small (< 10) that the thresholds would all collapse
// into the same iteration — sub-10-iter caps are typically test
// rigs, not real sessions.
func shouldFireNudge(curIter, maxIters int, fired []bool) (idx int, body string) {
	return shouldFireNudgeWithTokens(curIter, maxIters, 0, fired)
}

// shouldFireNudgeWithTokens is the token-aware variant. cumulativeOutputTokens
// is the total assistant output tokens emitted so far in this Run() — passed
// through so the formatted nudge body can give the model genuine cost context
// ("you've also burned ~12k output tokens"), matching the spirit of
// claude-code's tokenBudget continuation message (constants/prompts.ts:540
// + utils/tokenBudget.ts). Pass 0 if you don't have the counter handy.
func shouldFireNudgeWithTokens(curIter, maxIters, cumulativeOutputTokens int, fired []bool) (idx int, body string) {
	if maxIters < 10 {
		return -1, ""
	}
	ratio := float64(curIter) / float64(maxIters)
	for i, n := range iterNudges {
		if fired[i] {
			continue
		}
		if ratio >= n.pct {
			return i, formatIterNudgeWithTokens(curIter, maxIters, cumulativeOutputTokens, n.body)
		}
	}
	return -1, ""
}

func formatIterNudge(curIter, maxIters int, body string) string {
	return formatIterNudgeWithTokens(curIter, maxIters, 0, body)
}

func formatIterNudgeWithTokens(curIter, maxIters, cumulativeOutputTokens int, body string) string {
	// Only include the token line when we actually have a number to
	// show — most call sites have it from runOutputTokens, but a few
	// older paths (tests, mock loops) still call the zero-token form.
	tokenLine := ""
	if cumulativeOutputTokens > 0 {
		tokenLine = fmt.Sprintf("(you've also generated ~%d output tokens this turn)\n",
			cumulativeOutputTokens)
	}
	return fmt.Sprintf("<system-reminder>\n[iter %d / %d budget check]\n%s%s\n</system-reminder>",
		curIter, maxIters, tokenLine, body)
}

// finalSummaryRescueMessage is injected when the iter cap is fully
// exhausted (after the grace iteration also ran). Instead of
// silently dropping the turn, we give the model one last
// "summary-only" iteration: tools may still be available (we don't
// have a way to forbid them at the schema level mid-turn), but the
// prompt is explicit that the model should write the answer now,
// not start new investigation.
const finalSummaryRescueMessage = `<system-reminder>
You've hit the iteration cap. Write the final answer for the user
right now in 1-3 lines based on what you have. Do NOT start any
new tool calls — that just burns the rescue iteration with no
chance to deliver a result. If you genuinely have nothing useful
to report, say "couldn't complete in budget — here's where I got
stuck: ..." and stop.
</system-reminder>`
