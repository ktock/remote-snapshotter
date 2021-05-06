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

ARG CONTAINERD_VERSION=v1.5.0
ARG RUNC_VERSION=v1.0.0-rc93
ARG CNI_PLUGINS_VERSION=v0.9.1
ARG NERDCTL_VERSION=0.8.1

# Legacy builder that doesn't support TARGETARCH should set this explicitly using --build-arg.
# If TARGETARCH isn't supported by the builder, the default value is "amd64".

FROM golang:1.15-buster AS golang-base

# Build containerd
FROM golang-base AS containerd-dev
ARG CONTAINERD_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    apt-get update -y && apt-get install -y libbtrfs-dev libseccomp-dev && \
    git clone -b ${CONTAINERD_VERSION} --depth 1 \
              https://github.com/containerd/containerd $GOPATH/src/github.com/containerd/containerd && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    GO111MODULE=off make && DESTDIR=/out/ make install

# Build containerd with builtin stargz snapshotter
FROM golang-base AS containerd-snapshotter-dev
ARG CONTAINERD_VERSION
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    apt-get update -y && apt-get install -y libbtrfs-dev libseccomp-dev && \
    git clone -b ${CONTAINERD_VERSION} --depth 1 \
              https://github.com/containerd/containerd $GOPATH/src/github.com/containerd/containerd && \
    echo 'require github.com/containerd/stargz-snapshotter v0.0.0\nreplace github.com/containerd/stargz-snapshotter => '$GOPATH'/src/github.com/containerd/stargz-snapshotter\nreplace github.com/containerd/stargz-snapshotter/estargz => '$GOPATH'/src/github.com/containerd/stargz-snapshotter/estargz' \
      >> $GOPATH/src/github.com/containerd/containerd/go.mod && \
    echo 'package main \nimport _ "github.com/containerd/stargz-snapshotter/service/plugin"' \
      > $GOPATH/src/github.com/containerd/containerd/cmd/containerd/builtins_stargz_snapshotter.go && \
    cd $GOPATH/src/github.com/containerd/containerd && \
    make vendor && make && DESTDIR=/out/ make install

# Build runc
FROM golang-base AS runc-dev
ARG RUNC_VERSION
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    apt-get update -y && apt-get install -y libseccomp-dev && \
    git clone -b ${RUNC_VERSION} --depth 1 \
              https://github.com/opencontainers/runc $GOPATH/src/github.com/opencontainers/runc && \
    cd $GOPATH/src/github.com/opencontainers/runc && \
    GO111MODULE=off make && make install PREFIX=/out/

# Build stargz snapshotter
FROM golang-base AS snapshotter-dev
ARG TARGETARCH
ARG GOARM
ARG SNAPSHOTTER_BUILD_FLAGS
ARG CTR_REMOTE_BUILD_FLAGS
COPY . $GOPATH/src/github.com/containerd/stargz-snapshotter
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/go/pkg/mod \
    cd $GOPATH/src/github.com/containerd/stargz-snapshotter && \
    PREFIX=/out/ GOARCH=${TARGETARCH:-amd64} GO_BUILD_FLAGS=${SNAPSHOTTER_BUILD_FLAGS} make containerd-stargz-grpc && \
    PREFIX=/out/ GOARCH=${TARGETARCH:-amd64} GO_BUILD_FLAGS=${CTR_REMOTE_BUILD_FLAGS} make ctr-remote

# Binaries for release
FROM scratch AS release-binaries
COPY --from=snapshotter-dev /out/* /

# Base image which contains containerd with default snapshotter
FROM golang-base AS containerd-base
ARG TARGETARCH
ARG NERDCTL_VERSION
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse && \
    curl -sSL --output /tmp/nerdctl.tgz https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH:-amd64}.tar.gz && \
    tar zxvf /tmp/nerdctl.tgz -C /usr/local/bin && \
    rm -f /tmp/nerdctl.tgz
COPY --from=containerd-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/

# Base image which contains containerd with stargz snapshotter
FROM containerd-base AS snapshotter-base
COPY --from=snapshotter-dev /out/* /usr/local/bin/
RUN ln -s /usr/local/bin/ctr-remote /usr/local/bin/ctr

# Base image which contains containerd with builtin stargz snapshotter
FROM golang-base AS containerd-snapshotter-base
ARG TARGETARCH
ARG NERDCTL_VERSION
RUN apt-get update -y && apt-get --no-install-recommends install -y fuse && \
    curl -sSL --output /tmp/nerdctl.tgz https://github.com/containerd/nerdctl/releases/download/v${NERDCTL_VERSION}/nerdctl-${NERDCTL_VERSION}-linux-${TARGETARCH:-amd64}.tar.gz && \
    tar zxvf /tmp/nerdctl.tgz -C /usr/local/bin && \
    rm -f /tmp/nerdctl.tgz
COPY --from=containerd-snapshotter-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=runc-dev /out/sbin/* /usr/local/sbin/
COPY --from=snapshotter-dev /out/ctr-remote /usr/local/bin/
RUN ln -s /usr/local/bin/ctr-remote /usr/local/bin/ctr

# Image which can be used for interactive demo environment
FROM containerd-base AS demo
ARG CNI_PLUGINS_VERSION
ARG TARGETARCH
RUN apt-get update && apt-get install -y iptables && \
    # Make CNI plugins manipulate iptables instead of nftables
    # as this test runs in a Docker container that network is configured with iptables.
    # c.f. https://github.com/moby/moby/issues/26824
    update-alternatives --set iptables /usr/sbin/iptables-legacy && \
    mkdir -p /opt/cni/bin && \
    curl -Ls https://github.com/containernetworking/plugins/releases/download/${CNI_PLUGINS_VERSION}/cni-plugins-linux-${TARGETARCH:-amd64}-${CNI_PLUGINS_VERSION}.tgz | tar xzv -C /opt/cni/bin

# Image which can be used as a node image for KinD (containerd with builtin snapshotter)
FROM kindest/node:v1.20.0 AS kind-builtin-snapshotter
COPY --from=containerd-snapshotter-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=snapshotter-dev /out/ctr-remote /usr/local/bin/
COPY ./script/config-builtin/ /
RUN apt-get update -y && apt-get install --no-install-recommends -y fuse
ENTRYPOINT [ "/usr/local/bin/entrypoint", "/sbin/init" ]

# Image which can be used as a node image for KinD
FROM kindest/node:v1.20.0
COPY --from=containerd-dev /out/bin/containerd /out/bin/containerd-shim-runc-v2 /usr/local/bin/
COPY --from=snapshotter-dev /out/* /usr/local/bin/
COPY ./script/config/ /
RUN apt-get update -y && apt-get install --no-install-recommends -y fuse && \
    systemctl enable stargz-snapshotter
ENTRYPOINT [ "/usr/local/bin/entrypoint", "/sbin/init" ]
