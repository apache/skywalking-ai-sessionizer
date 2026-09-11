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

# The Claude Code plugin: its manifest and hooks live in the tree, and its
# binary is built beside them, where the hooks find it.
PLUGIN_DIR    := plugins/claude-code
PLUGIN_BINARY := asz-claude-plugin

# The platforms a release ships binaries for. Every one is cross-compiled
# from any host: the binary is pure Go and needs no C toolchain.
PLATFORMS := darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64
DIST      := dist
PKG_BASE  := apache-skywalking-ai-sessionizer-$(VERSION)-bin

# The GPG key that signs a release, by id, fingerprint or email. Empty means
# gpg's default key. The key must be in the SkyWalking KEYS file.
GPG_USER  ?=

GOLANGCI_LINT_VERSION := v2.13.2
LICENSE_EYE_VERSION   := v0.9.0

.DEFAULT_GOAL := check

$(BIN_DIR):
	@mkdir -p $(BIN_DIR)

## build: compile the binary into ./bin
.PHONY: build
build: $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN_DIR)/$(BINARY) ./cmd/$(BINARY)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(PLUGIN_DIR)/bin/$(PLUGIN_BINARY) ./$(PLUGIN_DIR)

## test: run the whole suite, unit and end-to-end
.PHONY: test
test:
	$(GO) test -race -count=1 ./...

## test-e2e: the scenarios, the chain tests and the boundary rules, verbose
.PHONY: test-e2e
test-e2e:
	go test -race -count=1 -v ./tests/...

