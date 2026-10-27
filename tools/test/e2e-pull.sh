#!/bin/sh
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

# Pull conversations from asz servers in containers, over TLS and with a
# token, and check what each refuses: an untrusted certificate, a missing
# client certificate, a missing, unknown, changed or revoked token. Then put
# a shared cache in front of the page and check it never answers for a
# token. Needs docker and openssl. Used by the Pull job in CI and by
# make e2e-pull.
set -eu
cd "$(dirname "$0")/../.."
export ASZ_BASE_IMAGE="${ASZ_BASE_IMAGE:-gcr.io/distroless/static-debian12:nonroot}"
export NGINX_IMAGE="${NGINX_IMAGE:-nginx:1.29.3-alpine}"
export TLS_PORT="${E2E_TLS_PORT:-18788}"
export MTLS_PORT="${E2E_MTLS_PORT:-18789}"
export CACHE_PORT="${E2E_CACHE_PORT:-18790}"
export E2E_UID="$(id -u)" E2E_GID="$(id -g)"
WORK="$(mktemp -d)"
export WORK
ASZ="$PWD/bin/asz"
compose() { docker compose -p asz-e2e-pull -f tools/test/pull/compose.yaml "$@"; }
trap 'compose logs >"$WORK/compose.log" 2>&1 || true; compose down -v >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

failed=0
check() { # check NAME WANT GOT
  if [ "$2" = "$3" ]; then echo "ok   $1"; else echo "FAIL $1: want $2, got $3"; failed=1; fi
}

# Two authorities: one signs the servers' and the client's certificates, the
# other signs nothing a client is told to trust.
cd "$WORK"
for ca in ca other; do
  openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj "/CN=asz e2e $ca" \
    -keyout "$ca.key" -out "$ca.crt" 2>/dev/null
done
openssl req -newkey rsa:2048 -nodes -subj "/CN=localhost" -keyout server.key -out server.csr 2>/dev/null
printf 'subjectAltName=DNS:localhost,IP:127.0.0.1\n' > server.ext
openssl x509 -req -in server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile server.ext -out server.crt 2>/dev/null
openssl req -newkey rsa:2048 -nodes -subj "/CN=asz e2e client" -keyout client.key -out client.csr 2>/dev/null
printf 'extendedKeyUsage=clientAuth\n' > client.ext
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 1 -extfile client.ext -out client.crt 2>/dev/null
cd - >/dev/null

# The servers run this tree's asz, built for the containers' platform.
CGO_ENABLED=0 GOOS=linux GOARCH="$(docker version --format '{{.Server.Arch}}')" go build -o "$WORK/asz" ./cmd/asz

# One root, built and parsed here, then copied for each server onto the
# volume: a root's pulls belong to one serving process.
"$ASZ" scenario build tests/scenarios/provider-bodies.yaml --format claude-code --out "$WORK/built" --at 2026-06-01T09:00:00Z >/dev/null
"$ASZ" collect -once -config "$WORK/built/asz.yaml" >/dev/null
"$ASZ" parse -config "$WORK/built/asz.yaml" >/dev/null
conversation="$(ls "$WORK/built/_conversations")"

# A token for the client, and one to revoke.
token="$("$ASZ" token create -file "$WORK/tokens" e2e | sed -n 's/^token //p')"
other="$("$ASZ" token create -file "$WORK/tokens" revoked | sed -n 's/^token //p')"
printf '%s\n' "$token" > "$WORK/e2e.token"
printf '%s\n' "$other" > "$WORK/revoked.token"
chmod 600 "$WORK/e2e.token" "$WORK/revoked.token"

server_config() { # server_config NAME TOKENS TLS CLIENT_CA
  {
    printf 'storage:\n  root: /roots/%s\nremote_server:\n  tokens_file: "%s"\n  tls:\n' "$1" "$2"
    if [ "$3" = yes ]; then
      printf '    cert_file: /work/server.crt\n    key_file: /work/server.key\n'
    fi
    if [ -n "$4" ]; then
      printf '    client_ca_file: %s\n' "$4"
    fi
  } > "$WORK/$1.yaml"
}
server_config tls /work/tokens yes ""
server_config mtls /work/tokens yes /work/ca.crt
server_config plain /work/tokens no ""
server_config open "" no ""

