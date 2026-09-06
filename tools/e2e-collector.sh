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
cd "$(dirname "$0")/.."
IMAGE="${OTELCOL_IMAGE:-otel/opentelemetry-collector-contrib:0.158.0}"
GRPC_PORT="${OTELCOL_GRPC_PORT:-14317}"
HTTP_PORT="${OTELCOL_HTTP_PORT:-14318}"
WORK="$(mktemp -d)"
trap 'docker rm -f asz-e2e-otelcol >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

# The Collector configuration the export page shows, with the file exporter.
mkdir -p "$WORK/otelcol"
cat > "$WORK/otelcol/config.yaml" <<YAML
receivers:
  otlp:
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        max_recv_msg_size_mib: 32
      http:
        endpoint: 0.0.0.0:4318
exporters:
  file:
    path: /out/logs.json
service:
  pipelines:
    logs:
      receivers: [otlp]
      exporters: [file]
YAML

# push_over PROTOCOL ENDPOINT: build a session, start a Collector, push the
# session over the transport named, and check what the Collector wrote.
push_over() {
  protocol="$1"; endpoint="$2"
  root="$WORK/$protocol"
  ./bin/asz scenario build tests/scenarios/all-kinds.yaml --format claude-code --out "$root" --at 2026-06-01T09:00:00Z --repeat 2 >/dev/null
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
  go run ./tools/collectorcheck "$root" "$WORK/otelcol/logs.json"
}

push_over grpc "127.0.0.1:$GRPC_PORT"
push_over http "http://127.0.0.1:$HTTP_PORT"
