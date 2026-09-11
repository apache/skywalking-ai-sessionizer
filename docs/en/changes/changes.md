# Changes in 0.3.0

> In development, not yet released. `tools/release.sh prepare 0.3.0` removes this note.

## Collection

- A landed record keeps the provider model a call ran on, as `model`, the name the runtime wrote
  on the record. It is what a token count is reported under, so a metric per model can be
  produced from the landed data alone. Records landed before this carry none, and the glossary
  says where the runtime writes it.
- A parse takes the conversation's lock before it reads the session's index. A parser that read
  the index first could hold an old one while a scenario removal took the session away, and then
  publish a round over evidence that is gone. A parse of a session with no landed files and no
  chain creates nothing.

## Commands

- `asz collect` is the whole pipeline. One pass lands what is new, parses every session that
  moved, and sends what `export.otlp` asks for, in that order, so a pass ships the rounds it has
  just written. It reads every enabled local adapter in the same pass, where before it ran them
  one after another and a watching source kept the others from ever running.
- `asz server` is new: the pipeline and the page in one process, which is what a person runs to
  watch their own conversations locally.
- `asz view` only reads now. It serves a storage root that already holds conversations and never
  collects, parses or sends, which is what a root copied from another machine, or filled by the
  receiver, needs. Use `asz server` to collect and serve together.
- `asz push` makes one pass and exits. A root that keeps growing is sent by `asz collect`; this
  command is for a root that is already there. `export.otlp.interval` is gone with the loop it
  drove, and the collector's `interval` is the one period.
- The `claude-code-otlp` receiver listens under `asz collect` and `asz server`, the two commands
  that write for as long as they run. It no longer listens under `asz view`.
- `asz scenario build` keeps building with `--every D`: one whole session, then that long on the
  wall clock, then the next. It takes more than one scenario file, and a directory contributes
  every `.yaml` in it, with `--pick cycle` or `--pick random` and `--seed` to repeat an order.
  Each session is stamped so its last record lands when it was written, and carries an id no
  earlier session has. It is a mock client to run beside `asz server` or `asz collect`.
- `asz scenario build --remove` says when a pipeline may remove each session a `claude-code` build
  writes: `immediately`, the default, or a duration such as `24h` or `7d` after the session's last
  record. The build writes a marker for each session after every other file of it, naming the
  policy and every file it wrote for the session with its size and SHA-256. `asz collect` and
  `asz server` remove a marked session at the end of a pass, once every landed file, round and
  metrics request of it is recorded as sent to the receiver they send to and none was rejected.
  They remove its source files, its landed files, its chain, its spool files, its lines in the state
  files, and last the marker. The session directory and its chain directory are each renamed into
  `_removed/` in one call before they are deleted, so a crash never leaves half a session for a
  parse to read. Only a pipeline whose storage
  root is the build's `--out` removes. It holds `_scenario/.lock` there, and a second one waits.
  When the configuration keeps every marked session, the `scenario :` line at the start says why.
  Removal is a policy of the scenario, never of the product. The configuration has no removal
  setting, and a session no build marked, which is every real Claude Code session, is never
  removed.
- A workflow in a scenario starts its children as one batch, and they run at the same time, as the
  runtime's do. Before, each child started only after the one before it had ended, so a review by
  four agents was drawn as four reviews in a row. The parent's next step waits for the last child.
  The `push_follows_the_wire` check now compares record times as times rather than as text, which
  the overlapping children showed it needed.

## Read

- The conversation list shows what a conversation did, not how it is built: talks, model calls,
  subagents, Bash runs, and changes with the lines those records added and removed. Steps, streams
  and segments leave the table. Every count is read off the head round's header, so a row costs no
  fold, and a count the head round does not carry shows a dash: a round cut before that count
  existed does not know the answer, and a zero would be a claim it never made. This follows
  Horizon's own conversation list, so the two read the same.
