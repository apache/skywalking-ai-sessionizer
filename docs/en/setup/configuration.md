# Configuration

One YAML file. Every command reads `asz.yaml` from the working directory when no `-config` flag is
given, and `-config FILE` names another one. The file at the repository root is the default
configuration with every value written out, and a test holds it to the compiled defaults, so
reading that file is reading the defaults.

```yaml
storage:
  root: ./data

adapters:
  - name: claude-code-local
    enabled: true
    source_root: ""
    include: []
    exclude:
      - /private/tmp/**
    collector:
      mode: watch
      interval: 5s
      max_delta_bytes: 4194304
```

## storage

| Key | Default | Meaning |
| --- | --- | --- |
| `root` | `./data` | The storage root: where collected data lands and where conversations are assembled. A relative path is resolved from the working directory. |

The default is ignored by git, so running the collector inside a checkout never stages private
transcripts.

## adapters

A list. Version 0.1.0 has one adapter, `claude-code-local`, which reads Claude Code's files from
this machine. Every command runs once per enabled adapter.

| Key | Default | Meaning |
| --- | --- | --- |
| `name` | | `claude-code-local` |
| `enabled` | `true` | A disabled adapter is skipped by every command. |
| `source_root` | empty | Where Claude Code keeps its files. Empty resolves it the way Claude Code does: `CLAUDE_CONFIG_DIR`, then `XDG_CONFIG_HOME/claude`, then `~/.claude`, each followed by `projects`. Set it only to collect from a copy or a mounted directory. |
| `include` | empty | Session filters, see below. Empty means every session is a candidate. |
| `exclude` | `/private/tmp/**` | Session filters, see below. |

### Session filters

A session is judged by the working directory its main transcript was recorded under. An entry that
starts with `/` is a working directory, and `**` after it matches everything beneath. Anything else
is a glob matched against the source directory name as Claude Code wrote it, which is the working
directory with every separator replaced by `-`.

A session's child agents can run in other directories, so the filter looks at the main transcript
only. A session that merely used a scratch directory for a child agent is still collected. When the
main transcript has been pruned, the session is judged on all of its directories together and is
excluded only when every one of them matches, so an orphaned child stream from a real project is
still collected.

Claude Code runs its own helper agents in scratch directories under `/private/tmp`. Those sessions
are the tool's, not yours, which is why they are excluded by default.

## collector

| Key | Default | Meaning |
| --- | --- | --- |
| `mode` | `watch` | `watch` polls the source continuously. `once` makes a single pass and exits, which is the backfill path over history that already exists. `-once` on the command line overrides the file. |
| `interval` | `5s` | How long the collector sleeps between passes in watch mode. `asz view` refreshes on the same interval. |
| `max_delta_bytes` | `4194304` | The largest `.sd` file the collector writes, 4 MiB. A large catch-up is split into several files, and a single record larger than this is landed whole. It is also the largest unit a receiver has to accept in one message when landed files travel, which is why the default fits the 4 MiB limit an OpenTelemetry Collector applies out of the box. A change applies to new files only; `asz repack` brings an existing root under a new budget. |

## export

```yaml
export:
  otlp:
    endpoint: ""
    service_name: ""
    layer: GENAI
    batch_bytes: 1048576
    interval: 5s
```

| Key | Default | Meaning |
| --- | --- | --- |
| `endpoint` | empty | The OpenTelemetry logs receiver's base URL; `/v1/logs` is appended. The SkyWalking OAP listens on its REST port, `http://127.0.0.1:12800` by default. Empty means `asz push` refuses to run. |
| `service_name` | empty | The service every record is attributed to. Empty means the project directory each session was recorded under, one service per project. |
| `layer` | `GENAI` | Sent as `service.layer`, which the OAP uses to place the service. |
| `headers` | none | Headers added to every request, for example `Authorization`. |
| `batch_bytes` | `1048576` | How much body text one request carries at most, 1 MiB. |
| `interval` | `5s` | How long `asz push` sleeps between passes in watch mode. |

See [Export over OpenTelemetry](export-otlp.md) for what is sent.

## Precedence

1. `-config FILE` on the command line.
2. `./asz.yaml` in the working directory.
3. The compiled defaults, which are the values shown above.

A file may leave keys out. Anything unset takes its default, except that a file which lists
`adapters` replaces the whole list.
