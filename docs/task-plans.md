# Work plans within a session

A conversation can contain several separate work plans. TodoWrite and Task*
share the current plan; progress panes and automatic reminders read that plan
instead of merging every task ever created in the conversation.

- A new human request archives a completed plan before starting new work.
  Follow-ups on unfinished work retain the same plan.
- A completely different TodoWrite list starts a new plan automatically.
  `new_plan: true` explicitly starts one, including when step names overlap.
  TaskCreate supports the same flag on the first task of a new plan.
- Replaced plans retain their original task states. Archiving an unfinished
  task does not mark it completed. TodoRead's `include_archived` and TaskList's
  `includeArchived` expose retained history; explicit task IDs remain usable.
- Children freeze the plan they were assigned. Saved child execution identities
  and scheduled wakeups retain that plan across resumption. A late result can
  update its archived plan without making it the current plan again.
- TodoWrite history cards render the input/result of that tool event. Changing
  the current plan, resizing, or reopening a card cannot rewrite its history.

Existing untagged task files remain readable. The first plan mutation records a
legacy scope so that older structured tasks cannot reappear in a later plan.
Session deletion removes the plan history together with its task records.
