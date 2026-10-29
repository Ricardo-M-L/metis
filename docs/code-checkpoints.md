# Code checkpoints and multi-project workspaces

Metis has separate checkpoint mechanisms. Session-history checkpoints persist
messages and completed tool results for continuation and recovery. Code
checkpoints capture the working tree before a potentially mutating tool so
`/rewind` can restore code, conversation, or both. A `checkpoint before tool
skipped` warning concerns the latter; it does not replace the tool's result or
mean that session-history persistence failed.

Code checkpoints use a private shadow Git repository. They do not commit to
the project's Git repositories. The working directory may contain several
projects. Existing exclusions and the 1 MiB individual-file limit still apply;
files outside that scope are not protected by code rewind.
The same selected files must be present in the shadow commit, including files
ignored by a project's `.gitignore`. Otherwise an unchanged ignored file can
be misclassified as newly created and incorrectly selected for deletion during
rewind. Force-staging applies only to the private shadow repository.

Code checkpoints run automatically in ordinary project and home sessions; the
Desktop composer has no permanent checkpoint switch. Users choose a time point
through Rewind and restore code, conversation, or both. Each time point reports
whether code restoration is available and, when it is not, why; conversation
restoration remains available. `/checkpoint on`, `/checkpoint off`, and
`/checkpoint status` remain advanced current-session controls for compatibility
and diagnostics. An explicitly saved `off` is respected after upgrade or resume.
The preference is stored under
`checkpoint-controls/sessions/`, keyed by the durable session ID and canonical
workspace path, and verified against both identities when read. CLI and Desktop
opening the same session see the same preference after refresh or resume;
different sessions in one workspace remain independent. Settings changes are
refused while that session is running, without preventing another session from
changing its own preference. The previous workspace-wide preference is retained
on disk but is not broadcast into new or resumed sessions.

A new-chat preview only reads defaults; it never writes a workspace preference.
Advanced controls allocate a durable session before saving a changed preference.
Project workspaces default to enabled, and the bare user home automatically uses
file checkpoints: each direct file tool saves its
target before editing, without recursively scanning the home directory. Repeated
edits and later targets retain their original pre-edit state. Arbitrary shell
commands have no reliable file target in this mode, so the affected turn is
marked incomplete for code rewind; conversation rewind remains available.
Filesystem roots and empty workspaces retain their default-disabled policy.
Existing explicitly scoped CLI commands remain compatible for advanced callers;
Desktop does not offer a project-scope picker.

Neither the CLI footer nor Desktop composer constantly displays checkpoint
status. Rewind shows recovery availability where it is relevant; advanced callers
can still use `/checkpoint status` to inspect the automatic policy.

The shadow repository stores raw file bytes. Its own highest-priority attributes
disable text, encoding, ident and clean-filter transformations, and its local
`core.autocrlf` is false. Project and global Git configuration are not changed.
Existing shadow indexes are re-normalized once under the raw-byte policy before
creating a new baseline. Old commits are not rewritten; earlier missing or
transformed contents cannot be retroactively reconstructed.

The first automatic code baseline has a five-minute budget, including Git's
initial object and index creation for large multi-project directories. Subsequent
snapshots and post-tool scans have a one-minute budget. These limits also
inherit the turn's cancellation and any earlier deadline. Large workspaces
report periodic aggregate progress (stage, file counts, elapsed time); failures
identify the failed stage. No file contents or file names appear in those
aggregate progress messages.
Cancellation is checked between operations; an individual regular-file read
still depends on the operating system's I/O latency, so the budget is not a
hard real-time guarantee.

The scanner validates the directory hierarchy once and performs bounded
parallel file reads. It compares actual bytes and executable modes with the
mirror, writing only changed files. A same-size edit with a preserved timestamp
is still detected. Deletions, file/directory replacements, and interrupted
copies are reconciled before a new snapshot is committed. Cancellation joins
the copy workers before returning.

Every user turn receives a validated pre-edit baseline, including a turn whose
starting files match an existing snapshot. In that case the baseline reuses
the existing commit; it does not create a duplicate commit or attribute the
new turn's edits to an older turn.

If an automatic checkpoint fails, the tool may continue while the turn is
marked as missing complete code protection. The original warning is shown
once for that turn. Code rewind across that gap is refused; conversation-only
rewind remains available. A later complete baseline can protect later edits,
but cannot reconstruct the missing earlier state. Bash remains conservatively
protected because a command's name or apparent read-only intent does not prove
that it cannot modify files.
If changed files cannot all be fingerprinted (for example, a regular file was
replaced by a symlink), the turn is also marked incomplete rather than reporting
a successful full restore of only a subset.
