# Pull from Another asz

An asz that collects a product's agents serves its conversations. On your own machine, `asz pull`
copies the ones you choose, so you can judge them and read them here. A copy is a snapshot of the
conversation's head, checked against the digests its rounds carry. It never changes, and it expires.

## How it is deployed

Set up as below, the server answers pulls over HTTPS, where TLS encrypts the connection, and a token
goes with every request. Both are set on the server, as [Tokens and TLS](#tokens-and-tls) says.

```text
 Where the agents run, in a private network
┌────────────────────────────────────────────────────────────────────────────────┐
│                                                                                │
│  agents ──── traces, sent over the network ────────────►  asz server           │
│     │                                                     collects what it is  │
│     └── transcripts and file changes, written ──┐         sent and what it     │
│         to a shared volume                      └──────►  finds on the volume, │
│                                                           into its storage     │
│                                                           root                 │
│                                                                                │
│                                     one address, 8788: the page and the pull   │
│                                     routes, a token on every request, TLS      │
└──────────────────────────────────────────────┬─────────────────────────────────┘
                                               │  HTTPS, encrypted by TLS
                                               │  Authorization: Bearer TOKEN
                                               │  a client certificate, when asked
┌──────────────────────────────────────────────┴─────────────────────────────────┐
│ Your machine                                                                   │
│                                                                                │
│  asz pull -list                           what the server holds                │
│  asz pull prod/CONVERSATION               a snapshot under                     │
│                                           _remote/prod/CONVERSATION/TAG/       │
│  asz judge -remote-conversation           judges a snapshot here               │
│      prod/CONVERSATION                                                         │
│  asz view                                 pulled conversations beside your own │
└────────────────────────────────────────────────────────────────────────────────┘
```

The server is the asz that already collects the agents' conversations: one process, `asz server`, or
`asz view` beside a collector. It answers pulls on the same address as its page. A load balancer or
a proxy may stand between the two ends; a shared cache there never answers a request that carries a
token.

## Serve pulls

The server needs a certificate and its key, in PEM, for the host name your machine will use in the
`url`. If your own authority signs it, rather than one the system trusts, each machine that pulls
needs that authority's certificate too.

Make a directory the user that runs asz owns, and a token for each person or machine that pulls:

```sh
sudo install -d -o "$USER" -m 700 /etc/asz
asz token create -file /etc/asz/tokens alice
```

It prints the token once, on the line that starts with `token `, and adds the token's SHA-256 to the
tokens file. Give the token, the text after `token `, to Alice. To revoke it, delete its line: the
server reads the file again within five seconds.

The server's `asz.yaml`:

```yaml
storage:
  root: /var/lib/asz/data
remote_server:
  tokens_file: /etc/asz/tokens
  tls:
    cert_file: /etc/asz/server.crt
    key_file: /etc/asz/server.key
```

Then serve, with `asz server` or `asz view`, on an address others can reach:

```sh
asz server -config /etc/asz/asz.yaml 0.0.0.0:8788
```

With a tokens file, every request needs a token, the page and the requests it makes too. One process
serves a root's pulls at a time: a second `asz view` on the same root serves the page and waits to
take over the pulls. [Configuration](configuration.md#remote_server) lists every key.

In a container, keep the storage root on a volume. A directory shared from a macOS host into
Docker Desktop can fail asz's checks of its own directories.

## Pull

On your machine, save the token in a file only you can read. Paste it, then press Ctrl-D:

```sh
mkdir -p ~/.asz && chmod 700 ~/.asz
(umask 077 && cat > ~/.asz/prod.token)
```

Name the server in `asz.yaml`:

```yaml
storage:
  root: ~/asz-data
remotes:
  - name: prod
    url: https://asz.example.internal:8788
    token_file: ~/.asz/prod.token
    # Only when your own authority signed the server's certificate.
    ca_file: ~/.asz/ca.crt
```

List what it holds, newest activity first:

```sh
asz pull -list
asz pull -list -remote prod -since 24h -match parser
```

`LOCAL` is the head round of the newest unexpired snapshot here that admits a reader. Only the
snapshot's own records are read for it, not its files. A snapshot whose records do not read is said
and passed over to the next older one. `?` means the conversation's copy here is damaged itself, its
own records or its directories, and makes the command exit 1. Each `-list` prints one page. When a
remote has more, the last line gives the command for the next page, with the same filters.

Pull one or more conversations, by the identifier the listing shows after `prod/`:

```sh
asz pull prod/7722b6a5-2f0b-4a7e-9a8b-4b4f1e9f6c11
```

It prints each pull's snapshot as `prod/CONVERSATION:TAG`, and `unchanged` after it when the newest
unexpired snapshot here already holds the same files under the same head. A pull reuses what the
newest valid snapshot here holds, and fetches the rest. It exits 0 when every pull succeeded, 1 when
any failed, and 2 when its arguments are wrong. When one conversation fails, the next goes on. When
the remote itself fails, such as a refused token or certificate, the remaining pulls from that
remote stop, and the other remotes go on.

| `-list` flag | Meaning |
| --- | --- |
| `-remote NAME` | List this remote only. Repeat for several. Default: every remote. |
| `-since T`, `-until T` | Last activity at or after, or before: a duration back from now, such as `24h`, or an RFC 3339 time. |
| `-runtime R` | The session's runtime, such as `claude-code/1`. |
| `-match TEXT` | Text in the title, letter case ignored. |
| `-min-talks N` | At least this many talks. |
| `-limit N` | Rows per remote, 1 to 500. Default: 50. |
| `-after CURSOR` | The next page, with exactly one `-remote`. |

## Tokens and TLS

Every request to a server with a tokens file carries a token: `Authorization: Bearer TOKEN`. A
request with none, or with one that is not in the file, gets 401. The server keeps only each token's
SHA-256.

TLS encrypts the connection. Without `cert_file` and `key_file` the server speaks plain HTTP, and
tokens travel in clear text; asz warns when that address is not a loopback one. With TLS, your
machine always checks the server's certificate: against the system's authorities, or against
`ca_file` when it is set. Set `server_name` when the certificate names another host than the one in
`url`, as through a tunnel. asz follows no redirect, so a token never goes to another host, and it
uses an `http://` proxy from the environment, never an `https://` one.

A token can come from a file, `token_file`, or from an environment variable, `token_env`: exactly
one of the two.

To require a client certificate as well, set `client_ca_file` on the server:

```yaml
remote_server:
  tokens_file: /etc/asz/tokens
  tls:
    cert_file: /etc/asz/server.crt
    key_file: /etc/asz/server.key
    client_ca_file: /etc/asz/clients-ca.crt
```

and give each machine that pulls its certificate and key, beside its token:

```yaml
remotes:
  - name: prod
    url: https://asz.example.internal:8788
    token_env: ASZ_PROD_TOKEN
    ca_file: ~/.asz/ca.crt
    client_cert_file: ~/.asz/client.crt
    client_key_file: ~/.asz/client.key
```

A machine without a certificate that authority signed is refused before any request is answered, and
its pull stops.

## Judge a pulled conversation

Judging needs an evaluator, as it does for a local conversation: by default the `claude` program,
installed and signed in. [Judge](judge.md) says how to use another.

Give the pulled conversation in place of a conversation identifier:

```sh
asz judge -remote-conversation prod/7722b6a5-2f0b-4a7e-9a8b-4b4f1e9f6c11
asz judge -remote-conversation prod/7722b6a5-2f0b-4a7e-9a8b-4b4f1e9f6c11:20261025T103200.123456789Z
asz judge -pull -remote-conversation prod/7722b6a5-2f0b-4a7e-9a8b-4b4f1e9f6c11
```

Without a tag, or with `:latest`, it judges the newest unexpired snapshot here, and fails when that
snapshot is damaged. Without `-pull` nothing is asked of the server, so a remote no longer
configured does not stop you judging what is here. A run lives in the snapshot it judged.

`-pull` pulls the head first and judges what it pulled, as a new run. So it takes no fixed tag,
though `:latest` is allowed, and no `-list`, `-show` or `-regroup`, which select runs already made.
Every other judge flag works as it does on a local conversation.

`-list` without a tag lists the runs of every unexpired snapshot, each as
`prod/CONVERSATION:TAG run 3`. `-show` and `-regroup` take a run's number within the selected
snapshot.

## Read a pulled conversation

`asz view` and `asz server` list the conversations pulled into their root beside their own, each
from its newest unexpired snapshot. Its title starts with `[remote, prod]`.

## Where copies live

Copies of conversation `CONVERSATION` from remote `prod` live under `_remote/prod/CONVERSATION/` in
the storage root, one directory per snapshot, named by its tag. A snapshot and every judge run in it
expire `remote_snapshot_ttl` after it was published, `720h` by default. `asz pull` and `asz judge
-remote-conversation` remove the expired snapshots they find when they start. They leave one a
reader still holds, one whose records do not read, and every snapshot of a conversation another
command holds at that moment. Collection, parsing and sending refuse a snapshot as their root.
Judging writes inside one, a run in its own directory. [asz.pull](../formats/asz-pull.md) describes
what travels between the two.
