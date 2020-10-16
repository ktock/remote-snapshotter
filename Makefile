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

CMD=containerd-stargz-grpc ctr-remote

CMD_BINARIES=$(addprefix $(PREFIX),$(CMD))

.PHONY: all build check install-check-tools install uninstall clean test test-root test-all integration test-optimize benchmark test-pullsecrets test-cri

all: build

build: $(CMD)

FORCE:

containerd-stargz-grpc: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) -v ./cmd/containerd-stargz-grpc

ctr-remote: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) -v ./cmd/ctr-remote

docker-optimize: FORCE
	GO111MODULE=$(GO111MODULE_VALUE) go build -o $(PREFIX)$@ $(GO_BUILD_FLAGS) -v ./cmd/docker-optimize

check:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) golangci-lint run

install-check-tools:
	@curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s -- -b $(go env GOPATH)/bin v1.19.1

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
	@GO111MODULE=$(GO111MODULE_VALUE) go test -race ./...

test-root:
	@echo "$@"
	@GO111MODULE=$(GO111MODULE_VALUE) go test -race ./snapshot -test.root

test-all: test-root test

integration:
	@./script/integration/test.sh

test-optimize:
	@./script/optimize/test.sh

benchmark:
	@./script/benchmark/test.sh

test-pullsecrets:
	@./script/pullsecrets/test.sh

test-cri:
	@./script/cri/test.sh