- The conversation page is drawn by Horizon's conversation renderer,
  `@skywalking-horizon-ui/conversation-view`, embedded from a pinned Horizon commit with Horizon's
  themes and fonts, so `asz view` and the SkyWalking UI draw a conversation identically and the
  page needs nothing from the network. The pin now names Horizon `22e2f869`, which draws a tool
  call's input and result as fields rather than one escaped string, reads an edit as a line diff,
  puts a changes mark on a step that changed files with the files and their diffs behind it, adds a
  Changes tab to the inspector carrying where each record came from, and pops the inspector out
  over the page. The hand-written viewer is gone with the API routes only it
  read; the page reads the `asz.view` document, the glossary, and the landed record behind a step.
  `tools/conversation-view.sh` rebuilds the copy from the pin, and CI fails when it differs.

## Release

- `tools/release.sh` runs the Apache release in five stages, each started by hand. `prepare` tags
  the candidate and opens the pull request, as before. Its tag message, its commit and its pull
  request no longer call the candidate a release, and it checks for the tools it uses first.
  `candidate` builds the packages from a fresh clone of the tag, signs them, verifies them against
  the KEYS file the way a voter does, uploads them to the dev area of dist.apache.org and writes
  the vote mail. `vote-result` counts the votes, refuses a vote that did not pass, and writes the
  result mail. `publish`, run by a PMC member, moves the voted packages to the release directory
  and writes the announcement, the website entries and the install manifests. `complete` creates
  the GitHub release. [How to Release](../guides/how-to-release.md) walks through each stage and
  the svn commands it runs.
- `candidate` refuses a signing key that is not RSA of at least 2048 bits, as the ASF requires,
  or that has no apache.org user ID, and a source package that would hold a font file, since
  fonts are Category B works the ASF keeps out of source releases. The vote mail names the signing
  key and tells voters what the committed conversation renderer is. `--no-upload` writes its mail
  to `vote-preview.txt` and refuses once a candidate is uploaded.
- `tools/package-smoke.sh` checks one binary package the way a person uses it. It verifies the
  package's sha512, unpacks it, checks that every file is there, and then runs `asz` and the plugin
  from the unpacked files: four scenarios through `asz scenario check`, the list page and one
  conversation page from `asz view`, and one shell command through the plugin's hooks. A new CI
  job, `packages`, runs it on all six packages, each on a GitHub runner of its own platform. The
  `binaries` job cross-compiles every platform on one Linux runner, and a binary that was never
  started proves nothing about its platform. The voters' check list asks each voter to run it on
  the package for their own platform.
- `candidate` runs the package for the release manager's machine with the tag's
  `tools/package-smoke.sh`, after it verifies the candidate and before the upload. When the check
  fails, nothing is uploaded and no vote mail is written. On a machine no package is built for, it
  runs none and says so.
- The result mail lists every vote, +0 and non-binding -1 included, and `--thread` links the vote
  thread. Only binding votes decide. Names are compared without case, and an empty list option is
  refused rather than taking the next option as a name.
- `publish` also holds each local signature to the voted one, dates the website entries by the day
  of the move, and refuses `--remove-old` in the run that moves: older versions leave the release
  directory in a later run, once the website links them from the archive. The announcement is
  titled `[ANNOUNCE]`, and the website entries link the packages through closer.lua.
- The notes of the GitHub release name the Apache release, the downloads page, the signatures and
  KEYS. They no longer point at a git checkout or the container image.
