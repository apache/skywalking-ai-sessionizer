# Export over OpenTelemetry

`asz push` sends what the storage root holds to an OpenTelemetry logs receiver: every landed
file and every round, as OTLP logs over gRPC, or over HTTP with a protobuf body. The SkyWalking
OAP accepts both, on its gRPC port and on its REST port, and so does an OpenTelemetry Collector.

```yaml
# asz.yaml
export:
  otlp:
    protocol: grpc              # the default; http posts to the receiver's /v1/logs instead
    endpoint: 127.0.0.1:11800   # the OAP's gRPC port; with http, http://127.0.0.1:12800
```

```sh
./bin/asz push -once      # send everything not yet sent, then exit
./bin/asz push            # keep sending new files every export.otlp.interval
```

## One log record per file

A file is sent whole: one log record whose body is the file's bytes, unchanged. A receiver
stores it as it was landed, checks its digest at once, and has nothing to put back together.
Every `{seq, row, block}` reference in a round still means what it meant, because a row is a
line of the body it names.

This is the second design. The first sent one record per line, and it was measured on one
conversation of 306 files: 39,683 records, attributes 16% on top of the body even after
trimming, and a receiver that had to track which lines had arrived before it could verify
anything. A landed file is cut at a budget and a round is cut at the same budget, so a whole file
is a small record, and the same conversation is 306 records.

Landed files and rounds are both write-once, so each is sent once. `push.state` in the storage
root lists what was sent, with the digest each file had; a file is recorded only after the request
carrying it succeeded, so a failed request leaves it for the next pass.

## Transport

The request is the `ExportLogsServiceRequest` of the OpenTelemetry protocol, built from the
protocol's own Go definitions and sent by the gRPC client of the official module, or posted
over HTTP with a protobuf body. Nothing about the encoding is the project's own, so any OTLP
logs receiver reads it.

| `protocol` | Endpoint | How a request travels |
| --- | --- | --- |
| `grpc`, the default | `host:port`, such as `127.0.0.1:11800` for the OAP | One connection is opened for the run and every request of a pass is one `Export` call on it. OTLP defines `Export` as a single call and answer, not a stream, so a pass is a sequence of calls on one HTTP/2 connection. `tls: true` makes the connection a TLS one, verified against the system's roots |
| `http` | The receiver's base URL, such as `http://127.0.0.1:12800` for the OAP; `/v1/logs` is appended | Each request is one POST with `Content-Type: application/x-protobuf`. The scheme decides whether the connection is TLS |

`headers` travel with every request on both transports, as gRPC metadata or as HTTP headers,
which is where an authorization token goes. A receiver that answers with a partial success,
saying it rejected some records, has taken the request, and the protocol says not to send it
again: the count is reported on the pass line as `rejected`, and the files are marked sent.

## What every record carries

The resource, which names the service a record belongs to:

| Attribute | Value |
| --- | --- |
| `service.name` | `export.otlp.service_name`, or when empty the runtime that produced each session, read off its landed header's adapter: `Claude Code` for `claude-code-local`, `Mock Agent` for `mock`. One service per kind of agent, and a root that holds both is pushed as both |
| `service.instance.id` | `export.otlp.instance_id`: who is pushing, in words the people reading the receiver recognise. A receiver lists it under the service as the instance, so put your mailbox, your name, or the machine there. Empty means `user@host` of the machine running `asz push`, which is stable across restarts. The session a record belongs to is on the record as `asz.session` |
| `service.layer` | `export.otlp.layer`, `AI_AGENT` by default, the layer the receiver places the service in. The OAP selects its rules by layer, and a layer name is upper case with underscores |
| `telemetry.sdk.name` | `asz`, so a receiver can tell these records apart from any other source |
| `telemetry.sdk.version` | the version of `asz` that sent them |
| `telemetry.sdk.language` | `go` |

The scope is `github.com/apache/skywalking-ai-sessionizer` with the same version. Each record then
says what its file is, so a receiver can route it, index it and verify it without decoding the
body:

| Attribute | Value |
| --- | --- |
| `asz.format` | `sd` for a landed file, `sf` for a round |
| `asz.format.version` | the version in the file's first line: `sd/1` or `sf/1` |
| `asz.file` | the file's path relative to the storage root |
| `asz.file.kind` | for `sd`, the header's kind: `transcript`, `agent_meta`, `journal`, `workflow_manifest`, `workflow_script`; for `sf`, `round` |
| `asz.file.digest` | the file's SHA-256, the digest of the body as received |
| `asz.lines` | how many lines the body has, the header and the closing line included |
| `asz.session` | the session the file belongs to; for `sf`, the session the round was assembled from |
| `asz.from_time`, `asz.through_time` | the earliest and the latest record time in the file, as the runtime wrote them, in UTC; for `sf`, the round header's own pair, the range of the files that round consumed. Absent when no record carries a time, as in a child's meta file |
| `asz.session.from_time`, `asz.session.through_time` | for `sf` only: the session's own range as of that round, when it began and its last activity so far. A landed file never carries it: it can travel before any round exists, and the last activity keeps moving, so the value there would be missing or stale |
| `asz.conversation.title`, `asz.conversation.talks`, `asz.conversation.steps`, `asz.conversation.streams`, `asz.conversation.segments`, `asz.conversation.unresolved` | for `sf` only: what a list of conversations shows, as of that round, copied off the round's header. A receiver lists conversations off its newest round per conversation and never folds |
| `asz.seq` | for `sd`: the landed sequence. With the session it names the file a round's `{seq, row}` reference points at, and the row is a line of the body |
| `asz.stream`, `asz.run` | for `sd`: the stream or workflow run the file belongs to |
| `asz.conversation`, `asz.round` | for `sf`: the conversation and the round number |

The record's time is chosen so a receiver can bound a read by a range it already holds. A landed
file is stamped with its last record time, and a file whose records carry no time, such as a
child's meta file, with the latest record time of the session as known at push, which is always
inside the session's range and cannot go stale. A round is stamped with the session's last
activity as of that round, so a receiver's newest row per conversation is the head. Every record
also carries the time it was observed.

Every scenario in the test suite is pushed to a receiver over both transports and checked against
the two tables above, in both formats, so a change to the wire that this page does not describe
fails the build. See [Scenarios](../guides/scenario.md).

## Checking what a receiver gets

An OpenTelemetry Collector with a file exporter writes back what it decoded, which is the easiest
way to see the records before pointing at a backend:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        max_recv_msg_size_mib: 32   # the default is 4 MiB, below the 8 MiB batches; see Size
      http:
        endpoint: 0.0.0.0:4318      # the default limit is 20 MiB, enough
exporters:
  file:
    path: /out/logs.json
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [file]
```

Point `export.otlp.endpoint` at `127.0.0.1:4317`, or with `protocol: http` at
`http://127.0.0.1:4318`, run `asz push -once`, and read `logs.json`: one JSON line per request,
with the resource, the scope and the records as the Collector understood them.
`make e2e-collector` does exactly this with a generated session and a Collector container, once
over each transport, then checks every record against the root and rebuilds the root from what
the Collector wrote; CI runs it on every change. Writing each record's body to `asz.file` under a
new root gives a root that `asz verify` and `asz view` read like the original.

## Metrics

`asz push` sends metrics as well, when an adapter produces them, under the name Claude Code's
own OpenTelemetry exporter uses, so a receiver holds one metric name whichever produced the
points. What the local adapter derives is a reconstructed subset of the exporter's family, not a
copy of it, and this table says exactly which part:

| | The runtime's exporter | Derived from the transcripts |
| --- | --- | --- |
| metric | `claude_code.token.usage`, `Number of tokens used`, unit `tokens`, a monotonic delta sum | the same name, description, unit and kind |
| `type` | `input`, `output`, `cacheRead`, `cacheCreation`, a point for each even when zero | the same four, zero included |
| `query_source` | `main`, `subagent`, `auxiliary` | `main` and `subagent`, from the stream the call was made on; the auxiliary calls, such as the Haiku call that names a session, never reach a transcript |
| `model`, `session.id` | yes | yes |
| `user.id`, `user.email`, `user.account_uuid`, `user.account_id`, `organization.id`, `terminal.type`, `effort`, `speed`, and the agent, skill, plugin and MCP attribution | yes | no; a transcript does not carry them |
| the other seven metrics: cost, active time, lines of code, commits, pull requests, sessions started, edit decisions | yes | no, and never estimated |
| resource | `service.name` `claude-code`, `service.version`, `host.arch`, `os.type`, `os.version` | `service.name` `claude-code`; asz normalises both on the way out |
| instrumentation scope | `com.anthropic.claude_code`, versioned as the runtime | asz's own, so a receiver that keys on the scope sees two streams of one name; the OAP keys on the name and the labels |
| a point's window | the exporter's export interval, wall clock | the minute the call's last fragment ended, see below |
| value | a double | a double |

