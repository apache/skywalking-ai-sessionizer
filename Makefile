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

# The version the binary reports. A tagged checkout says v0.1.0, anything
# else says the nearest tag and commit, and -dirty when the tree has changes.
# Pass VERSION=0.1.0 to override, which is also what a release does.
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -X main.version=$(VERSION)

RELEASE_NAME := apache-skywalking-ai-sessionizer-$(VERSION)-src

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

## release: build the source release for a vote into dist/: tarball, sha512 and GPG signature. Needs VERSION=x.y.z and the tag vx.y.z
.PHONY: release
release:
	@case "$(VERSION)" in [0-9]*.[0-9]*.[0-9]*) ;; *) echo "set the version, for example: make release VERSION=0.1.0"; exit 2 ;; esac
	@git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null || { echo "tag v$(VERSION) does not exist"; exit 2; }
	@mkdir -p dist
	git archive --format=tar --prefix=$(RELEASE_NAME)/ v$(VERSION) | gzip -n > dist/$(RELEASE_NAME).tgz
	cd dist && shasum -a 512 $(RELEASE_NAME).tgz > $(RELEASE_NAME).tgz.sha512
	cd dist && gpg --armor --detach-sign $(RELEASE_NAME).tgz
	@ls -la dist/$(RELEASE_NAME).tgz*

check: vet lint license-check dep-check test

## clean: remove build output
.PHONY: clean
clean:
	@rm -rf $(BIN_DIR) coverage.txt

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
