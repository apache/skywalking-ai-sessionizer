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

# Push a generated session into a real OpenTelemetry Collector, once over
# each transport, and verify what its file exporter wrote. Needs docker.
# Used by the Collector job in CI and by make e2e-collector.
set -eu
cd "$(dirname "$0")/../.."
IMAGE="${OTELCOL_IMAGE:-otel/opentelemetry-collector-contrib:0.158.0}"
GRPC_PORT="${OTELCOL_GRPC_PORT:-14317}"
HTTP_PORT="${OTELCOL_HTTP_PORT:-14318}"
WORK="$(mktemp -d)"
trap 'docker rm -f asz-e2e-otelcol >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

# The Collector configuration is read from the export page itself: the first
# YAML block under "See what a receiver gets". A copy kept here once added a
# metrics pipeline the page did not have, so CI passed while a person who
# followed the page saw asz push fail.
page=docs/en/setup/export-otlp.md
mkdir -p "$WORK/otelcol"
# The block must open and close inside that section; anything else is a page
# whose shape changed, said here rather than as a Collector that fails later.
awk '
  /^## / { if (inside) exit 2; section = ($0 == "## See what a receiver gets"); next }
  section && !inside && $0 == "```yaml" { inside = 1; next }
  inside && $0 == "```" { closed = 1; exit }
  inside { print }
  END { if (!closed) exit 2 }
' "$page" > "$WORK/otelcol/config.yaml" && [ -s "$WORK/otelcol/config.yaml" ] ||
  { echo "$page has no closed YAML block under \"See what a receiver gets\""; exit 1; }

# push_over PROTOCOL ENDPOINT: build a session, start a Collector, push the
# session over the transport named, and check what the Collector wrote.
push_over() {
  protocol="$1"; endpoint="$2"
  root="$WORK/$protocol"
  # Two scenarios in one root: every kind of file the export page names,
  # and a session whose original lost records, so what the Collector gets
  # back is a session with open references, rebuilt and verified as such.
  ./bin/asz scenario build tests/scenarios/all-kinds.yaml --format claude-code --out "$root" --at 2026-06-01T09:00:00Z --repeat 2 >/dev/null
  ./bin/asz scenario build tests/scenarios/lost-records.yaml --format claude-code --out "$root" --at 2026-06-01T10:00:00Z >/dev/null
  ./bin/asz collect -once -config "$root/asz.yaml" >/dev/null
  ./bin/asz parse -config "$root/asz.yaml" >/dev/null
  ./bin/asz verify -config "$root/asz.yaml" >/dev/null
  cat >> "$root/asz.yaml" <<YAML
export:
  otlp:
    protocol: $protocol
    endpoint: $endpoint
YAML

  : > "$WORK/otelcol/logs.json"
  chmod 666 "$WORK/otelcol/logs.json"
  docker run -d --rm --name asz-e2e-otelcol -p "$GRPC_PORT:4317" -p "$HTTP_PORT:4318" \
    -v "$WORK/otelcol:/out" -v "$WORK/otelcol/config.yaml:/etc/otelcol/config.yaml" \
    "$IMAGE" --config /etc/otelcol/config.yaml >/dev/null
  # The Collector says when both receivers are up. Docker's port proxy
  # accepts a connection before that, so a probe of the port is not enough.
  for i in $(seq 1 30); do
    if docker logs asz-e2e-otelcol 2>&1 | grep -q "Everything is ready"; then break; fi
    sleep 1
  done
  sleep 1

  echo "== push over $protocol to $endpoint"
  # A refused pass leaves every file for the next one, so a pass that meets
  # a receiver still settling is simply run again.
  for i in $(seq 1 5); do
    if ./bin/asz push -once -config "$root/asz.yaml"; then break; fi
    if [ "$i" = 5 ]; then echo "push over $protocol failed five times"; exit 1; fi
    sleep 2
  done

  # The file exporter flushes on its own schedule; give it a moment.
  for i in $(seq 1 20); do
    if [ -s "$WORK/otelcol/logs.json" ]; then break; fi
    sleep 1
  done
  sleep 2
  docker rm -f asz-e2e-otelcol >/dev/null
  go run ./tools/test/collector-check "$root" "$WORK/otelcol/logs.json"
}

push_over grpc "127.0.0.1:$GRPC_PORT"
push_over http "http://127.0.0.1:$HTTP_PORT"
