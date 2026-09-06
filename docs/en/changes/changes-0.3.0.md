# Changes in 0.3.0

> In development, not yet released. `tools/release.sh prepare 0.3.0` removes this note.

## Read
- The conversation page is drawn by Horizon's conversation renderer,
  `@skywalking-horizon-ui/conversation-view`, embedded from a pinned Horizon commit with Horizon's
  themes and fonts, so `asz view` and the SkyWalking UI draw a conversation identically and the
  page needs nothing from the network. The hand-written viewer is gone with the API routes only it
  read; the page reads the `asz.view` document, the glossary, and the landed record behind a step.
  `tools/conversation-view.sh` rebuilds the copy from the pin, and CI fails when it differs.

## Release

- A release publishes the container image under the version the tag names, and under `latest`
  when it is the newest release, and nothing looser. The `major.minor` line tag, `0.2` beside
  `0.2.0`, is no longer published: a reader who pulled it could not tell which release answered.
  The `0.2` tag that 0.2.0 published stays on the registry and points at the same image as `0.2.0`.
