# Export over OpenTelemetry

asz can send what it collects to an OpenTelemetry receiver, such as the SkyWalking OAP or an
OpenTelemetry Collector: every collected file and every conversation round as a log record, and
Claude Code's token usage as metrics. [OTLP Records](../formats/otlp.md) describes what a receiver
gets.

## Send

Name the receiver in `asz.yaml`:

```yaml
export:
  otlp:
    protocol: grpc
    endpoint: 127.0.0.1:11800   # the SkyWalking OAP's gRPC port
```

For HTTP, set `protocol: http` and `endpoint: http://127.0.0.1:12800`. Put a token in `headers`
if the receiver needs one. [Configuration](configuration.md#export) lists every setting.

`asz collect` and `asz server` then send what is new after every collection. `asz push` sends
everything not sent yet, and exits. Each file is sent once.

## Metrics

To send Claude Code's token usage, `claude_code.token.usage`, choose one source:

- Derive it from the transcripts: set `metrics: true` on the `claude-code-local` adapter.
- Or receive what Claude Code's own exporter sends, with the
  [receiver adapter](configuration.md#the-receiver-adapter).

Not both, since both would count the same tokens.

## Limit the rate

The first send carries the whole history. To spread it out, set a limit:

```yaml
export:
  otlp:
    max_bytes_per_minute: 10485760   # 10 MiB
```

## Receiver limits

A request carries up to 8 MiB, `batch_bytes`, and a single larger file goes in a request of its
own. The SkyWalking OAP accepts that. An OpenTelemetry Collector accepts only 4 MiB over gRPC unless
its receiver sets `max_recv_msg_size_mib`, as below.

## See what a receiver gets

Run an OpenTelemetry Collector that writes what it receives to a file:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        max_recv_msg_size_mib: 32
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

Point `endpoint` at `127.0.0.1:4317`, run `asz push`, and read `logs.json`.
