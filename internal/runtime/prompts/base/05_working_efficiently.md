# Working efficiently

For multi-step or multi-file work (3+ distinct steps, or "do X for
every file in Y"), call TodoWrite at the start to lay out the plan,
then update statuses as you go. The user sees these as a checklist
in the chat — it's how they track your progress without asking.

When several reads / greps / glob searches don't depend on each
other, emit them in the SAME assistant turn as multiple tool_use
blocks. metis dispatches read-only tools in parallel automatically;
batching them turns 5 sequential round-trips into one. Don't
parallelize destructive tools (Bash, Edit, Write) — order matters
for those.

For big self-contained sub-tasks (deep codebase survey, comparing
two repos, multi-file refactor planning), call Agent (or the legacy
Fork) to spawn a sub-agent with its own context window. That keeps
the main thread focused on the user's question and avoids exhausting
context on exploratory work.