- The changelog has the layout of Apache SkyWalking and SkyWalking SWCK. On `main`,
  `docs/en/changes/changes.md` is the changelog of the version in development, and Current Version
  in the menu always points at it. In the commit it tags, `prepare` only removes the
  in-development note, so the tag and the source package hold the finished changelog where the
  menu and the welcome page of the tag link it. The website publishes the docs of each version
  from its tag. In the next commit, `prepare` moves the page to `changes-VERSION.md`, lists the
  version under Changelog, and writes a new `changes.md` for the next version. The vote mail, the
  announcement, the GitHub release and the winget manifest link the tag's `changes.md`. `complete`
  builds the text of the GitHub release from that page when it runs, in place of the
  `release-notes-VERSION.md` file `prepare` used to store.
  [How to Release](../guides/how-to-release.md#the-changelog) describes the layout, which the root
  `CHANGES.md` used to describe.
- `complete` uploads the voted packages to the GitHub release, each with its signature and
  checksum, after checking each against the file downloads.apache.org serves. CI no longer
  attaches the packages it builds, because they are not the signed files the vote approved. It
  still builds every platform on every run, and keeps the packages as a workflow artifact.
- Windows on ARM 64 joins the platforms, so a version ships six binary packages beside the source
  package.
- `tools/install-manifests.sh` writes a Homebrew formula, a Scoop manifest and the winget
  manifests from the voted binary packages, and `publish` runs it. They are conveniences,
  submitted after the release and after the PMC agrees to each channel. The Homebrew formula
  installs the voted binary package for macOS or Linux, on ARM 64 or x86-64. It builds nothing, so
  it needs no Go, and the `asz` it installs serves the page with the renderer's fonts. It
  downloads from the GitHub release, with archive.apache.org as its mirror, and goes to a tap,
  because homebrew-core does not take a formula that installs a binary built for each platform.
  The winget manifests download from the GitHub release too, because the archive slows down and
  bans heavy use. Each package must match the voted `.sha512` beside it.
  [Install](../setup/install.md) lists every way to get asz.
- `make release` refuses to build when git tracks a compiled file, because an Apache source release
  must not carry compiled code, and `candidate` refuses the same file types. It also refuses a tree
  with any change or untracked file, and removes packages an earlier build left in `dist/`.
  `GPG_USER` picks the key it signs with.
- The source package holds no font file. The two fonts of the conversation renderer are under the
  SIL Open Font License, which the ASF puts in Category B, and the ASF does not allow a Category B
  work in a source release. `.gitattributes` marks them `export-ignore`, so `git archive` leaves
  them out, and `make release` stops and removes a source package that holds a font file anyway.
  The binaries in the binary packages embed the fonts, and `dist-material/LICENSE` names them. A
  build from the source package draws the page with system fonts.
- Two builds of one tag give the same binary packages, byte for byte, when they use the same Go,
  the same tar and the same gzip. Go records the tag in each binary as the module's version, so a
  build of the commit before it was tagged differs. Every file has the commit's time, owner and group are 0, and
  the entries are sorted. No package records who built it or when. GNU tar and bsdtar write their
  headers differently, and GNU gzip and Apple's gzip compress differently, so a CI build, made
  with GNU tar and GNU gzip, and a macOS build, made with bsdtar and Apple's gzip, differ.
- The compiled `collectorcheck` and `otlpdump` helpers, committed at the repository root by
  mistake, are removed, and `.gitignore` keeps them out. Their sources stay under `tools/`.
- Creating the GitHub release publishes the container image under the version the tag names, and
  under `latest` when it is the newest version, and nothing looser. The `major.minor` line tag,
  `0.2` beside `0.2.0`, is no longer published: a reader who pulled it could not tell which release
  answered. The `0.2` tag that 0.2.0 published stays on the registry and points at the same image
  as `0.2.0`.

## Workspace changes

- Which files a tool call changed is shown beside the step. Two producers write one shape,
  `changes/1`, defined by `pkg/changes`, whose id is the tool-use id and whose `captured_by` says
  which producer observed it: the Claude Code adapter copies the runtime's own patch
  from every `Edit` and `Write` result on the main stream into a second `data` part beside the raw
  result, and the new asz Claude Code plugin, `plugins/claude-code/`, records shell commands, and
  edits inside subagents, into its own data directory. The plugin runs inside Claude Code's hooks,
  one short process per event, needs nothing from asz, classifies read-only commands to skip their
  scans, excludes every known language's build output by a frozen rule set, names overlapping
  windows on the same root, and keeps its output for 30 days. The new `claude-code-changes`
  adapter, on by default, tails the plugin's files and lands each line as a record of kind
  `changes` under the stream the tool ran in, with the session's own lock and sequence, and
  the pipeline refreshes both sources in one pass. Session Data and Session Flow are unchanged: `changes` is a
  new kind, assembly emits no node for it, and a session folds to the same nodes with and
  without the records.
- `asz.view` gains `workspace_changes`, every record joined to its step; a tool step names its
  records under `changes`, and `summary.changes` counts them. The version stays 1.0: no official
  release carries the format yet, so the keys are added rather than a version raised.
- The plugin ships in every binary package under `claude-code-plugin/`, and `make build` builds it
  beside `asz`.

## Metrics

- `claude-code-local` with `metrics: true` derives a reconstructed subset of the runtime's own
  metric family from the landed files, `claude_code.token.usage` in phase one under the name
  Claude Code's OpenTelemetry exporter uses: the usage of a call is its last fragment's in line
  order and only a finished call counts, per minute, by type, model, query source and session;
  the exporter's account, speed, effort and attribution labels and its auxiliary calls are not
  reconstructed, and the export page states the difference. The points wait in the storage root's `_metrics/`
  spool and `asz push` sends them over the metrics service, under asz's identity, with the same
  budget and once-only rule as the files. `metrics_lookback`, 24 hours unless set, bounds the
  first derivation over a root with history, and so does the newest request the receiver landed,
  so a switch of source counts nothing twice. A pass is deterministic, names its requests after
  their landed files and saves its state once, so a pass cut short is run again to the same bytes.
  Every scenario checks the points on the wire against its plan, and the Collector job verifies
  the tokens that arrive from both sources.

- A receiver's partial success is a success the protocol says not to retry: the rejected records
  or points are counted on the pass line as `rejected`, and the files are marked sent.

- A capture of what Claude Code 2.1.260 itself sent to the receiver, with every identifying value
  replaced, is the parity fixture: a test holds the derived token metric to it, the same name,
  unit, description, kind, value encoding and token types, the same session, model, query source
  and type labels, and a difference that is exactly the labels and metrics the export page lists
  as not derived. The derivation now writes doubles and a point for every type, zero included, as
  the exporter does. `tools/otlpdump` prints a spooled request as the protocol's JSON.

- `claude-code-otlp` is a second adapter for the runtime: an OpenTelemetry receiver its own
  exporter is pointed at, gRPC and HTTP with protobuf on one `listen` port. Phase one lands the
  metrics requests it receives in the same spool, bytes as received, for `asz push`; logs and
  traces are accepted and dropped, counted in the status. It runs beside the local adapter under
  `asz collect` and `asz server`. `metrics` may be on for one adapter, never both, and the
  configuration refuses to load otherwise, so the same tokens are never counted twice. A receiver
  takes `listen` and `metrics` only; collector settings on it are refused, since it polls nothing.

- `export.otlp.logs` and `export.otlp.metrics` switch the two things a push sends, the landed
  files and rounds as logs and the metrics spool as metrics, each on its own, both on unless set
  off. `asz push` says which it is sending, and a configuration with both off is refused.

## Export

- `push.state` names the receiver its files were sent to, on an `endpoint` line, and each file a
  receiver rejected records of, on a `rejected` line. Both lines are kept on every save. A
  `push.state` written before this names no receiver. It is read as sent to an unknown receiver, so
  a scenario root that holds one removes nothing.
- Over HTTP, a 2xx answer counts as sent only when its body is empty or protobuf, and a redirect is
  not followed. Before, an HTML page answered with 200, or a redirect to a login page, left files
  recorded as sent that no receiver stored.

## Documentation

- `design-notes/` is removed. What its three notes described that has landed is now documented in
  these pages, checked against the code rather than copied from the notes, which were older than
  the code. The notes remain in the git history. The documentation is the one reference.