## scenarios: run every scenario through the built command, as CI does
.PHONY: scenarios
scenarios: build
	@for f in tests/scenarios/*.yaml; do \
		case $$f in *.expect.yaml) continue;; esac; \
		echo "== $$f"; $(BIN_DIR)/asz scenario check $$f || exit 1; \
	done

## e2e-collector: push a generated session into a real OpenTelemetry Collector over both transports and verify what it wrote (needs docker)
.PHONY: e2e-collector
e2e-collector: build
	tools/e2e-collector.sh

## conversation-view: rebuild the conversation renderer asz view embeds from the pinned Horizon commit (needs node 24 and pnpm)
.PHONY: conversation-view
conversation-view:
	tools/conversation-view.sh update

## conversation-view-check: fail when the embedded renderer is not what the pinned Horizon commit builds
.PHONY: conversation-view-check
conversation-view-check:
	tools/conversation-view.sh check

## asz-view-example: regenerate the complete example document on the asz.view format page from the fixture scenario
.PHONY: asz-view-example
asz-view-example: build
	@dir=$$(mktemp -d); \
	$(BIN_DIR)/asz scenario build tests/scenarios/fixture.yaml --format sd --out $$dir --at 2026-01-01T00:00:00Z >/dev/null && \
	$(BIN_DIR)/asz parse -config $$dir/asz.yaml >/dev/null && \
	$(BIN_DIR)/asz conversation -config $$dir/asz.yaml -yaml 00000001-0000-4000-8000-000000000001 > docs/en/formats/asz-view-example.yaml && \
	rm -rf $$dir && echo "docs/en/formats/asz-view-example.yaml"

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
	@GOBIN=$(CURDIR)/$(BIN_DIR) $(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

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

## dep-licenses: regenerate dist-material/LICENSE, dist-material/NOTICE and dist-material/licenses from the modules built into the binaries; every binary package carries them
.PHONY: dep-licenses
# license-eye lists every module go.mod requires, and some of them only the
# tests of a dependency need, such as github.com/kr/text through the tests of
# gopkg.in/yaml.v3. A binary package must account for exactly what it
# holds. So tools/dep-notices.sh also writes a license-eye configuration
# that excludes every module the two binaries do not use on any platform.
dep-licenses: $(BIN_DIR)/license-eye
	@rm -rf dist-material/licenses
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && \
	  tools/dep-notices.sh dist-material/NOTICE $$tmp/licenserc.yaml && \
	  $(BIN_DIR)/license-eye -c $$tmp/licenserc.yaml dependency resolve --summary dist-material/LICENSE.tpl --output dist-material/licenses
	@$(MAKE) --no-print-directory font-licenses OUT=dist-material/licenses

# The two fonts the embedded conversation renderer carries are bundled into
# the binary too, under the SIL Open Font License; license-eye resolves Go
# modules only, so their texts are copied beside the modules' by hand.
.PHONY: font-licenses
font-licenses:
	@cp internal/view/conversation-view/host-shell/fonts/LICENSE-inter.txt $(OUT)/license-inter-font.txt
	@cp internal/view/conversation-view/host-shell/fonts/LICENSE-jetbrains-mono.txt $(OUT)/license-jetbrains-mono-font.txt

## dep-licenses-check: fail when dist-material is not what the modules built into the binaries resolve to, so a changed dependency cannot ship without its license
.PHONY: dep-licenses-check
dep-licenses-check: $(BIN_DIR)/license-eye
	@tmp=$$(mktemp -d) && trap 'rm -rf "$$tmp"' EXIT && mkdir $$tmp/dist-material && \
	  cp dist-material/LICENSE.tpl $$tmp/dist-material/ && \
	  tools/dep-notices.sh $$tmp/dist-material/NOTICE $$tmp/licenserc.yaml && \
	  $(BIN_DIR)/license-eye -v warn -c $$tmp/licenserc.yaml dependency resolve --summary $$tmp/dist-material/LICENSE.tpl --output $$tmp/dist-material/licenses >/dev/null && \
	  $(MAKE) --no-print-directory font-licenses OUT=$$tmp/dist-material/licenses && \
	  if diff -r $$tmp/dist-material dist-material; then echo "dist-material matches the dependencies"; \
	  else echo "dist-material is out of date: run 'make dep-licenses' and commit the result"; exit 1; fi

## tidy: verify go.mod and go.sum are current
.PHONY: tidy
tidy:
	$(GO) mod tidy
	@git diff --exit-code go.mod go.sum || (echo "go.mod/go.sum are not tidy; run 'make tidy' and commit"; exit 1)

## check: everything CI runs
.PHONY: check
docker: ## Build the container image, as CI builds and publishes it
	docker build --build-arg VERSION=$(VERSION) -t skywalking-ai-sessionizer:dev .

## binaries: cross-compile every platform in PLATFORMS and package each, with the Claude Code plugin, the LICENSE, the NOTICE and the dependency licenses, into dist/
.PHONY: binaries
# Two builds of one tag give the same bytes when they use the same Go, the
# same tar and the same gzip. Go records the tag in each binary as the
# module's version, so a build of the commit before it was tagged differs.
# A rebuild can then be held against a
# package, and no package records who built it or when. So every staged
# file gets the commit's time and the same modes, entries are stored in one
# sorted order with owner and group 0, and neither gzip nor zip adds a time
# or a local user of its own. The time is SOURCE_DATE_EPOCH when set, else
# the commit's. An unpacked source package has neither, and its packages
# keep the build time. GNU tar and bsdtar name the same settings
# differently, and they write their headers differently. From the same
# staged files, GNU tar 1.35 and bsdtar 3.5.3 wrote different archives, and
# GNU gzip 1.14 and Apple gzip 479 compressed one archive differently. So a
# CI build, with GNU tar and GNU gzip, and a macOS build, with bsdtar and
# Apple gzip, differ.
# The build ignores a go.work and the GOFLAGS of the environment. A
# workspace changes the code a dependency is compiled from, and git does not
# show a go.work, because it is ignored. tools/release.sh builds in a clone
# inside the checkout, where a go.work at the checkout's root applies too.
binaries:
	@mkdir -p $(DIST)/build
	@epoch=$${SOURCE_DATE_EPOCH:-$$(git log -1 --format=%ct 2>/dev/null)}; stamp=""; \
	if [ -n "$$epoch" ]; then stamp=$$(date -u -d "@$$epoch" +%Y%m%d%H%M.%S 2>/dev/null || date -u -r "$$epoch" +%Y%m%d%H%M.%S) || exit 1; fi; \
	if tar --version 2>/dev/null | grep -q 'GNU tar'; then owner="--owner=0 --group=0 --numeric-owner"; else owner="--uid 0 --gid 0 --numeric-owner"; fi; \
	for t in $(PLATFORMS); do \
	  os=$${t%/*}; arch=$${t#*/}; out=$(DIST)/build/$$os-$$arch; ext=""; \
	  if [ "$$os" = windows ]; then ext=.exe; fi; \
	  rm -rf $$out && mkdir -p $$out/claude-code-plugin/bin && \
	  cp dist-material/LICENSE dist-material/NOTICE $$out/ && cp -R dist-material/licenses $$out/licenses && \
	  echo "building $$os/$$arch" && \
	  CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=readonly GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $$out/$(BINARY)$$ext ./cmd/$(BINARY) || exit 1; \
	  cp -R $(PLUGIN_DIR)/.claude-plugin $(PLUGIN_DIR)/hooks $$out/claude-code-plugin/ && \
	  CGO_ENABLED=0 GOWORK=off GOFLAGS=-mod=readonly GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "-s -w $(LDFLAGS)" -o $$out/claude-code-plugin/bin/$(PLUGIN_BINARY)$$ext ./$(PLUGIN_DIR) || exit 1; \
	  chmod -R u=rwX,go=rX $$out || exit 1; \
	  if [ -n "$$stamp" ]; then find $$out -exec env TZ=UTC0 touch -t $$stamp {} + || exit 1; fi; \
	  (cd $$out && find $(BINARY)$$ext claude-code-plugin LICENSE NOTICE licenses | LC_ALL=C sort > ../$$os-$$arch.list) || exit 1; \
	  if [ "$$os" = windows ]; then \
	    rm -f $(DIST)/$(PKG_BASE)-$$os-$$arch.zip && \
	    (cd $$out && TZ=UTC0 zip -X -q ../../$(PKG_BASE)-$$os-$$arch.zip -@ < ../$$os-$$arch.list) || exit 1; \
	  else \
	    (cd $$out && tar --format=ustar $$owner --no-recursion -cf ../$$os-$$arch.tar -T ../$$os-$$arch.list) && \
	    gzip -n -c $(DIST)/build/$$os-$$arch.tar > $(DIST)/$(PKG_BASE)-$$os-$$arch.tgz || exit 1; \
	  fi; \
	done
	@ls -la $(DIST)/$(PKG_BASE)-*

