# Quick Start

Turn the Claude Code history on this machine into conversations you can read. Nothing in Claude
Code needs to change, and history written before asz was installed is included.

## 1. Install

[Install](install.md) `asz`. Check it:

```sh
asz version
```

## 2. Run

asz keeps its data in `./data` under the directory you run it in. Pick one, and start the server
there:

```sh
mkdir -p ~/asz && cd ~/asz
asz server
```

Open <http://127.0.0.1:8787>. `asz server` collects when it starts and then every 10 minutes, and
the list page shows when it last did.

## 3. Configure

asz reads `asz.yaml` from the same directory, or the file given with `-config`. Without one it uses
the defaults. To change them, download the default configuration of your version into that
directory and edit it:

```sh
VERSION=$(asz version | cut -d ' ' -f 2)
curl -fsSL -o asz.yaml "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/asz.yaml"
```

[Configuration](configuration.md) describes every setting. The common ones:

- `storage.root`: where the data goes.
- `collector.interval` of an adapter: how often it collects. A shorter interval shows new data
  sooner and writes more, smaller files.
- `export.otlp.endpoint`: send the data to an OpenTelemetry receiver, such as the SkyWalking OAP.
  See [Export over OpenTelemetry](export-otlp.md).

## Other commands

```sh
asz sources          # the sessions asz found, and their files
asz collect -once    # collect once, without serving the page
asz verify           # check the collected data against its digests
```

[Command Line](command-line.md) lists every command. To record which files each tool call changed,
install the [Claude Code plugin](claude-code-plugin.md). To start over, stop asz and delete `./data`.
