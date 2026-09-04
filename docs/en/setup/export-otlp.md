# Export over OpenTelemetry

`asz push` sends what the storage root holds to an OpenTelemetry logs receiver: every landed
file and every round, over OTLP/HTTP with a protobuf body, to the receiver's `/v1/logs`. The
SkyWalking OAP accepts it on its REST port, and so does an OpenTelemetry Collector.

```yaml
# asz.yaml
export:
  otlp:
    endpoint: http://127.0.0.1:12800
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

## What every record carries

The resource, which names the service a record belongs to:

| Attribute | Value |
| --- | --- |
| `service.name` | `export.otlp.service_name`, or when empty the runtime the adapter reads, `Claude Code` for `claude-code-local`: one service per kind of agent |
| `service.instance.id` | `export.otlp.instance_id`, the identity of this sender, or when empty a new UUID each time `asz push` starts; the session a record belongs to is on the record as `asz.session` |
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

Every scenario in the test suite is pushed to a receiver and checked against the two tables
above, in both formats, so a change to the wire that this page does not describe fails the build.
See [Scenarios](../guides/scenario.md).

## Checking what a receiver gets

An OpenTelemetry Collector with a file exporter writes back what it decoded, which is the easiest
way to see the records before pointing at a backend:

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
        max_request_body_size: 33554432   # the default is 20 MiB, enough for the 8 MiB batches; see Size
exporters:
  file:
    path: /out/logs.json
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [file]
```

Point `export.otlp.endpoint` at `http://127.0.0.1:4318`, run `asz push -once`, and read
`logs.json`: one JSON line per request, with the resource, the scope and the records as the
Collector understood them. `make e2e-collector` does exactly this with a generated session and a
Collector container, then checks every record against the root and rebuilds the root from what
the Collector wrote; CI runs it on every change. Writing each record's body to `asz.file` under a new root gives a
root that `asz verify` and `asz view` read like the original.

## Size

A request carries at most `export.otlp.batch_bytes` of file bytes, 8 MiB by default, which keeps a
request under the 10 MiB the OAP's HTTP server accepts. A file larger than that is sent alone, in
a request of its own. A landed file is cut at
`max_delta_bytes`, 2 MiB by default, and a round is cut at `parse.max_round_bytes`, also
2 MiB, so a request normally carries several files. The exception on both sides is a single
unit larger than the budget: a source record is landed whole, and a round covering one landed
file is published whole. The largest source record in the measured corpus is 4.5 MB.

The receiver's limit must cover the largest single request. The OAP accepts 10 MiB over HTTP and
50 MB over gRPC by default. An OpenTelemetry Collector accepts 20 MiB over HTTP and 4 MiB over
gRPC unless its receiver is configured otherwise.
