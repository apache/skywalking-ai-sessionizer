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

BINARY      := asz
BIN_DIR     := bin
GO          := go

# The version the binary reports. A tagged checkout says 0.1.0, anything
# else says the nearest tag and commit, and -dirty when the tree has changes.
# The tag carries a v, as Go modules require; the version does not, so that
# a local build, a release build and the image all print the same thing.
# Pass VERSION=0.1.0 to override, which is also what a release does.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
LDFLAGS     := -X main.version=$(VERSION)

RELEASE_NAME := apache-skywalking-ai-sessionizer-$(VERSION)-src

# The platforms a release ships binaries for. Every one is cross-compiled
# from any host: the binary is pure Go and needs no C toolchain.
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64
DIST      := dist
PKG_BASE  := apache-skywalking-ai-sessionizer-$(VERSION)-bin

GOLANGCI_LINT_VERSION := v1.64.8
LICENSE_EYE_VERSION   := v0.9.0

.DEFAULT_GOAL := check

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

## build: compile the binary into ./bin
.PHONY: build
build: $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)

## test: run the whole suite, unit and end-to-end
.PHONY: test
test:
	$(GO) test -race -count=1 ./...

## test-e2e: run only the end-to-end adapter tests against the fixture corpus
.PHONY: test-e2e
test-e2e:
	$(GO) test -race -count=1 -v ./tests/...

## coverage: run tests with a coverage profile
.PHONY: coverage
coverage:
	$(GO) test -race -count=1 -coverprofile=coverage.txt -covermode=atomic ./...

## fmt: format the tree
.PHONY: fmt
fmt:
	$(GO) fmt ./...

## vet: run go vet
.PHONY: vet
vet:
	$(GO) vet ./...

$(BIN_DIR)/golangci-lint: $(BIN_DIR)
	@echo "installing golangci-lint $(GOLANGCI_LINT_VERSION)"
	@GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

## lint: run golangci-lint
.PHONY: lint
lint: $(BIN_DIR)/golangci-lint
	$(BIN_DIR)/golangci-lint run ./...

$(BIN_DIR)/license-eye: $(BIN_DIR)
	@echo "installing license-eye $(LICENSE_EYE_VERSION)"
	@GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install github.com/apache/skywalking-eyes/cmd/license-eye@$(LICENSE_EYE_VERSION)

## license-check: verify every source file carries the Apache-2.0 header
.PHONY: license-check
license-check: $(BIN_DIR)/license-eye
	$(BIN_DIR)/license-eye header check

## license-fix: insert missing license headers
.PHONY: license-fix
license-fix: $(BIN_DIR)/license-eye
	$(BIN_DIR)/license-eye header fix

## dep-check: validate the licenses of every dependency
.PHONY: dep-check
dep-check: $(BIN_DIR)/license-eye
	$(BIN_DIR)/license-eye dependency check

## tidy: verify go.mod and go.sum are current
.PHONY: tidy
tidy:
	$(GO) mod tidy
	@git diff --exit-code go.mod go.sum || (echo "go.mod/go.sum are not tidy; run 'make tidy' and commit"; exit 1)

## check: everything CI runs
.PHONY: check
docker: ## Build the container image, as CI builds and publishes it
	docker build --build-arg VERSION=$(VERSION) -t skywalking-ai-sessionizer:dev .

## binaries: cross-compile every platform in PLATFORMS and package each with LICENSE and NOTICE into dist/
.PHONY: binaries
binaries:
	@mkdir -p $(DIST)/build
	@for t in $(PLATFORMS); do \
	  os=$${t%/*}; arch=$${t#*/}; out=$(DIST)/build/$$os-$$arch; ext=""; \
	  if [ "$$os" = windows ]; then ext=.exe; fi; \
	  mkdir -p $$out && cp LICENSE NOTICE $$out/ && \
	  echo "building $$os/$$arch" && \
	  CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $$out/$(BINARY)$$ext ./cmd/$(BINARY) || exit 1; \
	  if [ "$$os" = windows ]; then \
	    rm -f $(DIST)/$(PKG_BASE)-$$os-$$arch.zip && (cd $$out && zip -q ../../$(PKG_BASE)-$$os-$$arch.zip $(BINARY)$$ext LICENSE NOTICE); \
	  else \
	    tar -C $$out -czf $(DIST)/$(PKG_BASE)-$$os-$$arch.tgz $(BINARY) LICENSE NOTICE; \
	  fi; \
	done
	@ls -la $(DIST)/$(PKG_BASE)-*

## checksums: write a sha512 file beside every package in dist/
.PHONY: checksums
checksums:
	@cd $(DIST) && for f in *.tgz *.zip; do [ -f "$$f" ] && shasum -a 512 "$$f" > "$$f.sha512"; done; ls *.sha512

## release-notes: print the text for the GitHub release page of VERSION, from its changelog page
.PHONY: release-notes
release-notes:
	@f=docs/en/changes/changes-$(VERSION).md; [ -f "$$f" ] || f=docs/en/changes/changes.md; \
	tail -n +2 "$$f" | sed '/^> In development/,/^$$/d'; \
	printf '%s\n' '' '#### Downloads and documentation' \
	  '- Downloads, with a checksum and a signature beside each package: https://skywalking.apache.org/downloads/' \
	  '- Documentation: https://skywalking.apache.org/docs/skywalking-ai-sessionizer/v$(VERSION)/readme/' \
	  '- Container image: `ghcr.io/apache/skywalking-ai-sessionizer:$(VERSION)`' \
	  '- Full changelog: https://github.com/apache/skywalking-ai-sessionizer/blob/v$(VERSION)/docs/en/changes/changes-$(VERSION).md'

## release: build everything a vote needs into dist/: the source package, every binary package, sha512 files and GPG signatures. Needs VERSION=x.y.z with the tag vx.y.z checked out
.PHONY: release
release:
	@case "$(VERSION)" in [0-9]*.[0-9]*.[0-9]*) ;; *) echo "set the version, for example: make release VERSION=0.1.0"; exit 2 ;; esac
	@git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null || { echo "tag v$(VERSION) does not exist"; exit 2; }
	@[ "$$(git rev-parse HEAD)" = "$$(git rev-parse 'v$(VERSION)^{commit}')" ] || { echo "check out v$(VERSION) first: the binaries are built from the working tree"; exit 2; }
	@git diff --quiet HEAD || { echo "the working tree has changes; a release is built from the tag alone"; exit 2; }
	@$(MAKE) binaries VERSION=$(VERSION)
	git archive --format=tar --prefix=$(RELEASE_NAME)/ v$(VERSION) | gzip -n > $(DIST)/$(RELEASE_NAME).tgz
	@$(MAKE) checksums VERSION=$(VERSION)
	@cd $(DIST) && for f in *.tgz *.zip; do gpg --armor --detach-sign --yes "$$f"; done
	@ls -la $(DIST)/*.tgz* $(DIST)/*.zip*

check: vet lint license-check dep-check test

## clean: remove build output
.PHONY: clean
clean:
	@rm -rf $(BIN_DIR) coverage.txt

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
