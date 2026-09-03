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

# The binary is cross-compiled on the build host: Go builds every target
# platform from one machine, so a multi-platform image needs no emulation
# and no third-party build action.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
# The version the binary reports. CI passes the release version or the
# commit; a local build says dev.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/asz ./cmd/asz

# Static binary, no shell, non-root. The image reads and writes only its
# storage root, mounted at /asz/data, which is also the default the binary
# resolves when run from /asz.
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.title="SkyWalking AI Sessionizer" \
      org.opencontainers.image.description="Conversation-level observability for long-lived AI agents" \
      org.opencontainers.image.source="https://github.com/apache/skywalking-ai-sessionizer" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/asz /usr/local/bin/asz
WORKDIR /asz
VOLUME ["/asz/data"]
EXPOSE 8787
ENTRYPOINT ["/usr/local/bin/asz"]
# Inside a container the page must listen on every interface, not loopback.
CMD ["view", "0.0.0.0:8787"]
