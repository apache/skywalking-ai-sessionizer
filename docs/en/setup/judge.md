# Judge a Conversation

`asz judge` measures one collected conversation: the targets the person set, whether each was
achieved, what each cost in tokens, money and time, and how much went to avoidable work. The
[Conversation Judge](../concepts-and-designs/conversation-judge.md) page explains what it answers.

What an evaluator reads goes to the model behind it.

## Run it

The conversation must be collected and parsed, as `asz collect` or `asz server` does.

```sh
asz judge 0438c73b-2367-4ed5-9de3-13ef9a17ed01
```

It runs `judge.plan` and prints each target's outcome and cost when the run closes. `-round N` judges
the conversation as it was at round N of its chain. To give every stage to one evaluator, or each stage
to its own:

```sh
asz judge -evaluator codex 0438c73b-2367-4ed5-9de3-13ef9a17ed01
asz judge -plan targets=claude,achievement=codex,subtasks=codex,efficiency=claude 0438c73b-2367-4ed5-9de3-13ef9a17ed01
```

To judge part of a long conversation, give a window. A window takes whole main talks, each with
every sub-task it started: by the time they start, or by their ids.

```sh
asz judge -from 2026-10-19 -to 2026-10-20 0438c73b-2367-4ed5-9de3-13ef9a17ed01
asz judge -from 2026-10-19T14:00 0438c73b-2367-4ed5-9de3-13ef9a17ed01
asz judge -from-id talk/main/3f1c2a9e-5b7d-4c11-9e2a-6d0b8f4a1c3e -to-id talk/main/9a07e4d2-8c1b-4f6a-b3e5-2d7c9f0a4b18 0438c73b-2367-4ed5-9de3-13ef9a17ed01
```

`-from` takes the talks that start at that time or later, and `-to` those that start before it. A time
is RFC 3339, or a date with or without a time of day, in the local time zone. `-from-id` and `-to-id`
name the first and the last talk, both included, by their whole identifiers, as `asz judge -show`
and the page name them.

`-mode single` gives each stage to one call, and `-mode stream` cuts each into calls; the default,
`judge.mode`, is `auto`.

Run it again whenever you want another judgment: later, as the conversation grows, or now, with
another evaluator. Each run is kept.

## Read the results

```sh
asz judge -list 0438c73b-2367-4ed5-9de3-13ef9a17ed01          # every run
asz judge -show 2 0438c73b-2367-4ed5-9de3-13ef9a17ed01        # one run
asz judge -show 2 -json 0438c73b-2367-4ed5-9de3-13ef9a17ed01  # its stage files
```

To compare the run with what a person concluded, such as a ticket or an incident report, give each
source with `-reference`: a file, an http or https URL, or a source a configured command or MCP server
fetches. The
run then finds the conclusions each source holds, and compares the conversation with each.

```sh
asz judge -reference OPS-4412.md -reference https://example.com/postmortem 0438c73b-2367-4ed5-9de3-13ef9a17ed01
```

A source that needs a login goes through a command or a tool of an MCP server you configure. Neither
sees the conversation. A command is given the source as its last argument and writes the content on its
standard output:

```yaml
judge:
  references:
    fetch:
      - prefix: https://tickets.example.com/
        command: [ticket-export, --markdown]
        content_type: text/markdown
```

An MCP server is a remote one at `url`, or a local one asz starts with `command`. asz calls `tool` once
for each source, with `arguments`. In an argument, `{source}` is the source and `{rest}` what follows the
prefix. A remote server is sent `headers`, each read from the environment variable it names.

Atlassian's remote server takes an API token, which an organization admin turns on. The variable holds
the whole header: `Basic` and the base64 of `email:token` for a personal token, or `Bearer` and the key
for a service account.

```yaml
judge:
  references:
    fetch:
      - prefix: https://example.atlassian.net/browse/
        mcp:
          url: https://mcp.atlassian.com/v2/mcp
          headers:
            Authorization: ASZ_JIRA_AUTHORIZATION
          tool: getJiraIssue
          arguments:
            cloudId: https://example.atlassian.net
            issueIdOrKey: "{rest}"
```

```sh
export ASZ_JIRA_AUTHORIZATION="Basic $(printf '%s' 'you@example.com:API_TOKEN' | base64)"
```

