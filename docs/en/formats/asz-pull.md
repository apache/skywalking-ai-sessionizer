# asz.pull

`asz.pull` is how one asz copies conversations from another over HTTP. A client lists the
conversations a server holds, asks it to freeze one conversation's head as a snapshot, and fetches the
snapshot's files as one archive, in ranges. The protocol is version 1, and every JSON answer but a
problem carries `"schema": "pull/1"`. asz defines and owns it, as it owns [Session Data](session-data.md),
[Session Flow](session-flow.md) and [asz.view](asz-view.md), and a server or a client written in
another language follows this page. [Pull from Another asz](../setup/remote-pull.md) says how to set
up both ends, and [Storage Root](storage-root.md#served-pulls) where each keeps what it holds.

The files travel as they are stored, byte for byte: the rounds through the head, the landed files and
skeletons through the head's `through_seq`, and every published entry of the session's provider
store. `view.hide` does not apply to them, since a masked file would no
longer match the digests its rounds carry.

## Routes

| Method | Path | Purpose |
| --- | --- | --- |
| `GET` | `/pull/v1/conversations` | one page of the conversations the server holds |
| `GET` | `/pull/v1/conversations/{conversation}/manifest` | start a freeze of the head, or poll one, until it answers the snapshot's manifest and a lease |
| `POST` | `/pull/v1/conversations/{conversation}/snapshots/{tag}/archives` | ask for an archive of some of the snapshot's files |
| `GET` | `/pull/v1/conversations/{conversation}/snapshots/{tag}/archives/{id}` | the archive's state |
| `GET` | `/pull/v1/conversations/{conversation}/snapshots/{tag}/archives/{id}/data` | one range of the archive's bytes |
| `POST` | `/pull/v1/conversations/{conversation}/snapshots/{tag}/done` | release a lease |

Below, `...` stands for `/pull/v1/conversations/{conversation}`. Each part of a path is
percent-decoded once, then checked: `{conversation}` is a [name](#names), `{tag}` a
[tag](#reading-an-answer), `{id}` 64 lower-case hexadecimal characters, and a part that fails is 400.
An unknown path under `/pull/` is 404. Another method, `HEAD` included, is 405 with `Allow` naming the
route's one method. A query on any route but the listing is 400. asz's client adds the routes to the
path of the remote's `url`, so a proxy can serve them under a prefix.

## A whole pull

Every request carries `Authorization: Bearer <token>`. Bodies are cut short with `…`.

```text
GET  /pull/v1/conversations?since=24h&limit=1
200  {"schema":"pull/1","rows":[{"id":"0438c73b","session":"0438c73b","runtime":"claude-code/1",
      "title":"Check otel-rules support","round":4,"digest":"2e77…","talks":3,"steps":41,"streams":2,
      "segments":1,"unresolved":0,"session_through_time":"2026-10-26T07:58:01.5Z"}],"skipped":[],"next":"eyJ2…"}
GET  /pull/v1/conversations/0438c73b/manifest
202  {"schema":"pull/1","conversation":"0438c73b","freeze":"9c41…","state":"building"}
GET  /pull/v1/conversations/0438c73b/manifest                   Asz-Pull-Freeze: 9c41…
200  {"schema":"pull/1","conversation":"0438c73b","session":"0438c73b","round":{…},"generation":0,
      "formats":{"sd":"sd/1","sf":"sf/1"},"files":[…],"snapshot":"20261026T081630.123456789Z","lease":"5b0e…"}
POST /pull/v1/conversations/0438c73b/snapshots/20261026T081630.123456789Z/archives
     {"lease":"5b0e…","files":["0438c73b/streams/main/transcript-20261026T075801.500000000Z-000004.sd",…]}
202  {"schema":"pull/1","snapshot":"20261026T081630.123456789Z","archive":"7d2e…","state":"building"}
GET  …/snapshots/20261026T081630.123456789Z/archives/7d2e…      Asz-Pull-Lease: 5b0e…
200  {"schema":"pull/1","snapshot":"2026…","archive":"7d2e…","state":"ready","bytes":9437184,"digest":"a3f1…"}
     ETag: "a3f1…"
GET  …/archives/7d2e…/data   Range: bytes=0-8388607         If-Match: "a3f1…"   Asz-Pull-Lease: 5b0e…
206  Content-Range: bytes 0-8388607/9437184
GET  …/archives/7d2e…/data   Range: bytes=8388608-9437183   If-Match: "a3f1…"   Asz-Pull-Lease: 5b0e…
206  Content-Range: bytes 8388608-9437183/9437184
POST …/snapshots/20261026T081630.123456789Z/done   {"lease":"5b0e…"}
204
```

## Authentication

A server serves pull routes only when `remote_server.tokens_file` names a tokens file. Then every
request to the address needs a token: the page, `/api` and the pull routes alike. Without a tokens
file the address serves no pull routes, and the page is open as before.

- **The header.** Exactly one `Authorization` header: `Bearer` in any letter case, one space, and the
  token. The server compares the SHA-256 of the token's text, exactly as sent, with each line of the
  file in constant time. Any text whose hash is listed is taken. `asz token create` makes one of `asz_` and
  43 characters of unpadded base64url. No header, another form, an unknown token, or a tokens file that
  cannot be read is 401, with `WWW-Authenticate: Bearer realm="asz"`, before the body is read.
- **The tokens file.** At most 4 MiB, one line per token: a name, one space, and the token's SHA-256
  in lower-case hexadecimal. Blank lines and lines that start with `#` are passed over, and a line may
  end in a carriage return. A name is a [name](#names) of only `A` to `Z`, `a` to `z`, `0` to `9`, `-`,
  `_` and `.`. Any other line, or a name or a hash given twice, refuses the whole file. A request that
  comes five seconds or more after the server last read the file reads it again. New content that does
  not read is reported, and the last good set kept. A file that cannot be read, or that names no
  token, refuses every request.
- **Every answer** on the address carries `Cache-Control: no-store`, refusals included, without
  `Expires` or `Pragma`. A token runs at most `remote_server.max_concurrent_per_token` requests at
  once, 2 by default, and one more is 429. A request keeps its place until its answer is sent whole.
  An authenticated `OPTIONS *` is 405 with `Allow: GET, POST`. The page and `/api` answer `GET` only.

A pull request is checked in this order: the token (401), the route (404, or 400 for a part of the
path), the method (405), a place within the token's limit (429), and whether this process serves the
root's pulls (503 `owner-wait`, or 503 while it stops, each with `Retry-After: 1`). Then the query
(400), then the route's own checks.

## Reading an answer

Every JSON answer and every request body follows these rules. A reader refuses a whole answer that
breaks one, and asz's client then stops reading that server.

- One JSON value, with only white space after it. A server ends each answer with a newline.
- Valid UTF-8. A `\u` escape of half a surrogate pair without its other half is refused.
- A member name appears once in an object, at every level, unknown members included. Two spellings
  of one name, such as `"id"` and `"\u0069d"`, are the same name. Names are case sensitive.
- A known member is never `null`, required or optional. An optional member with no value is absent.
- An answer may carry members this page does not list, and a reader passes over them. A request
  body may not. There an unknown member is 400.
- The numbers this page lists are decimal integers, with no sign, fraction or exponent. Counts,
  sizes, sequences and round numbers are unsigned 64-bit. A member it does not list is passed over
  whatever its value. `generation` is at most 9,223,372,036,854,775,807.
- A digest is a full SHA-256 as 64 lower-case hexadecimal characters, as are a lease, a freeze
  identifier and an archive identifier.
- A tag is a UTC time to the nanosecond, written `YYYYMMDDTHHMMSS.NNNNNNNNNZ` with exactly nine
  fractional digits, such as `20261026T081630.123456789Z`. Any other spelling of the same time is
  refused. Tags sort by their bytes in time order.
- Bounds, checked while the text is read: at most 16 nested objects and arrays, 4,096 members in
  one object, and 65,536 bytes in one decoded string or member name. A manifest lists at most
  1,000,000 files. A listing's `limit` is 1 to 500, and its cursor at most 4,096 bytes once decoded.

### Names

A conversation, a session, an owner and each part of a file path is a name any machine can hold. It
is valid UTF-8 that JSON encoding and decoding give back unchanged, 1 to 255 bytes, and not `.` or
`..`. It holds no character from U+0000 to U+001F or U+007F to U+009F, and none of
`< > : " | ? * \ /`. It does not end in a dot or a space, or start with `.tmp-` in any letter case.
It is not a device name in any letter case, with or without an extension, spaces before the
extension ignored: `CON`, `PRN`, `AUX`, `NUL`, `CONIN$`, `CONOUT$`, `COM1` to `COM9`, `LPT1` to
`LPT9`, or `COM` or `LPT` followed by `¹`, `²` or `³`. A file path is such names joined by `/`, at
most 1,024 bytes, with no `/` at either end.

### Problems

Every refusal is one JSON object, sent as `application/problem+json`. asz's client reads a problem
only with that content type. An answer of 408, 429, 500, 502, 503 or 504 without it, such as a
proxy's, is a status alone, tried again. Any other refusal without a problem that reads, such as a
proxy's HTML 404, stops the server.

| Member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `type` | string | yes | `about:blank`, or one of the typed problems below |
| `title` | string | yes | the text of the HTTP status |
| `detail` | string | yes | what went wrong, for a person |
| `status` | number | yes | 400 to 599. A reader refuses a problem whose `status` is not the status it came with |
| `conversation`, `freeze` | string | on `freeze-build-failed` | the conversation and the freeze polled. Absent on any other type |
| `snapshot`, `archive` | string | on `archive-build-failed` | the tag and the archive asked for. Absent on any other type |
| `attempt` | number | on both | the attempt that failed: 3 or more on `freeze-build-failed`, 1 or more on `archive-build-failed` |

asz's client stops the server on a `freeze-build-failed` or an `archive-build-failed` that does not
name what it asked for, or whose `attempt` is out of those ranges.

| `type` | Status | When |
| --- | --- | --- |
| `urn:asz:pull:owner-wait` | 503 | another process serves this root's pulls. `Retry-After: 1` |
| `urn:asz:pull:storage-state` | 500 | the server's own state could not be read or written |
| `urn:asz:pull:source-read` | 503 | a freeze could not read the evidence, and nothing showed it invalid |
| `urn:asz:pull:source-changing` | 503 | the evidence changed under each of a freeze's four reads |
| `urn:asz:pull:source-invalid` | 422 | the evidence is not valid while it holds still, or a freeze found more than 1,000,000 files |
| `urn:asz:pull:freeze-build-failed` | 500 | a freeze's worker stopped without finishing, and the next attempt would be 3. `attempt` is 3 or more |
| `urn:asz:pull:archive-build-failed` | 500 | an archive's build failed, or its worker stopped and the next attempt would be 3 or later |

A problem of one of these types under any other status breaks this page's rules. A conversation whose
rounds directory holds more entries than the bound is refused before a freeze starts, with
`storage-state`, since the server cannot list it.

A failed freeze answers the problem it failed with: a `source` problem, `freeze-build-failed`,
`storage-state`, or a plain 404, 409 or 410. Every poll of that freeze gives the same answer. Only a
GET without `Asz-Pull-Freeze` starts a new freeze.

## The listing

`GET /pull/v1/conversations` answers one page. Each parameter comes at most once. An unknown
parameter, one given twice, or a value out of range is 400.

| Parameter | Value |
| --- | --- |
| `since`, `until` | a positive duration counted back from the server's clock, made of numbers and units from `ns`, `us`, `ms`, `s`, `m` and `h`, such as `24h` or `1h30m`, or an RFC 3339 time. A conversation is kept when its last activity, the head round's `session_through_time`, is at or after `since` and before `until`. With either one, a conversation with no activity time is left out. `since` after `until` is 400 |
| `runtime` | the session's dialect, such as `claude-code/1`, compared byte for byte |
| `match` | text the title holds, compared under Unicode simple case folding, with no normalization |
| `min_talks` | at least this many talks: digits only, with no leading zero but in `0` |
| `limit` | rows on a page, 1 to 500, 50 when absent |
| `after` | the `next` of the page before |

| Member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `schema` | string | yes | `pull/1` |
| `rows` | array of rows | yes | newest activity first, then by `id` in byte order. Rows with no activity time come last |
| `skipped` | array | yes | each conversation this scan could not read, as `id` and `reason`, both strings and required. Empty when none |
| `next` | string | no | the `after` of the next page. Absent on the last page, and never empty |

| Row member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `id`, `session` | name | yes | the conversation, and the session its rounds were assembled from |
| `runtime` | string | yes | the dialect of the session's first landed transcript, else of the first landed file that names one, else empty |
| `title` | string | yes | the head round's title, empty when it has none |
| `round`, `digest` | number, digest | yes | the head round, from 1, and its commit digest |
| `talks`, `steps`, `streams`, `segments`, `unresolved` | number | yes | the head round's counts, 0 when its header leaves one out |
| `session_from_time`, `session_through_time` | string | no | RFC 3339 times from the [head round's header](session-flow.md#header): when the session began, and its last activity |
| `changes`, `lines_added`, `lines_removed`, `llm_calls`, `subagents`, `bash_runs` | number | no | the head round's counts, absent when its header does not carry them |

A row is read from the head round's header and commit, and from landed files' headers, never by
folding, so it promises nothing about its rounds' integrity. A skipped `id` is display text, never a
name to ask for, each byte that is not valid UTF-8 written `\xHH`. Every page carries all the skipped
entries its scan found, sorted by `id`, and they count toward no `limit`. A conversation with no rounds, a retired
session, or one a removal is taking is left out without a word. A root that cannot be listed is 500
`storage-state`.

**The cursor.** A client treats `next` as opaque, and sends it back with exactly the parameters it
gave for the first page, or the answer is 400. asz writes unpadded base64url of a JSON object of six
members. `v` is 1. `filters` holds the six filters as given, `""` for an absent text filter,
`min_talks` 0 and `limit` 50. `since` and `until` are the bounds the first page resolved, RFC 3339 in
UTC, or `""`, so a duration is resolved once. `time` and `id` are the last row's. A cursor that is not
canonical, lacks a member or holds another, or decodes to more than 4,096 bytes is 400, and so are
filters too large for a cursor. A head that moves between pages can show a conversation twice or pass
one over.

## The manifest

`GET .../manifest` freezes the conversation's head before it answers. The first GET carries no
`Asz-Pull-Freeze`. It joins the freeze this conversation has building, or starts one, and answers 202
with the freeze's identifier. A poll is the same GET with exactly one `Asz-Pull-Freeze` header naming
that freeze. It answers 202 while the freeze builds, then the freeze's result. A freeze whose evidence
has the same file-list digest as the conversation's newest snapshot answers that snapshot.

| Status | When | asz's client |
| --- | --- | --- |
| 202 | the freeze is building | polls again after one second |
| 200 | the freeze is ready. Each 200 grants a new lease | reads the manifest |
| 400 | a query, an `Asz-Pull-Freeze` that is not one value of 64 lower-case hexadecimal characters, or a freeze of another conversation | stops the conversation |
| 403 | the token was taken out of the tokens file before the lease was granted | stops the server |
| 404 | the conversation has no rounds, on a first GET or as a freeze's failure | stops the conversation |
| 409 | no such freeze, a ready freeze whose snapshot is gone or expired, or a freeze that failed because a removal of the session's files was in progress | drops the freeze and sends a first GET, at most three times for one conversation. Outside an archive fetch, that GET has a fresh budget |
| 410 | the freeze failed because the session was retired | stops the conversation |
| 422 | `source-invalid` | stops the conversation |
| 429 | a poll found the freeze with no worker and no free place to run one | tries again |
| 500 | `storage-state` | stops the conversation |
| 500 | `freeze-build-failed` | checks `conversation`, `freeze` and `attempt`, then sends a first GET |
| 503 | `owner-wait` | tries again |
| 503 | `source-read` or `source-changing` | tries again. To a poll it is the freeze's saved failure, so the next try is a first GET |

The server forgets a failed freeze `remote_server.snapshot_ttl` after it failed, and a ready one once
its snapshot is gone. A poll of either is then 409.

| 202 member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `schema` | string | yes | `pull/1` |
| `conversation` | name | yes | the conversation asked for. A reader refuses any other |
| `freeze` | string | yes | the freeze's identifier, 64 lower-case hexadecimal characters. A poll's answer names the freeze polled |
| `state` | string | yes | `building` |

The 200 answer, for the snapshot of the [worked example](#the-file-list-digest):

```json
{"schema":"pull/1","conversation":"0438c73b","session":"0438c73b",
 "round":{"round":1,"digest":"fbd9789139ecf53daf3c4baea3da2a5deeb5a883f520b4c2a3198dd1273afc54","through_seq":1},
 "generation":0,"formats":{"sd":"sd/1","sf":"sf/1"},
 "files":[{"file":"0438c73b/streams/main/transcript-20261020T081630.000000000Z-000001.sd","kind":"landed",
           "bytes":1834,"digest":"d38c7d7f3dc559ff67f30e29404a65716bbb3fbe99ae34b7e7e4132e7464f901","seq":1},
          {"file":"_conversations/0438c73b/rounds/r000001-fbd9789139ec.sf","kind":"round",
           "bytes":2210,"digest":"da52fe0c9181a81d7288352f43b0dcb3c6947d28fe6e0f9fd825e6cd78e996f5","round":1}],
 "snapshot":"20261026T081630.123456789Z",
 "lease":"5b0e6a2f9d1c4e8b7a3f2d6c1e9b8a7f6e5d4c3b2a1f0e9d8c7b6a5f4e3d2c1b"}
```

| 200 member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `schema` | string | yes | `pull/1` |
| `conversation` | name | yes | the conversation asked for |
| `session` | name | yes | its session, which never starts with `_` |
| `round` | object | yes | the head, with three required members: `round`, its number from 1, `digest`, its commit digest, and `through_seq`, the last landed sequence it reaches, which can be 0 |
| `generation` | number | yes | the session's removal generation when the server read it. It grows when retention removes files, and is not part of the digest |
| `formats` | object | yes | `sd` and `sf`, both required, `sd/1` and `sf/1`. A reader refuses a version it does not read |
| `files` | array | yes | 1 to 1,000,000 file entries, in strictly ascending byte order of `file` |
| `snapshot` | string | yes | the snapshot's tag |
| `lease` | string | yes | the lease this answer granted, 64 lower-case hexadecimal characters |

The **saved manifest** is the same object without `snapshot` and `lease`. Each snapshot keeps it as
its `_manifest`, at either end. A reader of a saved manifest passes over those two members.

| Entry member | Type | Kinds | Meaning |
| --- | --- | --- | --- |
| `file` | path | all | the path within the snapshot, with `/` between names |
| `kind` | string | all | `round`, `landed`, `skeleton` or `store` |
| `bytes` | number | all | the stored file's size, from 1 to 9,223,372,036,854,775,807. Empty evidence is invalid |
| `digest` | digest | all | the SHA-256 of the stored bytes |
| `round` | number | `round` | the round's number, from 1 |
| `seq` | number | `landed`, `skeleton` | the sequence, from 1 |
| `landed_digest` | digest | `skeleton` | the SHA-256 of the landed file the skeleton replaced |
| `content` | digest | `store` | the SHA-256 of the entry's bytes once decompressed |

Every member is required for the kinds it lists. A member of another kind, or an unknown kind,
refuses the manifest.

| Kind | Path |
| --- | --- |
| `round` | `_conversations/<conversation>/rounds/r<number>-<prefix>.sf`. `<number>` is six or more digits and equals `round`. `<prefix>` is the first twelve characters of the round's commit digest |
| `landed` | `<session>/streams/<owner>/<name>.sd`, `<session>/runs/<owner>/<name>.sd` or `<session>/provider_body/<name>.sd`. `<name>` is `<kind>-<tag>-<sequence>`, as [Landed files](storage-root.md#landed-files) names it. `<kind>` is `transcript`, `meta`, `journal`, `manifest`, `script`, `changes`, `execution` or `provider_body`, and `<sequence>` is six or more digits equal to `seq` |
| `skeleton` | the same places and names, ending in `.sk`. The file is one gzip member holding the [skeleton](session-data.md#skeletons) |
| `store` | `<session>/provider/tips/<generation>/<hex>.gz` or `<session>/provider/pieces/<content>.gz`, an entry of the session's [provider store](storage-root.md#provider-bodies). `<generation>` is a whole number from 1 with no leading zero, and `<hex>` 64 lower-case hexadecimal characters. The file is one gzip member |

A reader also checks the whole list. Every entry but a round is under `<session>/`. No path starts
with `_copy`, `_manifest`, `_root`, `_leases` or `_work` in any letter case, the names a snapshot
keeps for itself. No two paths, and no two directories on them, are equal under Unicode simple case
folding, so `s/streams/Main/a.sd` and `s/streams/main/b.sd` together are refused. The rounds are 1 to
the head's `round`, each once, and the head round's file is named for the head's `digest`. The
landed files and skeletons hold the sequences 1 to `through_seq`, each once.

### The file-list digest

The manifest's digest is the SHA-256 of its canonical file list. Two manifests list the same
evidence exactly when their digests are equal. The list is one line per entry, sorted ascending by
the bytes of `file`. A line holds `file`, `kind`, `seq`, `round`, `bytes`, `digest`, `landed_digest`
and `content`, in this order, separated by a tab (byte 9), and ends with a newline (byte 10), the last
line too. Numbers are decimal with no leading zero. A member the entry's kind does not have is a
single `-`. Text is its exact UTF-8 bytes, with no quotes or escapes, and nothing else is written. The
order of the entries in the answer, unknown members, `generation`, `snapshot` and `lease` change
nothing. The two entries above give these lines, `\t` standing for the tab and `\n` for the newline.
The landed file comes first, since `0` (byte 48) sorts before `_` (byte 95):

```text
0438c73b/streams/main/transcript-20261020T081630.000000000Z-000001.sd\tlanded\t1\t-\t1834\td38c7d7f3dc559ff67f30e29404a65716bbb3fbe99ae34b7e7e4132e7464f901\t-\t-\n
_conversations/0438c73b/rounds/r000001-fbd9789139ec.sf\tround\t-\t1\t2210\tda52fe0c9181a81d7288352f43b0dcb3c6947d28fe6e0f9fd825e6cd78e996f5\t-\t-\n
```

Their SHA-256, the manifest's digest, is `3b71076bf358aef2572af4a1f4f4af93ee561c7d14b95f024c3ad68ce701c209`.

## Archives

`POST .../snapshots/{tag}/archives` asks for an archive of some of the snapshot's files. Its body:

| Member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `lease` | string | yes | the lease the manifest granted |
| `files` | array of paths | yes | the files wanted, each a `file` of the manifest, each once, in any order. At least one, and no more than the manifest lists |
| `restart` | number | no | the failed attempt to start again after, from 1 |

**The identifier.** An archive is named by its set of files. Sort the paths ascending by their
bytes, write each followed by a newline (byte 10), the last too, and take the SHA-256 of those bytes
and nothing else. The two files of the worked example give
`33dbf51fc530f07d28c2e42f54ef6449c03a3a4f37cec1ee2514b0a66d734804`. One set in one snapshot is one
archive, so asking again joins it or answers its state. A client computes the identifier itself, and
refuses an answer that names another.

`GET .../snapshots/{tag}/archives/{id}` answers an archive's state, and carries exactly one
`Asz-Pull-Lease` header, the lease. Both routes answer:

| Status | When |
| --- | --- |
| 202 | the archive is building, queued or running |
| 200 | the archive is ready. `ETag` is its digest, quoted |
| 400 | a body or a header that breaks the rules, or a `restart` that does not apply |
| 403 | the lease is missing, another token's or released, or the token was taken out of the tokens file |
| 404 | a name the manifest does not list, to a POST. No such archive, to a GET |
| 410 | the snapshot is gone |
| 429 | a GET found the archive building with no worker and no free place to run one |
| 500 | `archive-build-failed`, the saved failure of the current attempt, or `storage-state` |
| 503 | another request is recording the same archive at this moment, to a POST. `Retry-After: 1` |

| Member | Type | Required | Meaning |
| --- | --- | --- | --- |
| `schema` | string | yes | `pull/1` |
| `snapshot` | string | yes | the tag asked for. A reader refuses any other |
| `archive` | string | yes | the identifier asked for. A reader refuses any other |
| `state` | string | yes | `building` with 202, `ready` with 200 |
| `bytes` | number | with 200 | the archive's size, from 1 to 9,223,372,036,854,775,807 |
| `digest` | digest | with 200 | the SHA-256 of the archive's bytes. A client refuses a ready answer whose `ETag` is not this, quoted |

**A failed archive** answers its failure to every POST and GET until a POST restarts it. Attempts
count from 1. A worker that stops is followed by the next attempt, and when that would be attempt 3
or later, the archive fails with `archive-build-failed` instead. A POST with `restart: N`:

- When the current attempt N failed, keeps that failure, starts attempt N + 1 and answers 202.
- When the current attempt is past N and the failure of N was kept, answers the current state. So the
  same restart can be sent again.
- When the current attempt is N and did not fail, is 500 `storage-state`.
- With no archive yet, or in any other case, is 400.

asz's client checks `snapshot`, `archive` and `attempt` of an `archive-build-failed`, then sends the
same POST with `restart` set to its `attempt`.

### The archive's bytes

An archive is one POSIX tar inside one gzip member. Its entries are the files asked for, in
ascending byte order of their paths. Each is a regular file named by its `file`, of its `bytes`,
holding the stored bytes exactly. A name or a size a plain tar header cannot hold goes in a PAX
extended header. asz writes mode 0444, owner and group 0, no owner names, and the Unix epoch as the
time. The archive's digest is the SHA-256 of all its bytes, the tar's end and the gzip trailer
included, and `bytes` is their count. A reader checks the whole download's size and digest first,
then reads the archive under these rules, which the server also applies to what it built:

- One gzip member, read to its checksum and trailer, with nothing after it, not even an empty member.
- The decompressed bytes stay within the sizes of the files asked for, plus 4,096 bytes for each file
  and 1,024 bytes for the end. More is an error.
- Only regular files. Each is a path asked for, once, of the size and the SHA-256 the manifest lists,
  in any order, and every path asked for is there. A link, a directory or another type is refused.
- Mode, owner and times are ignored. No header decides where a file goes. The manifest does.
- Bytes after the tar's end are read within the bound and never extracted.

Before a client uses the files, it checks the evidence itself: every landed file and skeleton read
to its end, its header bound to its path and the manifest, every store entry decompressed to its
`content`, and the rounds verified as one chain over the files, as [Reading it](session-flow.md#reading-it)
says. Every provider body record of a landed file is rebuilt, in landing order, from its own
representation against the snapshot's store, each occurrence of a repeated record too, unless the
body holds or cites a piece too large to land, which no reader can rebuild and the format allows. A
skeleton's records are checked for their structure only. A tip the store holds must be the body of
the record it is named for, when that record is in the snapshot. Matching file digests alone do not
accept a snapshot.

### Ranges

`GET .../snapshots/{tag}/archives/{id}/data` answers one range of a ready archive. It carries
exactly one of each header:

| Header | Value |
| --- | --- |
| `Asz-Pull-Lease` | the lease |
| `Range` | `bytes=<first>-<last>`: one closed range, decimal with no sign and no leading zero, `<last>` not before `<first>`, at most 8,388,608 bytes. No list, open end, suffix or space |
| `If-Match` | `"<digest>"`, the archive's digest as one quoted strong entity tag. No `*`, weak tag or list |

The server checks in this order:

1. `Range`, then `If-Match`, then `Asz-Pull-Lease`. A missing or malformed one is 400.
2. The snapshot, which is 410 when gone, then the lease, which is 403.
3. The archive: 404 when there is none, 202 with its state while it builds, 429 when it builds with
   no worker and no free place to run one, or its problem when it failed.
4. `If-Match` against the ready archive's digest. Another digest is 412, even for a range past the
   end of the archive.
5. The range against the archive's size. A `<last>` at or past the size is 416, with
   `Content-Range: bytes */<size>`.

Otherwise the answer is 206 with `Content-Type: application/gzip`, `Content-Length` of `<last>` -
`<first>` + 1, `Content-Range: bytes <first>-<last>/<size>`, `Accept-Ranges: bytes` and
`ETag: "<digest>"`, and no `Content-Encoding`. Its body is exactly bytes `<first>` to `<last>`.

asz's client sends `Accept-Encoding: identity`. It asks for ranges in order from byte 0, each of at
most 8,388,608 bytes, and keeps only a contiguous prefix. A 206 whose `ETag`, `Content-Range` or
`Content-Length` is not what it asked for, any `Content-Encoding`, a 200, or a body longer than
`Content-Length` stops the server. A body that ends early, or sends no byte for sixty seconds, is
cut: the client keeps what came, and the next range starts at the first byte it lacks and ends where
the cut one was to end. An error the connection reports with the last bytes or after them, such as
a length its frames do not match, fails the range: its bytes are dropped, and it is asked again
whole. Any other answer is read
within two minutes. After a 412 with its problem, a 202 that names the snapshot and archive asked
for, or a 500 `archive-build-failed`, it reads the state, and a changed size or digest drops the
prefix. A 500 `storage-state` stops the conversation. Once every byte is there, a size or a digest
that does not match stops the server.

## Leases and lifetime

A lease belongs to one snapshot and to the token that took it, and each 200 of the manifest route
grants a new one. A snapshot route checks the snapshot first, which is 410 when it was never
published, was removed or expired. It then checks the lease, which is 403 when it is missing, another
token's or released, or when its token was taken out of the tokens file. `done` alone answers 204 for
a lease this token released.

Each request that passes records a use of the snapshot, and a request still being answered records
one again from time to time. A snapshot expires `remote_server.snapshot_ttl` after its last use, and
no answer says when. An open lease does not keep it from expiring, and work already admitted may
finish after. A newer snapshot of the conversation supersedes the older ones, and each of those is
removed after its last open lease is released, or when it expires. The newest stays until it expires.

After a 410 while it fetches an archive, asz's client sends a first GET for the manifest again. With
the same file-list digest and the same head, it carries on with the new snapshot and lease, asking
for the archive there from byte 0. With anything else, it builds its copy again from the new
manifest, with the conversation still held, and with what is left of the archive fetch's budget. A
conversation takes at most three fresh manifests after its first.

### Releasing a lease

`POST .../snapshots/{tag}/done` releases a lease. Its body is exactly `{"lease": "<lease>"}`.

| Status | When |
| --- | --- |
| 204 | the lease is released, now or before by this token. No body |
| 400 | a body that breaks the rules |
| 403 | no such lease, or another token's |
| 410 | the snapshot is gone. asz's client counts the lease as released |

Releasing again is safe. The first release records a use of the snapshot, and a repeated one records
nothing. asz's client releases a lease once it stops using its manifest, whether it published a copy,
found the copy it holds the same, or stopped. A lease it could not release is left to expiry.

## Retries and transport

What asz's client does with an answer, beyond what each route's table says:

- A failed connection, a cut answer, 408, 429, 500, 502, 503 and 504 are tried again, after about half
  a second, then one second, then two seconds for each later try, each varied at random. A
  `Retry-After` in seconds or as an HTTP date replaces that wait, up to 60 seconds. A longer one
  stops the server.
- 401 and 403 stop the server, as do a 410 to a listing, an answer that breaks this page's rules,
  an archive that fails its checks, a file in it that is not the one the manifest lists, and a digest
  that does not match. A certificate that does not verify, and a TLS alert from the server, such as
  for a missing client certificate, stop the server at the first request. Any other refusal stops
  the listing or the conversation. A retryable answer with a problem is read as a problem, and one
  that does not read stops the server.
- A failure of the client's own storage, such as a full disk while an archive arrives, stops the
  conversation and is never tried again.
- Failures count against a budget of four. A listing page, a manifest with its polls, and a release
  each have one. An archive fetch has one, shared by every request it makes, a fresh manifest
  included, and a copy built again from a changed list goes on with what is left of it. A 412
  counts, and each failed attempt of a freeze or an archive counts once. The fourth failure stops
  the server. A poll, and a range that succeeds, count nothing.
- No redirect is followed, since a token could go with it to another host. At most two requests run
  at once for one server address and token.

HTTP/1.1 is served, and HTTP/2 when the address serves TLS and both ends support it. TLS is 1.2 or
later. With `remote_server.tls.client_ca_file`, the server requires a client certificate that
authority signed. No request waits open for a freeze or a build: a client polls. A server gives a
request body 30 seconds, and each write of at most 32 KiB of an answer 60 seconds. asz's client gives
a JSON request two minutes in all, and goes direct or through an `http://` proxy, never `https://`.