The right-hand column is held to a capture of what Claude Code 2.1.260 sent to asz's receiver
from one short session, kept under `internal/metrics/testdata` with every identifying value
replaced, by a test that fails when the exporter sends a label or a metric this table does not
account for. `go run ./tools/otlpdump -redact FILE.pb` prints any spooled request the same way.

With `metrics: true` on the `claude-code-local` adapter, the collector derives the points from
the landed files by the assembler's own rule: the usage of a call is its last fragment's in line
order, never a sum, and only a call that finished counts. A main transcript repeats the final usage
on every fragment; a child's carries streaming partials on all but the last, and a call with no
terminal fragment, 7% of a real corpus's child calls, has no usage at all. A call cut at a landed
file boundary is read on into the next file of its stream, and a file ending mid-call waits for
that file for a short grace before it is derived with what it has, under a watching collector; a
single pass, the backfill over history that already exists, derives with what is there and never
waits. A call is counted once however
many files its records reach, and a record the runtime re-emitted before a context reset is the
same call again.

Points are summed per minute and attribute set, and the windows of one series never overlap: a
point takes its minute unless the series already has a point at or past it, as when two children
ran in the same minute or a child's file landed later, in which case it follows the series' last
point. No point of a series is ever thrown away for another. The first derivation over a root
with history is bounded by `metrics_lookback`, 24 hours unless set, and by the newest request the
receiver adapter landed, so switching the flag on sends neither a year of tokens nor what the
runtime's exporter already sent. Every later pass derives each new file whole. A pass is
deterministic and its requests are named after their landed files, so a pass cut short is run
again to the same bytes and nothing is counted twice.

The other source of the same family is the runtime's exporter itself: the `claude-code-otlp`
adapter receives what Claude Code sends and lands each metrics request in the same spool, bytes as
received. One root sends one source: `metrics` may be on for the local adapter or for the
receiver, and the configuration refuses both.

The points wait in the storage root's `_metrics/` spool, one write-once file per landed file
with points or per request received, and go out in order under the same budget and the same
once-only rule as the files. `export.otlp.logs` and `export.otlp.metrics` switch the two things a
push sends, the files and rounds as logs and the spool as metrics, so a receiver that takes one
and not the other is sent what it takes.
On the way out the resource is normalised to asz's identity, the service, the layer, the sender,
so the OAP holds one service for the runtime. A receiver that answers with a partial success has
taken the request, and the protocol says not to send it again: the rejected records or points are
counted on the pass line as `rejected` and the file is marked sent.

## Rate

A first push sends the whole history of the storage root, which can be hundreds of megabytes, as
fast as the receiver takes it. `export.otlp.max_bytes_per_minute` caps what goes on the wire: a
pass waits before a request until a minute's budget, refilled continuously and never holding more
than a minute's worth, covers the request's encoded size. The budget starts full, so a pass that
sends a few new files never waits, and a request larger than a minute's budget waits for a full
one and leaves it empty. Zero, the default, is no limit. `asz push -once` still sends everything
before it exits, and the pass line's `paused=` field says how long it waited.

A pass goes session by session, the session landed first going first: its files, then its
rounds. A receiver rebuilds a session once it holds both, so during a long first push the sessions
become complete one after another rather than all at the end.

A receiver that answers `429` over HTTP or `ResourceExhausted` over gRPC is asking the sender to
slow down. The pass stops there, leaves the rest for the next pass, and empties the budget, so the
next request waits a whole minute's worth when a rate is set. In watch mode a `Retry-After` longer
than the interval is honored.

## Size

A request carries at most `export.otlp.batch_bytes` of file bytes, 8 MiB by default, which keeps a
request under the 10 MiB the OAP's HTTP server accepts. A file larger than that is sent alone, in
a request of its own. A landed file is cut at
`max_delta_bytes`, 2 MiB by default, and a round is cut at `parse.max_round_bytes`, also
2 MiB, so a request normally carries several files. The exception on both sides is a single
unit larger than the budget: a source record is landed whole, and a round covering one landed
file is published whole. The largest source record in the measured corpus is 4.5 MB.

The receiver's limit must cover the largest single request. The OAP accepts 50 MB over gRPC and
10 MiB over HTTP by default. An OpenTelemetry Collector accepts 4 MiB over gRPC and 20 MiB over
HTTP unless its receiver is configured otherwise, so its gRPC receiver needs
`max_recv_msg_size_mib` raised, as the example above does.
