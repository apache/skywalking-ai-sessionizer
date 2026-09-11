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
- An `unknown` part that `claude-code-local` or `claude-code-changes` lands keeps every byte the
  dialect could not describe, as the Session Data page always said. A JSON string cannot hold
  bytes that are not valid UTF-8. Go's JSON encoder wrote U+FFFD in their place, so such a byte was
  lost while the part still said `available`. When any byte is not valid UTF-8, the part now keeps
  all of its bytes in standard base64, and carries a new key, `encoding`, set to `base64`. Valid
  UTF-8 is written as before, as one JSON string with no `encoding`, so every file landed before
  reads as it did. The schema stays `sd/1`, because a key was only added. `Part.Raw` in
  `pkg/sessiondata` reads both forms, and `Part.SetRaw` writes them. Both adapters now build the
  part with `Part.SetRaw`. The plugin's adapter, `claude-code-changes`, passed the line to the JSON
  encoder as a string, and lost such bytes the same way. Claude Code and the plugin write UTF-8,
  but such bytes can still reach an adapter. A write cut short and joined to the next line is one
  way, and a workflow script saved in another encoding is another. Measured on 2026-09-11 on one
  machine's storage root of 60 sessions, none of its 151 `unknown` parts had lost a byte. That root
  held no change records of the plugin, so the measurement covers `claude-code-local` only.