## checksums: write a sha512 file beside every package in dist/
.PHONY: checksums
# The first failure stops it, as the first gpg failure stops release. A
# loop ends with the status of its last command, and the shell creates the
# .sha512 file before shasum runs. So a failed checksum once left an empty
# .sha512, and make went on to sign every package. Each file is checked
# right after it is written.
checksums:
	@cd $(DIST) && for f in *.tgz *.zip; do \
	  [ -f "$$f" ] || continue; \
	  { shasum -a 512 "$$f" > "$$f.sha512" && shasum -a 512 --status -c "$$f.sha512"; } \
	    || { rm -f "$$f.sha512"; echo "cannot write the sha512 of $$f, so this stops here"; exit 1; }; \
	done; ls *.sha512

# The font files a source release must not carry. They are found by name,
# because file(1) often reports a web font only as data. Fonts come under
# licenses such as the SIL Open Font License, which the ASF puts in
# Category B, and a Category B work must not be in a source release.
# tools/release.sh candidate refuses the same list, in font_files, so
# change both together.
FONT_FILES := \.(woff2?|ttf|otf|eot)

# The file(1) types a source release must not carry, because the ASF says a
# source release should not contain compiled code. A static library, a Go
# object and a Go archive are application/x-archive, and a jar is
# application/java-archive. tools/release.sh reads this line from the tag's
# Makefile, so both refuse the same files.
COMPILED_TYPES := application/(x-(mach-binary|executable|pie-executable|sharedlib|dosexec|object|java-applet|archive|bytecode\.python)|vnd\.microsoft\.portable-executable|java-archive|wasm)

# The names of compiled files, for the ones file(1) cannot tell by type.
# The file 5.41 that ships with macOS reports a WebAssembly module and a
# Python .pyc as application/octet-stream. tools/release.sh reads this line
# from the tag's Makefile too.
COMPILED_FILES := \.(a|o|so|dylib|dll|exe|lib|obj|class|jar|war|pyc|pyo|wasm)

