# Claude Code Plugin

The plugin records which files each tool call changed, including shell commands and edits inside
subagents. asz collects the records, and the conversation page shows each change beside the step
that made it. [Claude Code Plugin Internals](../adapters/claude-code-plugin.md) explains what is
recorded and how.

## Install

Install Claude Code first. Use the same version as the asz you run, from the
[downloads page](https://skywalking.apache.org/downloads/).

On macOS or Linux:

```sh
VERSION=<version>
curl -fsSL "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$VERSION/install/claude-code-plugin.sh" | sh -s -- "$VERSION"
```

On Windows, in PowerShell:

```powershell
$Version = "<version>"
& ([scriptblock]::Create((Invoke-RestMethod -UseBasicParsing "https://raw.githubusercontent.com/apache/skywalking-ai-sessionizer/v$Version/install/claude-code-plugin.ps1"))) $Version
```

The script puts `asz-claude-plugin` in `~/.local/bin`, or `%USERPROFILE%\.local\bin` on Windows, and
installs the plugin into Claude Code. Restart Claude Code afterwards.

With Homebrew, install the binary with `brew install apache/skywalking-ai-sessionizer/asz-claude-code`
instead, then run the commands under [By hand](#by-hand).

### By hand

1. Put `asz-claude-plugin` from the [binary package](install.md#binary-package) in a directory on
   your `PATH`.
2. Install the plugin. In a shell:

   ```sh
   claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v$VERSION" --sparse .claude-plugin plugins/claude-code/plugin &&
     claude plugin install asz-changes@skywalking-ai-sessionizer
   ```

   In PowerShell:

   ```powershell
   claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v$Version" --sparse .claude-plugin plugins/claude-code/plugin
   if ($LASTEXITCODE -eq 0) { claude plugin install asz-changes@skywalking-ai-sessionizer }
   ```

## Check

```sh
asz-claude-plugin version
claude plugin list
```

The plugin writes its records under `~/.claude/plugins/data/asz-changes-skywalking-ai-sessionizer/`,
or under `CLAUDE_CONFIG_DIR` when that is set. If a shell command leaves no record there:

- If Claude Code reports `Executable not found in $PATH: "asz-claude-plugin"`, it cannot find the
  binary. Put the binary's directory on `PATH`, and start Claude Code from that terminal.
- Otherwise, read `log/plugin.log` in the same directory.

An `Edit` or `Write` in the main conversation leaves no plugin record. Claude Code records those
changes itself, and asz reads them from the transcript.

## Upgrade

Run the install script again with the new version. It keeps the plugin's settings and the records
asz has not collected yet.

By hand, first move `asz-claude-plugin` to the new version: `brew upgrade asz-claude-code`,
`sudo apt update && sudo apt upgrade`, or the binary from the new version's
[binary package](install.md#binary-package). `asz-claude-plugin version` must print it. Then, in a
shell:

```sh
(
  set -eu
  : "${VERSION:?set VERSION to the version to move to}"
  claude plugin uninstall asz-changes@skywalking-ai-sessionizer --keep-data
  claude plugin marketplace remove skywalking-ai-sessionizer
  claude plugin marketplace add "https://github.com/apache/skywalking-ai-sessionizer.git#v$VERSION" --sparse .claude-plugin plugins/claude-code/plugin
  claude plugin install asz-changes@skywalking-ai-sessionizer
)
```

In PowerShell:

```powershell
& {
  if (-not $Version) { throw "set `$Version to the version to move to" }
  $Steps = @(
    @("plugin", "uninstall", "asz-changes@skywalking-ai-sessionizer", "--keep-data"),
    @("plugin", "marketplace", "remove", "skywalking-ai-sessionizer"),
    @("plugin", "marketplace", "add", "https://github.com/apache/skywalking-ai-sessionizer.git#v$Version", "--sparse", ".claude-plugin", "plugins/claude-code/plugin"),
    @("plugin", "install", "asz-changes@skywalking-ai-sessionizer"))
  foreach ($Step in $Steps) {
    & claude @Step
    if ($LASTEXITCODE -ne 0) { throw "claude $Step failed, so the steps after it did not run" }
  }
}
```

Keep this order. Removing the marketplace, or uninstalling without `--keep-data`, deletes the
plugin's data.

## Remove

```sh
claude plugin uninstall asz-changes@skywalking-ai-sessionizer
claude plugin marketplace remove skywalking-ai-sessionizer
```

This deletes the plugin's data. Run `asz collect -once` first to keep its records in asz.

## Settings

Optional. Put `settings.yaml` in the plugin's data directory. These are the defaults:

```yaml
roots: []                     # empty: the session's project directory
exclude:
  defaults: standard-v1
  add: []
  remove: []
read_only:
  enabled: true
tools:
  scope: [Bash, PowerShell, Monitor]
retention:
  idle: 30m
  ttl: 720h
scan_timeout: 30s
size_cap: 1048576
```

- `roots`: the directories watched.
- `exclude`: directories never watched. `standard-v1` holds the build output and caches of common
  languages and tools; [Exclusions](../adapters/claude-code-plugin.md#exclusions) lists them. `add`
  adds rules and `remove` drops a default. A rule that starts with `/` is relative to the root; any
  other name matches at any depth.
- `read_only.enabled`: skip the scan around a shell command that only reads.
- `tools.scope`: the tools watched with a scan.
- `retention.idle`: drop the saved file contents of a root that has been idle this long.
- `retention.ttl`: delete an output file not written for this long.
- `scan_timeout`: stop a scan after this long, and mark its record partial. Keep it well under 60
  seconds, after which Claude Code stops the hook and nothing is recorded.
- `size_cap`: record a larger file by its hash only.

With `CLAUDE_PLUGIN_DATA` set to the data directory, `asz-claude-plugin status` prints the settings
in force, and `asz-claude-plugin prune` applies the retention rules now.
