# Changes in 0.4.0

> In development, not yet released. `tools/release.sh prepare 0.4.0` removes this note.

## Collection

- The collector's default `interval` is 10 minutes, in `asz.yaml` and when no configuration is
  given; it was 5 seconds. Every pass that finds new records lands new files, so a period of
  seconds wrote many small files, and each one is a log record when sent. `asz collect` and
  `asz server` still make a pass when they start. A configuration that sets `interval` keeps its
  value. [Configuration](../setup/configuration.md#choosing-an-interval) explains the choice.
- A scenario build writes `interval: 5s` into the configuration it creates, so a collector beside a
  feed still picks up each session as it arrives. A directory an earlier build wrote without an
  interval is brought up to date instead of refused.