## release: build everything a vote needs into dist/: the source package, every binary package, sha512 files and GPG signatures. Needs VERSION=x.y.z with the tag vx.y.z checked out
.PHONY: release
release:
	@case "$(VERSION)" in [0-9]*.[0-9]*.[0-9]*) ;; *) echo "set the version, for example: make release VERSION=0.1.0"; exit 2 ;; esac
	@git rev-parse -q --verify "refs/tags/v$(VERSION)" >/dev/null || { echo "tag v$(VERSION) does not exist"; exit 2; }
	@[ "$$(git rev-parse HEAD)" = "$$(git rev-parse 'v$(VERSION)^{commit}')" ] || { echo "check out v$(VERSION) first: the binaries are built from the working tree"; exit 2; }
	@# A file git does not track counts too, an ignored one included. go
	@# build compiles an untracked .go file, internal/view embeds every file
	@# in conversation-view whose name does not start with . or _, and
	@# binaries copies whole directories, so an ignored .DS_Store in
	@# plugins/claude-code/hooks goes into a binary package. The source
	@# package, made from the tag, holds none of them. Only the build output
	@# may be there: dist/, bin/ and plugins/claude-code/bin/.
	@left=$$(git status --porcelain --ignored | grep -v -x -e '!! $(DIST)/' -e '!! $(BIN_DIR)/' -e '!! $(PLUGIN_DIR)/bin/' || true); \
	  [ -z "$$left" ] || { echo "the working tree has changes, or files git does not track, ignored ones included. A release is built from the tag alone, so build it in a fresh clone of the tag, as tools/release.sh candidate does:"; echo "$$left"; exit 2; }
	@# Two build outputs were once committed by accident, so refuse any
	@# tracked compiled file rather than ship it in the voted source package.
	@# file(1) tells most of them by type, and COMPILED_FILES names the ones
	@# it cannot tell. A file both of them find is named once, with its type.
	@bad=$$( { git ls-files -z | xargs -0 file -N --mime-type | grep -E ': *$(COMPILED_TYPES)$$'; git ls-files | grep -E '$(COMPILED_FILES)$$'; } | awk -F: '!seen[$$1]++' || true); \
	  [ -z "$$bad" ] || { echo "the source package would carry compiled files; remove them from git first:"; echo "$$bad"; exit 2; }
	@# Packages left from an earlier build would be signed and checksummed
	@# with these, and moved beside them.
	@rm -f $(DIST)/*.tgz $(DIST)/*.zip $(DIST)/*.tgz.* $(DIST)/*.zip.*
	@$(MAKE) binaries VERSION=$(VERSION)
	git archive --format=tar --prefix=$(RELEASE_NAME)/ v$(VERSION) | gzip -n > $(DIST)/$(RELEASE_NAME).tgz
	@# .gitattributes leaves the renderer's fonts out of git archive. This is
	@# the last guard, for the day that changes. It stops the release before
	@# anything is checksummed or signed, and removes the package, so it
	@# cannot be uploaded by hand.
	@listing=$$(tar -tzf $(DIST)/$(RELEASE_NAME).tgz) || { echo "cannot list $(DIST)/$(RELEASE_NAME).tgz"; exit 2; }; \
	  fonts=$$(printf '%s\n' "$$listing" | grep -E '$(FONT_FILES)$$' || true); \
	  [ -z "$$fonts" ] || { rm -f $(DIST)/$(RELEASE_NAME).tgz; \
	    echo "the source package holds font files, so $(DIST)/$(RELEASE_NAME).tgz was removed:"; echo "$$fonts"; \
	    echo "Fonts come under licenses such as the SIL Open Font License, which the ASF puts in Category B. The ASF third-party license policy says: \"Do not include Category B licensed works in source releases.\" See https://www.apache.org/legal/resolved.html#category-b. Mark each font export-ignore in .gitattributes."; \
	    exit 2; }
	@$(MAKE) checksums VERSION=$(VERSION)
	@# A name that is not a file is skipped. With no Windows package, *.zip
	@# stays as it is, and gpg would be asked to sign a file of that name.
	@# The first gpg failure stops the release. A loop ends with the status of
	@# its last command, so a failure followed by a success would pass.
	@cd $(DIST) && for f in *.tgz *.zip; do \
	  [ -f "$$f" ] || continue; \
	  gpg --armor --detach-sign --yes $(if $(GPG_USER),--local-user "$(GPG_USER)",) "$$f" \
	    || { echo "gpg could not sign $$f, so the release stops here"; exit 1; }; \
	done
	@# The zip files are listed only when there are some, for the same reason.
	@cd $(DIST) && ls -la *.tgz* $$(ls *.zip* 2>/dev/null)

check: vet lint license-check dep-check dep-licenses-check test

## clean: remove build output
.PHONY: clean
clean:
	@rm -rf $(BIN_DIR) coverage.txt

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## //' | awk -F': ' '{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'