A local server reads its credential from its environment. List each variable that holds one in
`secret_env`. For [mcp-atlassian](https://github.com/sooperset/mcp-atlassian) on Jira Server or Data
Center:

```yaml
judge:
  references:
    fetch:
      - prefix: https://jira.example.com/browse/
        mcp:
          command: [uvx, mcp-atlassian]
          secret_env: [JIRA_PERSONAL_TOKEN]
          tool: jira_get_issue
          arguments:
            issue_key: "{rest}"
            comment_limit: 100
            update_history: false
```

```sh
export JIRA_URL=https://jira.example.com
export JIRA_PERSONAL_TOKEN=...
```

On Jira Cloud it reads `JIRA_USERNAME` and `JIRA_API_TOKEN` instead; list `JIRA_API_TOKEN` in
`secret_env`.

To choose the tool and its arguments, list the server's tools with the entry's settings, before `tool`
is set:

```sh
asz judge -reference-tools https://example.atlassian.net/browse/OPS-1
```

It prints each tool with its input schema. An argument that holds a secret goes in `arguments_env`, the
argument and the variable that holds its value; `arguments` are kept as written.

What asz keeps of a source goes with the run when it is exported. A URL is kept without its query and
fragment, and one with a user or a password is refused. An MCP server is kept as its URL's scheme and
host, or its program's name; the arguments as sent, an `arguments_env` one as `[env NAME]`. Images and
other blocks that are not text are left out, and so is the structured content when there is text;
`-show` says what a copy leaves out.

asz removes the secrets it knows from all it keeps: each header value and its credential, a Basic
credential's user and token too, each `arguments_env` and `secret_env` value, the session id, and the
proxy's user and password. It finds them as written, inside JSON strings however they are escaped, and
in text escaped as JSON with no quotes around it, such as a log line, to four levels of string inside
string. A secret in another form, such as base64, a URL's percent
encoding or HTML character references, or deeper than four levels, is not found. A query is still sent
to its server by an http or a command fetch, and the page can repeat it.

To compare the judge's grouping with your own, list your goals for the conversation in your own words:

```yaml
goals:
  - Finish the post-vote steps of the 0.4.0 release
  - Remove the website pull request step from the post-vote script
```

```sh
asz judge -labels goals.yaml 0438c73b-2367-4ed5-9de3-13ef9a17ed01              # a new run, regrouped
asz judge -regroup 2 -labels goals.yaml 0438c73b-2367-4ed5-9de3-13ef9a17ed01   # a closed run
```

One call of an evaluator places each target of the run in the goal it serves: the one `-evaluator`
names, or the plan's targets stage's.

`-show` then follows your goals and says where the judge's grouping differed.

`-show` prints a run goal by goal, the most severe outcome first. Each target has its final score out
of 100, with the record's score beside it when they differ. Then come why it holds, what was done for
it, the person's corrections, its rework, what it still misses, and what it cost. A text the evaluator
wrote is marked `read:`, apart from what asz checked.

A run is kept in the session's directory, under `judgments/`. Its `work/` directory holds what each
evaluator was given, every navigation call it made, and what it answered. For an agent CLI it also
holds the CLI's output. For an API evaluator it holds what the model said on each turn, up to 2,000
bytes, and what it called. What a tool answered is not kept: the run is pinned to its head, so the
same call answers the same again. `work/sources/` holds each source as asz saved it: `R1.json`, its
manifest, `R1.fetched`, the bytes fetched or, for an MCP server, the tool's result with its secrets
removed, and `R1.txt`, the text the evaluators read.

When `export.otlp.endpoint` is set, `asz collect`, `asz server` and `asz push` send each run once it has
closed. A run of a pulled snapshot is not sent: it stays in the snapshot, and expires with it. See
[Export over OpenTelemetry](export-otlp.md).

## Evaluators

`judge.evaluators` names the evaluators a stage can be given to. The defaults are three agents'
command-line programs:

```yaml
judge:
  evaluators:
    claude:
      kind: cli
      program: claude
      args: []
      max_turns: 60
    codex:
      kind: cli
      program: codex
      args: []
    kiro:
      kind: cli
      program: kiro-cli
      args: []
```

An agent CLI must be installed and logged in. asz gives it the conversation through `asz judge mcp` as
its only tools. `claude` runs with its built-in tools, other MCP servers, and user and project settings
switched off, so their hooks and plugins do not run; settings an organization enforces still apply.
`codex` runs without the user's configuration, its shell, image and browser tools off. `model` picks
the CLI's model; empty leaves its default. `command` is its path when it is not on `PATH`.

`kiro-cli` keeps every session it runs in Kiro's history, by directory, so a `kiro-cli` stage leaves its
session there. `kiro-local` collects the sessions of Kiro's V3 engine. When `kiro-cli` runs that engine,
exclude the storage root, where a stage runs, so asz does not collect its own judge sessions. A file that
lists one or more `adapters` replaces the whole default list, so keep the others you use:

```yaml
adapters:
  - name: kiro-local
    enabled: true
    exclude:
      - /private/tmp/**
      - /absolute/path/to/the/storage/root/**
```

An API evaluator is called by asz itself:

```yaml
judge:
  evaluators:
    haiku:
      kind: api
      program: anthropic
      endpoint: https://api.anthropic.com
      model: claude-haiku-4-5
      api_key_env: ANTHROPIC_API_KEY
    gpt:
      kind: api
      program: openai
      endpoint: https://api.openai.com/v1
      model: gpt-5
      api_key_env: OPENAI_API_KEY
    deepseek:
      kind: api
      program: bedrock
      model: us.deepseek.r1-v1:0
      region: us-east-1
      profile: default
    deepseek-key:
      kind: api
      program: bedrock
      model: deepseek.v3.2
      region: us-west-2
      api_key_env: AWS_BEARER_TOKEN_BEDROCK
```

- `anthropic` is the Messages API.
- `openai` is any endpoint that speaks the OpenAI chat completions API, such as a gateway or a local
  model server. Its `endpoint` ends before `/chat/completions`.
- `bedrock` is Amazon Bedrock's Converse API. With `api_key_env`, that variable holds a Bedrock API key,
  sent as a bearer token. Without it, requests are signed with the credentials in
  `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY` and `AWS_SESSION_TOKEN`, or else with those the AWS CLI
  gives for `profile`. Run `aws login` first when the profile uses a login.

`max_turns` bounds the model calls an API evaluator makes in one evaluator call, 40 when unset, and
`max_tokens` what one model call may answer: 8000 when unset for `anthropic` and `bedrock`, and the
server's own limit when unset for `openai`, since OpenAI's reasoning models refuse the field.
`judge.timeout` bounds one evaluator call of any evaluator. A stage cut into calls runs several, and a
call that fails, a timeout included, is tried once more. When the timeout passes, the CLI is stopped,
with everything it started on Linux and macOS. On Windows a program the CLI started can outlive it.

## Prices

Money needs a price file. Name it in `judge.pricing`, or pass `-pricing` to `asz measure`:

```yaml
currency: USD
models:
  - model: claude-opus-5
    per_million: {in: 5, out: 25, cache_read: 0.5, cache_write: 6.25}
  - model: claude-opus-5
    from: 2026-11-01T00:00:00Z
    per_million: {in: 4, out: 20, cache_read: 0.4, cache_write: 5}
  - model: deepseek.v3.2
    aliases: [us.deepseek.v3.2]
    per_million: {in: 0.62, out: 1.85, cache_read: 0.062, cache_write: 0}
```

`model` is the name exactly as the runtime recorded it, and `aliases` are other recorded names for the
same price. An entry with `from` applies to calls recorded at or after that time. An entry without it
applies when no dated entry does. A call whose model has no entry is left unpriced, and its tokens
still count.

`asz measure` prints what a conversation's calls consumed, per talk, with no model and no run:

```sh
asz measure -pricing pricing.yaml 0438c73b-2367-4ed5-9de3-13ef9a17ed01
```

## Keep things from the evaluators

`judge.hide` lists the flags whose records the evaluators are never shown, as `view.hide` does for the
page: `system_prompt`, `tool_schemas`.

## Judge a conversation from another asz

A conversation an asz elsewhere collected is judged here once `asz pull` copied it:

```sh
asz judge -pull -remote-conversation prod/7722b6a5-2f0b-4a7e-9a8b-4b4f1e9f6c11
```

See [Pull from Another asz](remote-pull.md#judge-a-pulled-conversation).