compose up -d >/dev/null
# Ready when each server with tokens serves its pulls, which comes after it
# owns its root's, and the cache answers.
for i in $(seq 1 30); do
  if [ "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$WORK/ca.crt" -H "Authorization: Bearer $token" "https://localhost:$TLS_PORT/pull/v1/conversations")" = 200 ] &&
     [ "$(curl -s -o /dev/null -w '%{http_code}' --cacert "$WORK/ca.crt" --cert "$WORK/client.crt" --key "$WORK/client.key" -H "Authorization: Bearer $token" "https://localhost:$MTLS_PORT/pull/v1/conversations")" = 200 ] &&
     curl -s -o /dev/null "http://127.0.0.1:$CACHE_PORT/open/"; then break; fi
  sleep 1
done

# The client's configuration: the server as it is, the same server trusted
# through the wrong authority, the server that wants a client certificate
# reached with and without one, and a token that is revoked below.
client="$WORK/client"
mkdir -p "$client"
cat > "$client/asz.yaml" <<YAML
storage:
  root: $client/data
remotes:
  - name: prod
    url: https://localhost:$TLS_PORT
    token_file: $WORK/e2e.token
    ca_file: $WORK/ca.crt
  - name: untrusted
    url: https://localhost:$TLS_PORT
    token_file: $WORK/e2e.token
    ca_file: $WORK/other.crt
  - name: mutual
    url: https://localhost:$MTLS_PORT
    token_file: $WORK/e2e.token
    ca_file: $WORK/ca.crt
    client_cert_file: $WORK/client.crt
    client_key_file: $WORK/client.key
  - name: nocert
    url: https://localhost:$MTLS_PORT
    token_file: $WORK/e2e.token
    ca_file: $WORK/ca.crt
  - name: later
    url: https://localhost:$TLS_PORT
    token_file: $WORK/revoked.token
    ca_file: $WORK/ca.crt
YAML
pull() { "$ASZ" pull -config "$client/asz.yaml" "$@" >"$WORK/out" 2>&1 && echo 0 || echo $?; }
# check_pull NAME WANT ARGS...: a pull's exit status, and what it said when
# that is not the one wanted.
check_pull() {
  name="$1" want="$2"
  shift 2
  got="$(pull "$@")"
  check "$name" "$want" "$got"
  [ "$want" = "$got" ] || sed 's/^/     /' "$WORK/out"
}

echo "== pull over TLS with a token"
check_pull "a listing names the conversation" 0 -list -remote prod
grep -q "prod/$conversation" "$WORK/out" || { echo "FAIL the listing: $(cat "$WORK/out")"; failed=1; }
check_pull "a pull" 0 "prod/$conversation"
check_pull "a second pull is unchanged" 0 "prod/$conversation"
grep -q unchanged "$WORK/out" || { echo "FAIL not unchanged: $(cat "$WORK/out")"; failed=1; }
snapshot="$(ls -d "$client/data/_remote/prod/$conversation"/2*)"
printf 'storage:\n  root: %s\n' "$snapshot" > "$WORK/snapshot.yaml"
check "the pulled snapshot verifies" 0 "$("$ASZ" verify -config "$WORK/snapshot.yaml" >/dev/null 2>&1 && echo 0 || echo $?)"
check_pull "a client certificate the server asks for" 0 "mutual/$conversation"

echo "== refusals"
check_pull "an untrusted certificate" 1 "untrusted/$conversation"
grep -qi certificate "$WORK/out" || { echo "FAIL said: $(cat "$WORK/out")"; failed=1; }
check_pull "no client certificate" 1 "nocert/$conversation"
status() { curl -s -o /dev/null -w '%{http_code}' --cacert "$WORK/ca.crt" "$@"; }
list="https://localhost:$TLS_PORT/pull/v1/conversations"
check "no token" 401 "$(status "$list")"
check "an unknown token" 401 "$(status -H "Authorization: Bearer $(printf '%064d' 0)" "$list")"
changed="$(printf '%s' "$token" | sed 's/.$/x/')"
check "a token with one byte changed" 401 "$(status -H "Authorization: Bearer $changed" "$list")"
check "a valid token" 200 "$(status -H "Authorization: Bearer $token" "$list")"
check "plain HTTP to the TLS address" 400 "$(curl -s -o /dev/null -w '%{http_code}' "http://localhost:$TLS_PORT/")"
check_pull "the revoked token works before" 0 "later/$conversation"
grep -v '^revoked ' "$WORK/tokens" > "$WORK/tokens.new" && mv "$WORK/tokens.new" "$WORK/tokens"
sleep 6
check_pull "a revoked token" 1 "later/$conversation"

echo "== a shared cache in front of the page"
cache="http://127.0.0.1:$CACHE_PORT"
asset="$(curl -s "$cache/open/" | grep -o '/assets/conversation-view/[^"]*' | head -1)"
[ -n "$asset" ] || { echo "FAIL no asset on the open page"; failed=1; }
hit() { curl -s -o /dev/null -D - "$@" | tr -d '\r' | sed -n 's/^X-Cache: //p'; }
for path in "$asset" /logo.svg; do
  hit "$cache/open$path" >/dev/null
  check "the cache keeps $path of a page without tokens" HIT "$(hit "$cache/open$path")"
  check "$path with a token" 200 "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $token" "$cache/plain$path")"
  check "$path again without one" 401 "$(curl -s -o /dev/null -w '%{http_code}' "$cache/plain$path")"
  check "$path with an unknown token" 401 "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(printf '%064d' 0)" "$cache/plain$path")"
  check "$path with the revoked token" 401 "$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $other" "$cache/plain$path")"
done

exit $failed
