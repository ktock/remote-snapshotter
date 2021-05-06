#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.


# Base path used to install.
CMD_DESTDIR ?= /usr/local
GO111MODULE_VALUE=auto
PREFIX ?= out/

PKG=github.com/containerd/stargz-snapshotter
VERSION=$(shell git describe --match 'v[0-9]*' --dirty='.m' --always --tags)
REVISION=$(shell git rev-parse HEAD)$(shell if ! git diff --no-ext-diff --quiet --exit-code; then echo .m; fi)
GO_LD_FLAGS=-ldflags '-s -w -X $(PKG)/version.Version=$(VERSION) -X $(PKG)/version.Revision=$(REVISION) $(GO_EXTRA_LDFLAGS)'

CMD=containerd-stargz-grpc ctr-remote

CMD_BINARIES=$(addprefix $(PREFIX),$(CMD))

.PHONY: all build check install-check-tools install uninstall clean test test-root test-all integration test-optimize benchmark test-pullsecrets test-cri

all: build

build: $(CMD)

FORCE:

containerd-stargz-grpc: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./cmd/containerd-stargz-grpc

ctr-remote: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) $(GO_LD_FLAGS) -v ./cmd/ctr-remote

check:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) golangci-lint run
	@cd ./estargz ; GO111MODULE=$(GO111MODULE_VALUE) golangci-lint run

install-check-tools:
	@curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s -- -b $(go env GOPATH)/bin v1.39.0

install:
	@echo "$@"
	@mkdir -p $(CMD_DESTDIR)/bin
	@install $(CMD_BINARIES) $(CMD_DESTDIR)/bin

uninstall:
	@echo "$@"
	@rm -f $(addprefix $(CMD_DESTDIR)/bin/,$(notdir $(CMD_BINARIES)))

clean:
	@echo "$@"
	@rm -f $(CMD_BINARIES)

test:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) ENABLE_INTEGRATION_TEST=false go test $(GO_TEST_FLAGS) -race ./...
	@cd ./estargz ; GO111MODULE=$(GO111MODULE_VALUE) go test $(GO_TEST_FLAGS) -race ./...

test-root:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test $(GO_TEST_FLAGS) -race ./snapshot -test.root

test-all: test-root test

integration:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) ENABLE_INTEGRATION_TEST=true go test $(GO_TEST_FLAGS) -v -timeout=0 ./integration

benchmark:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test -v -timeout=0 ./benchmark
