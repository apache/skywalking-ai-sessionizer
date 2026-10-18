# A Claude Code 2.1.302 session that continues provider threads

One real session of `claude -p` 2.1.302 (entrypoint `sdk-cli`, model `claude-opus-5-5`), captured on
2026-10-18 with `OTEL_LOG_RAW_API_BODIES` set for the process only. Nine turns, each a new process
resuming the session: a long tool output, a subagent, the same message sent twice, a file read, a
`/compact`, and a question after it.

- `bodies/` holds the 28 bodies Claude Code wrote and its `index.jsonl`.
- `projects/-work/` holds the session's main transcript and its subagent's.
- `mtimes.json` holds each body's file time in nanoseconds. The provider collector lands bodies in the
  order of their file times and looks for a response's session among requests near its time, and git
  keeps no file times, so a test sets them from this file.

Every free-text string was replaced by `r:` and the first sixteen hexadecimal characters of its
SHA-256, so equal strings stay equal and an empty string stays empty. What an adapter reads for
structure was kept: every id, link, type, role, time, flag and tool id; the names of built-in tools
where a call or a definition names them; `metadata.user_id` as a JSON string holding `session_id`,
with the device and account ids replaced by made-up values; and the billing header's `key=value;`
form with `cc_prompt_id`, `cc_prev_req`, `cc_prompt_index` and `cc_turn_index` as they were. Paths,
the person's email, organization, connected servers and every instruction were replaced.

asz at commit f6e3274 builds the same conversation from this copy as from the capture it was made
from: the same 126 steps with the same states, flags and dropped parts, the same bodies joined to
the same calls, and the same summary.

What it shows: a process's first request starts a provider thread and holds the whole message list;
every later request of the process continues the thread from the previous response's message id and
holds only the new messages. See [Claude Code](../../../../../docs/en/adapters/claude-code.md).
