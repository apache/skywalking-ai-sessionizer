# LangChain Plugin

Records what each of a LangChain agent's tool calls changed on disk, so a
conversation shows the work as well as the words.

The [LangChain receiver](langchain.md) brings the conversation. This brings
what the tools did to the workspace, which nothing on the wire says.

## What it is

Two pieces, and neither holds any logic of its own:

| | Is |
| --- | --- |
| `apache-skywalking-asz-langchain` | says when a tool call begins and ends |
| `asz-changes` | scans the watched path and writes the difference |

`asz-changes` is the same program the Claude Code plugin runs. LangChain has no
way to tell a program outside the process that a tool started, and Claude Code
does, which is the whole reason the Python half exists.

## Installing

```sh
pip install apache-skywalking-asz-langchain
asz-langchain enable
```

or, from the source package of a release, `pip install ./plugins/langchain`. And `asz-changes` on
the machine, which the [install script, Homebrew and apt](install.md) all put beside `asz`.

The application imports nothing and changes nothing. `enable` writes a loader
into the environment's site directory, so it runs at interpreter start and
attaches the handler once the environment says to. It is a separate command
because a wheel cannot put a file there: measured, `pip` places a declared data
file in the environment's root, where Python never reads it and the loader
silently never runs.

`asz-langchain status` says whether it is on, and `asz-langchain disable` takes
it out again.

## Running

```sh
export ASZ_CHANGES=true
export ASZ_WATCH=/path/to/the/workspace
export ASZ_CHANGES_DATA=/var/lib/asz/changes
```

`ASZ_CHANGES_BIN` names the program when it is not on `PATH`. Without it and
without `PATH`, the handler logs one line and the agent runs untouched: a
missing record is a gap in observation, and stopping the agent would be a gap
in the application.

Then read what it wrote with the `changes` adapter, pointed at the same
directory:

```yaml
adapters:
  - name: changes
    enabled: true
    source_root: /var/lib/asz/changes
```

## Where the settings live

In a file, not in your code. `asz-changes` reads `settings.yaml` from the data
directory it was given:

```text
$ASZ_CHANGES_DATA/settings.yaml     the settings
$ASZ_CHANGES_DATA/output/           the records it writes
```

So with `ASZ_CHANGES_DATA=/var/lib/asz/changes`, the file is
`/var/lib/asz/changes/settings.yaml`, and the `changes` adapter reads the
records with `source_root: /var/lib/asz/changes`.

Under Claude Code there is no `ASZ_CHANGES_DATA`: the runtime gives the plugin
its own directory, and the file is
`~/.claude/plugins/data/file-changes-skywalking-ai-sessionizer/settings.yaml`.

## Choosing which tools are watched

**Nothing is watched until you say so.** The default names `Bash`, `PowerShell`
and `Monitor`, which are Claude Code's tools; a LangChain application has none
of them, so the plugin records nothing until this is set.

That is the honest default. A LangChain application names its own tools, and
asz has no way to know which of them write and which only read — there is no
convention to follow and nothing to guess from a name.

So the tool names below are **yours**, not ours. Write the ones your
application defines:

```yaml
tools:
  scope: ["*"]
  exclude: ["re:^(read|get|list)_", "search_docs"]
```

An entry is an exact name, a regular expression when it begins `re:`, or `*`
for every tool. The exclusions take the same three forms and are applied after,
so an exclusion beats an exact entry in `scope`.

They are what makes `*` usable. The read-only classifier understands shell
commands and nothing else, so for a runtime whose tools take arguments the
exclusions are the only way to keep a scan off a tool that only reads. A
regular expression that does not compile matches nothing rather than
everything, so a setting that was got wrong observes too little and is noticed
rather than scanning the workspace on every call.

Two ways to arrive at a setting, both fine:

- name the tools that write — `scope: [write_report, apply_patch]` — which
  costs nothing and misses a tool you forget;
- take everything and exclude what reads — `scope: ["*"]` — which misses
  nothing and scans more.

## What it costs when it is off

Nothing measurable. The loader reads one environment variable and imports
nothing else, because importing `langchain_core` takes about 0.2 s and the
loader runs in every Python process in the environment, `pip` included.
Measured against a real wheel installed into a fresh environment:

| | Interpreter start |
| --- | --- |
| plugin off | 0.01 s |
| plugin on | 0.21 s |

## Which conversation a record belongs to

The same name asz gives it: the project and the thread, slugged for a reader
and digested so two owners cannot collide. The rule is written twice, once in
Python and once in Go, and a test holds them to the same table — the receiver
lands the conversation, this lands what changed, and they meet only in the name
of the session directory.

A tool call in a run with no thread key records nothing. There is no
conversation to file it under, and inventing one would be worse than the gap.

## What it cannot see

- **Changes outside the watched path**, including anything in another container
  or on another host.
- **Which tool changed what, when two run at once.** The window is shared, and
  the record says so rather than guessing.
- **Anything at all after a long collector outage.** Output expires on the
  retention window whether or not it was collected, the same as on the Claude
  Code path.
