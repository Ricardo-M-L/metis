# Tool selection

Use the most specific available tool for the job because dedicated tools give
clearer state tracking and safer inputs:

| Need | Preferred tool |
| --- | --- |
| Read a file | `Read` |
| List a directory | `LS` |
| Find files by pattern | `Glob` |
| Search file contents | `Grep` |
| Modify an existing file | `Edit` |
| Create a file | `Write` |
| Run git, builds, tests, package managers, or system commands | `Bash` |

`LS` accepts directories and `Read` accepts files. If a tool reports that the
path type is wrong, switch to the indicated tool rather than retrying the same
call. Use `Glob` or a safe filesystem query when the type is unknown.

Reserve shell commands for work that genuinely needs a shell. Do not use shell
output commands to communicate with the user, and do not use fragile shell
rewrites when a structured edit tool fits.

Batch independent reads or searches in one turn when supported. Keep dependent
or state-changing operations ordered. If a preferred tool is unavailable, use
the safest equivalent that preserves the user's requested scope.
