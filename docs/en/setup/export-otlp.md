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

## One log record per line

A file is sent as one log record per line, the header and the closing line included, and each
record's body is the line's bytes unchanged. That is what lets a receiver write the file back
byte for byte: the file's digest still matches, an uploaded round still verifies against the
landed files it names, and every `{seq, row, block}` reference still means what it meant.

Landed files and rounds are both write-once, so each is sent once. `push.state` in the storage
root lists what was sent, with the digest each file had; a file is recorded only after the request
carrying its last line succeeded, so a failed request leaves it for the next pass.

## What every record carries

The resource, which names the service a record belongs to:

| Attribute | Value |
| --- | --- |
| `service.name` | `export.otlp.service_name`, or when empty the project directory the session was recorded under, one service per project |
| `service.instance.id` | `export.otlp.instance_id`, the identity of this sender, or when empty a new UUID each time `asz push` starts; the session a record belongs to is on the record as `asz.session` |
| `service.layer` | `export.otlp.layer`, `AI-AGENT` by default, the layer the receiver places the service in |
| `telemetry.sdk.name` | `asz`, so a receiver can tell these records apart from any other source |
| `telemetry.sdk.version` | the version of `asz` that sent them |
| `telemetry.sdk.language` | `go` |

The scope is `github.com/apache/skywalking-ai-sessionizer` with the same version. Each record then
says what it is. What a receiver needs to place a line and to resolve a round's reference to it is
on every line; what is constant for a file is on its header line only, and the file's digest is on
the header and on the closing line, where a receiver checks what it wrote back. Measured, that
keeps the attributes near 15% of the body instead of the 28% that repeating everything cost.

On every line:

| Attribute | Value |
| --- | --- |
| `asz.format` | `sd` for a landed file, `sf` for a round |
| `asz.file` | the file's path relative to the storage root |
| `asz.line` | the line's index in the file, starting at 0 for the header |
| `asz.line.kind` | for `sd`: `header`, `record`, `end`; for `sf`: the frame type, `header`, `node`, `relation`, `unresolved`, `commit` |
| `asz.session`, `asz.seq` | for `sd`: the session and the landed sequence. With `asz.line`, this is the address a round's `{seq, row}` reference names: a record's row is its line index |
| `asz.conversation`, `asz.round` | for `sf`: the conversation and the round number |

On the header line only:

| Attribute | Value |
| --- | --- |
| `asz.format.version` | the version in the file's first line: `sd/1` or `sf/1` |
| `asz.file.kind` | for `sd`, the header's kind: `transcript`, `agent_meta`, `journal`, `workflow_manifest`, `workflow_script`; for `sf`, `round` |
| `asz.stream`, `asz.run` | for `sd`: the stream or workflow run the file belongs to |
| `asz.session` | for `sf`: the session the round was assembled from |
| `asz.file.digest` | the file's SHA-256, repeated on the closing line, `end` or `commit` |

A landed record is stamped with its own time when it carries one, and with the file's collection
time otherwise. A round carries no time of its own, by design, so its lines are stamped with the
time they were sent. Every record also carries the time it was observed.

## Checking what a receiver gets

An OpenTelemetry Collector with a file exporter writes back what it decoded, which is the easiest
way to see the records before pointing at a backend:

```yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 0.0.0.0:4318
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
Collector understood them.

## Size

A request carries at most `export.otlp.batch_bytes` of body text, 1 MiB by default, and a file
larger than that is sent across several requests in order. The largest single record in the
measured corpus is 4.5 MB and is sent alone. The OAP accepts requests up to 50 MB by default; an
OpenTelemetry Collector accepts 4 MiB, which is why the landing budget defaults to 4 MiB as well.
