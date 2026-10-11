# Kiro

asz assembles conversations from the sessions Kiro writes on this machine: kiro-cli's, and Kiro
Crew's. It reads Kiro's own files. Kiro only has to run its V3 agent engine.

## Run Kiro on its V3 engine

kiro-cli, for one session:

```sh
kiro-cli chat --v3
```

kiro-cli, for every session:

```sh
kiro-cli settings chat.agentEngine v3
```

Kiro Crew, then restart it:

```sh
kirocrew config set agent.acp_backend kas
```

## Turn the adapter on

`kiro-local` is on by default. It reads `~/.kiro`. To read another directory, or to change which
sessions are collected, set it in `asz.yaml`:

```yaml
adapters:
  - name: kiro-local
    enabled: true
    source_root: ""
    include: []
    exclude:
      - /private/tmp/**
```

A file that lists `adapters` replaces the whole default list, so list every adapter you want. See
[Configuration](configuration.md#the-kiro-adapter).

## Check what it sees

```sh
asz sources
```

```text
kiro root  : /Users/me/.kiro
not read   : 24 session(s) of kiro-cli's V2 engine under sessions/cli; only the V3 engine's sessions are collected: start kiro-cli with --v3, or set chat.agentEngine to v3

SESSION                                    STREAMS  WORKSPACE
sess_5f20ccf4-b254-4bdb-945b-080e051eb7f5  1        /Users/me/.kiro/crew/workspace
```

Sessions of kiro-cli's V2 engine are counted and not collected.

## Collect

```sh
asz server
```

The page lists each Kiro session as a conversation, named by the title Kiro gave it.

## Supported versions

| Kiro | Versions | Collected |
| --- | --- | --- |
| kiro-cli on its V3 engine | 2.28.0, engine `@kiro/agent` 0.66.26 | yes |
| Kiro Crew on `kas` | 0.7.2, on kiro-cli 2.28.0; 0.8.0, with its kiro-cli 2.27.1, engine 0.66.22 | yes |
| kiro-cli on its V2 or V1 engine | every version | no; V2 sessions are counted |

A session whose `session.json` names a `schemaVersion` other than `1.0.0` is counted and not
collected.

## Limits

- Kiro records no token counts, so no token metric is derived for Kiro.
- Kiro Crew removes the sessions of a task's steps, `kirocrew run`, soon after each step ends. In one
  measured run they lasted 1.4 to 358 seconds, 68 at the median, so a collection every few minutes
  does not see most of them. See the [Kiro adapter](../adapters/kiro.md#kiro-crew).
- A large tool output is collected as the preview the model was sent.

What each Kiro record becomes is on the [Kiro adapter](../adapters/kiro.md) page.
