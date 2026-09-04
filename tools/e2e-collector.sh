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

# Push a generated session into a real OpenTelemetry Collector and verify
# what its file exporter wrote. Needs docker. Used by the Collector job in
# CI and by make e2e-collector.
set -eu
cd "$(dirname "$0")/.."
IMAGE="${OTELCOL_IMAGE:-otel/opentelemetry-collector-contrib:0.158.0}"
PORT="${OTELCOL_PORT:-14318}"
WORK="$(mktemp -d)"
trap 'docker rm -f asz-e2e-otelcol >/dev/null 2>&1 || true; rm -rf "$WORK"' EXIT

./bin/asz scenario build tests/scenarios/all-kinds.yaml --format claude-code --out "$WORK/root" --at 2026-06-01T09:00:00Z --repeat 2 >/dev/null
./bin/asz collect -once -config "$WORK/root/asz.yaml" >/dev/null
./bin/asz parse -config "$WORK/root/asz.yaml" >/dev/null
./bin/asz verify -config "$WORK/root/asz.yaml" >/dev/null
cat >> "$WORK/root/asz.yaml" <<YAML
export:
  otlp:
    endpoint: http://127.0.0.1:$PORT
YAML

# The Collector configuration the export page shows, with the file exporter.
mkdir -p "$WORK/otelcol"
cat > "$WORK/otelcol/config.yaml" <<YAML
receivers:
  otlp:
    protocols:
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
: > "$WORK/otelcol/logs.json"
chmod 666 "$WORK/otelcol/logs.json"
docker run -d --rm --name asz-e2e-otelcol -p "$PORT:4318" \
  -v "$WORK/otelcol:/out" -v "$WORK/otelcol/config.yaml:/etc/otelcol/config.yaml" \
  "$IMAGE" --config /etc/otelcol/config.yaml >/dev/null
for i in $(seq 1 30); do
  if curl -s -o /dev/null "http://127.0.0.1:$PORT/v1/logs"; then break; fi
  sleep 1
done

./bin/asz push -once -config "$WORK/root/asz.yaml"
# The file exporter flushes on its own schedule; give it a moment.
for i in $(seq 1 20); do
  if [ -s "$WORK/otelcol/logs.json" ]; then break; fi
  sleep 1
done
sleep 2
go run ./tools/collectorcheck "$WORK/root" "$WORK/otelcol/logs.json"