- Source JSON in a part's `data` now keeps its bytes, apart from whitespace between tokens. The
  writer inserts it after `json.Compact` instead of re-encoding it. Earlier writers changed `<`,
  `>` and `&` to `\u003c`, `\u003e` and `\u0026`, and U+2028 and U+2029 to `\u2028` and `\u2029`.
  Turning HTML escaping off alone still changes the last two under Go 1.27's JSON v2 engine.
  Inserting the compact JSON keeps `data` the same under both Go JSON engines. Unknown strings,
  derived change records and plugin lines keep their own encodings. Whitespace between tokens
  is still removed without a warning. [What data holds](../formats/session-data.md#what-data-holds)
  describes the exact boundary. On a 2026-09-12 sample of 6,377 source files from 85 sessions, all
  229,341 source JSON parts landed byte for byte, compared with 144,464 (62.99%) before. Both Go
  JSON engines gave the same result. The schema stays `sd/1`; old files and their digests stay intact.
  A data-only step can therefore show escapes from an old file and literal characters from a new
  one. The example `asz-view-example.yaml` is regenerated with the new writer. OAP's copies need
  a paired follow-up: regenerate both fixture scenarios and their example documents from the same
  asz commit, then replace their landed files and expected documents together.
- The new `parts_keep_source_bytes` scenario property compares source JSON and unknown bytes
  with landed parts and checks source digests. It runs on `claude-code` scenarios only, including
  the new `source-bytes.yaml`; it does not monitor real sources. Scenario writers now preserve
  literal `<`, `>`, `&`, U+2028 and U+2029. Workflow names produce valid run ids even when they
  start with punctuation, and a build refuses names that produce the same run id, ignoring letter
  case.
- A source Claude Code pruned now has its cursor set to `source_gone`, as the documentation said.
  A pass visits only the files discovery lists. So before, only a file that went while a pass read
  it was marked, and a pruned file's cursor said `active` forever. Measured on 2026-09-11 on one
  machine's storage root of 60 sessions, not one of 6,080 cursors said `source_gone`, while
  discovery no longer found 8 of the sessions, with 415 cursors between them, and 1 file of
  another. Now, after a session's sources, a pass sets `source_gone` on each other cursor of the
  session whose file does not exist. A session discovery no longer finds is checked the same way.
  Either way the pass holds the session's lock, and it leaves alone a session the adapter's
  `include` and `exclude` keep out. A collector checks a session discovery no longer finds until
  one check completes, and again only after discovery has found it in between. Claude Code prunes
  by age, so without that a pass would read more cursors every week. Discovery reports a project
  directory it cannot read, and a pass that met one does not check the sessions discovery did not
  find, because an unreadable directory hides files the way pruning does. Discovery does not
  report a directory it cannot read inside a session, such as `subagents/`, so such a directory
  does not stop the check. A cursor moves only when its file does not exist, so the cursor of a
  file in such a directory stays as it was. Only an `active` cursor is set to `source_gone`, and a
  `conflict` stays. A cursor that names a path this adapter does not produce, as the cursors of a
  scenario `sd` build do, is left alone. When discovery lists a file whose cursor says
  `source_gone`, restored from a backup for instance, the pass sets the cursor back to `active`
  before it reads the file. So an older copy of a transcript or a journal, shorter than what was
  read, becomes a `conflict` in that same pass. A copy that still holds every byte that was read
  is collected from where reading stopped. A file in a session that `include` and `exclude` keep
  out is not read, so its cursor stays `source_gone`. The landed files stay in every case. The
  plugin's change records are not Claude Code's files, and their cursors are still marked only
  when a file goes while a pass reads it.
  [Pruned sources](../formats/storage-root.md#pruned-sources) has the details.
- A new scenario property, `pruned_sources_gone`, checks this in every scenario in the
  `claude-code` format. On a copy of the finished root it deletes the session's main transcript,
  then every other file of the session that the Claude Code adapter finds, and then puts them all
  back with the bytes they had. After each step, one pass of `claude-code-local` and
  `claude-code-changes` runs, as a newly started process would. After a deletion, each deleted
  file's active cursor must say `source_gone`, and every other cursor must say what it said before,
  with no cursor added or removed. A cursor that already said `conflict` stays that way. After the
  files come back, every cursor must say again what it said before. No pass may land anything,
  meet a new conflict or an error, or change a landed file. A parse must write no round, and
  `asz verify` must count as many problems as it counted before. The scenario guide describes the
  property under [Check](../guides/scenario.md#check).

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
- `asz push` makes one pass and exits. A root that keeps growing is sent by `asz collect`. This
  command is for a root that is already there. `export.otlp.interval` is gone with the loop it
  drove, and the collector's `interval` is the one period.
- The `claude-code-otlp` receiver listens under `asz collect` and `asz server`, the two commands
  that write for as long as they run. It no longer listens under `asz view`.
- `asz verify` holds the first record of each stream to line 1 and byte 0. Where the stream has an
  append cursor, its records must also reach the line and the byte the cursor says the collector
  read to. Before, the check started from whatever the first record said and never read the
  cursor. So a lost first or last landed file passed unless a round had consumed it, and in a root
  never parsed, or one whose rounds were removed, nothing reported it. Now, in a stream with an
  append cursor, such a loss shows whether or not a round consumed the file. A lost first file
  shows as an `ord gap` and a `byte gap`. Records that stop short of the cursor show on a new
  `end gap` line, with the cursor's position and theirs. A stream with an append cursor whose every
  landed file is lost is checked from its cursor too. A snapshot cursor, or no cursor, is not
  compared. There are two exceptions, and in both only a round that consumed the file shows the
  loss. A lost file leaves no gap when another landed file holds the same records, as after an
  interrupted pass. A cursor never travels, so in a root rebuilt from a push the check stops at
  the last landed record, and a lost last file leaves no gap there. Which streams have an append
  cursor depends on what landed them. `claude-code-local` gives one to a transcript and a workflow
  journal, and `claude-code-changes` to the plugin's change records. `claude-code-local` reads a
  child agent's sidecar, a workflow manifest and a workflow script whole, with a snapshot cursor.
  So in a root it collected, a lost file of one of them is found only by the round chain, and a
  root with no rounds does not show it. A scenario `sd` build gives each of them an append cursor
  that counts records, so there such a loss is an end gap too. A cursor behind the records is what
  an interrupted pass leaves, and is not a problem. The cursor is read before the files are
  listed, so a collector running beside the check cannot make it look ahead. Measured on
  2026-09-11 on one machine's storage root of 60 sessions, all 6,080 streams began at line 1 and
  byte 0, and none of the 2,995 append cursors was ahead of its records. So on that root, neither
  rule reported a problem that was not there. The new `tests/scenarios/lost-ends.yaml` loses the
  first and then the last landed file of a stream. `tests/scenarios/lost-file.expect.yaml` now
  counts two problems where it counted one. The second is the end gap of the helper stream whose
  file it loses.
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
  parse to read. Only a pipeline whose storage root is the build's `--out` removes. It holds
  `_scenario/.lock` there, and a second one waits. When the configuration keeps every marked
  session, the `scenario :` line at the start says why. Removal is a policy of the scenario, never
  of the product. The configuration has no removal setting, and a session no build marked, which
  is every real Claude Code session, is never removed.
- A workflow in a scenario starts its children as one batch, and they run at the same time, as the
  runtime's do. Before, each child started only after the one before it had ended, so a review by
  four agents was drawn as four reviews in a row. The parent's next step waits for the last child.
  The `push_follows_the_wire` check now compares record times as times rather than as text, which
  the overlapping children showed it needed.

## Read

- The conversation list shows what a conversation did, not how it is built: talks, model calls,
  subagents, Bash runs, and changes with the lines those records added and removed. Steps, streams
  and segments leave the table. This follows Horizon's own conversation list, so the two read the
  same. The list folds each conversation to draw its row, and keeps the fold until the head round
  on disk moves. Talks and the time span are read from the fold. Model calls, subagents, Bash runs
  and changes are the counts in the head round's header, and a count the head round does not carry
  shows a dash: a round cut before that count existed does not know the answer, and a zero would be
  a claim it never made. Measured on 2026-09-11 on one machine's storage root of 60 conversations,
  the first list took 2.75 seconds, and a later one with no new round took 43 milliseconds.
- The conversation page is drawn by Horizon's conversation renderer,
  `@skywalking-horizon-ui/conversation-view`, embedded from a pinned Horizon commit with Horizon's
  themes and fonts, so `asz view` and the SkyWalking UI draw a conversation identically and the
  page needs nothing from the network. The pin now names Horizon `22e2f869`, which draws a tool
  call's input and result as fields rather than one escaped string, reads an edit as a line diff,
  puts a changes mark on a step that changed files with the files and their diffs behind it, adds a
  Changes tab to the inspector carrying where each record came from, and pops the inspector out
  over the page. The hand-written viewer is gone with the API routes only it read. The page reads
  the `asz.view` document, the glossary, and the landed record behind a step.
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
- `candidate` also refuses a signing key that has expired or is revoked in KEYS, the primary key
  or the subkey that signs. gpg reads a key's expiry from KEYS, not from the signer's machine, so
  a key extended there but not in KEYS passed before, and every voter's gpg would warn about it.
  Every signature, of the scratch file before the build and of each package after it, must now be
  a good signature by the checked key that gpg does not report as expired or revoked. gpg exits 0
  and writes `VALIDSIG` for such a key too, so neither was enough. A revoked key is now named as
  revoked, where before it was refused as having no apache.org user ID. An expired user ID no
  longer counts as that address. Reading the KEYS listing no longer stops `candidate` when more
  than a pipe buffer of it follows the signing key. The voters' check list and
  [Install](../setup/install.md) say what gpg must not print.
- `--dry-run` is described as it behaves. `candidate`, `publish` and `complete` read the tag to
  make their plan, so a dry run fetches `v$VERSION` from origin when the local repository does not
  have it, as a real run does. They fetch that one tag and no other, and say so. A `candidate`
  dry run signs its scratch file and reads KEYS in a temporary directory, which it removes.
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
- `tools/package-smoke.sh` reads the plugin's own record, `output/<session-id>/main.jsonl`, and
  needs it to name the file the shell command created. Before, it searched the plugin's whole data
  directory, where files of the plugin's scan name that file before the record is written, so a
  plugin that wrote no record passed. It compares the package's sha512 with the one in its
  `.sha512` itself. That file must hold exactly one line that is not blank, and that line must be
  a sha512 followed by this package's name. `sha512sum -c` checks whichever file the line names,
  and the BSD `sha512sum` on macOS passed an empty file. On Windows it hands the two paths to
  `Expand-Archive` in the environment, so an apostrophe in a path no longer stops the unpack. Each
  page request now has a time limit, and `asz view` has 20 seconds to answer.
- CI builds and tests the source package as a voter does. A new required job, `source-package`,
  makes it with `git archive`, unpacks it outside the checkout, where there is no `.git` and no
  font, and runs `go build` and `go test` there. The unit tests of the `build` job now also run
  the tests of the command and of the plugin, the plugin's hook from end to end included, on
  Linux, macOS and Windows. No CI job ran those tests before.
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
- `make release` also refuses a file git does not track when `.gitignore` ignores it. The build
  uses such files, and the source package, made from the tag, holds none of them. `asz` embeds
  every file in `internal/view/conversation-view/` whose name does not start with `.` or `_`, and
  a binary package copies the plugin's `.claude-plugin/` and `hooks/` and
  `dist-material/licenses/` whole. Only the build output, in `dist/`, `bin/` and
  `plugins/claude-code/bin/`, may be there. The binaries are built with `GOWORK=off` and
  `GOFLAGS=-mod=readonly`, so a `go.work` cannot change what they are built from. The compiled
  files it refuses now include static libraries, Go object files and archives, Java archives and
  Python byte code. `COMPILED_FILES` in the Makefile names by extension the ones `file` cannot
  tell by type: the `file` 5.41 that ships with macOS reports a WebAssembly module and a `.pyc`
  as `application/octet-stream`. `candidate` refuses the same files in the source package, and
  the voters' check list finds them too.
- `make checksums` stops at the first package it cannot checksum, removes that `.sha512`, and
  checks each `.sha512` right after writing it. Before, a failed checksum left an empty
  `.sha512`, and `make release` went on to sign every package.
- The source package holds no font file. The two fonts of the conversation renderer are under the
  SIL Open Font License, which the ASF puts in Category B, and the ASF does not allow a Category B
  work in a source release. `.gitattributes` marks them `export-ignore`, so `git archive` leaves
  them out, and `make release` stops and removes a source package that holds a font file anyway.
  The binaries in the binary packages embed the fonts, and `dist-material/LICENSE` names them. A
  build from the source package draws the page with system fonts.
- `make conversation-view-check` passes in an unpacked source package. That package leaves out
  the renderer's two fonts, so there the check leaves them out of its comparison, names them, and
  compares every other file. Before, it failed in every source package and told the voter to
  commit an update. In a clone of the tag it still compares the fonts, and the vote mail says
  which files it compares.
- The `LICENSE` and `licenses/` of a binary package name only the modules built into `asz` and
  the plugin. `github.com/kr/text` and `github.com/rogpeppe/go-internal`, which only the tests of
  `gopkg.in/yaml.v3` need, are gone from them, because the ASF asks the `LICENSE` of a package to
  account for exactly what it holds. `make dep-licenses` and `make dep-licenses-check` take the
  modules for the `LICENSE` and the `NOTICE` alike from `go list -deps` of both binaries, on every
  platform in `PLATFORMS`.
- Two builds of one tag give the same binary packages, byte for byte, when they use the same Go,
  the same tar and the same gzip. Go records the tag in each binary as the module's version, so a
  build of the commit before it was tagged differs. Every file has the commit's time, its owner
  and group are 0, and the entries are sorted. No package records who built it or when. GNU tar
  and bsdtar write their headers differently, and GNU gzip and Apple's gzip compress differently,
  so a CI build, made with GNU tar and GNU gzip, and a macOS build, made with bsdtar and Apple's
  gzip, differ.
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
  which producer observed it: the Claude Code adapter copies the runtime's own patch from every
  `Edit` and `Write` result on the main stream into a second `data` part beside the raw result,
  and the new asz Claude Code plugin, `plugins/claude-code/`, records shell commands, and edits
  inside subagents, into its own data directory. The plugin runs inside Claude Code's hooks, one
  short process per event, needs nothing from asz, classifies read-only commands to skip their
  scans, excludes every known language's build output by a frozen rule set, names overlapping
  windows on the same root, and keeps its output for 30 days. The new `claude-code-changes`
  adapter, on by default, tails the plugin's files and lands each line as a record of kind
  `changes` under the stream the tool ran in, with the session's own lock and sequence, and the
  pipeline refreshes both sources in one pass. Session Data and Session Flow are unchanged:
  `changes` is a new kind, assembly emits no node for it, and a session folds to the same nodes
  with and without the records.
- `asz.view` gains `workspace_changes`, every record joined to its step. A tool step names its
  records under `changes`, and `summary.changes` counts them. The version stays 1.0: no official
  release carries the format yet, so the keys are added rather than a version raised.
- The plugin ships in every binary package under `claude-code-plugin/`, and `make build` builds it
  beside `asz`.
- The plugin's hooks give Claude Code the binary as the command and `hook` as its one argument, in
  the hook's `args`. Claude Code's hooks reference says Claude Code then starts the command
  directly, with no shell, and that no shell splits the command into words on any platform.
  Claude Code 2.1.260 on macOS ran the hooks in this form, and the plugin's record named the file a
  shell command created. The command line the hooks gave before went to a shell. The hooks
  reference says that on Windows without Git Bash, that shell is PowerShell. On macOS, PowerShell
  7.5.3 refused that line, a quoted path followed by a word, and the binary did not start. Claude
  Code has not run either form on Windows. With no argument and its event on standard input, the
  binary now runs as `hook`, so a Claude Code that drops the argument does not block a tool with
  the usage text's exit status 2.
  [The hook command](../setup/claude-code-plugin.md#the-hook-command) describes the form, and
  [What was verified](../setup/claude-code-plugin.md#what-was-verified) says what is still unknown
  on Windows.

## Metrics

- `claude-code-local` with `metrics: true` derives a reconstructed subset of the runtime's own
  metric family from the landed files. Phase one derives `claude_code.token.usage`, under the name
  Claude Code's OpenTelemetry exporter uses. The usage of a call is its last fragment's in line
  order, and only a finished call counts. Points are per minute, by type, model, query source and
  session. The exporter's account, speed, effort and attribution labels and its auxiliary calls
  are not reconstructed, and the export page states the difference. The points wait in the
  storage root's `_metrics/` spool. They are sent over the metrics service, under asz's identity,
  with the same budget and once-only rule as the files, at the end of each pass of `asz collect`
  and `asz server`, and by `asz push`. `metrics_lookback`, 24 hours unless set, bounds the first
  derivation over a root with history, and so does the newest request the receiver landed, so a
  switch of source counts nothing twice. A pass is deterministic, names its requests after their
  landed files and saves its state once, so a pass cut short is run again to the same bytes.
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
  metrics requests it receives in the same spool, bytes as received, and they are sent from there
  as the derived points are. Logs and traces are accepted and dropped, counted in the status. It
  runs beside the local adapter under `asz collect` and `asz server`, and not with `-once`.
  `metrics` may be on for one adapter, never both, and the configuration refuses to load
  otherwise, so the same tokens are never counted twice. A receiver takes `listen` and `metrics`
  only. Collector settings on it are refused, since it polls nothing.
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
- Conversation Assembly now gives the size of the index that
  `design-notes/02-parse-and-assembly.md` measured: 47 MB of index for 1.0 GB of landed payload,
  about 21 times smaller. The note, as the tag `v0.2.0` holds it, said about 12 times for the same
  two numbers.
