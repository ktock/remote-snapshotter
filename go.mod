module github.com/containerd/stargz-snapshotter

go 1.16

require (
	github.com/containerd/console v1.0.2
	github.com/containerd/containerd v1.5.2
	github.com/containerd/continuity v0.1.0
	github.com/containerd/go-cni v1.0.2
	github.com/containerd/stargz-snapshotter/estargz v0.6.4
	github.com/coreos/go-systemd/v22 v22.3.2
	github.com/docker/cli v20.10.7+incompatible
	github.com/docker/docker v20.10.7+incompatible // indirect
	github.com/docker/docker-credential-helpers v0.6.4 // indirect
	github.com/docker/go-metrics v0.0.1
	github.com/gogo/protobuf v1.3.2
	github.com/golang/groupcache v0.0.0-20200121045136-8c9f03a8e57e
	github.com/hanwen/go-fuse/v2 v2.1.0
	github.com/hashicorp/go-multierror v1.1.1
	github.com/hashicorp/golang-lru v0.5.3 // indirect
	github.com/moby/sys/mountinfo v0.4.1
	github.com/opencontainers/go-digest v1.0.0
	github.com/opencontainers/image-spec v1.0.1
	github.com/opencontainers/runtime-spec v1.0.3-0.20200929063507-e6143ca7d51d
	github.com/pelletier/go-toml v1.9.1
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.7.1
	github.com/rs/xid v1.2.1
	github.com/sirupsen/logrus v1.8.1
	github.com/stretchr/testify v1.7.0 // indirect
	github.com/urfave/cli v1.22.2
	go.etcd.io/bbolt v1.3.5
	golang.org/x/sync v0.0.0-20201207232520-09787c993a3a
	golang.org/x/sys v0.0.0-20210324051608-47abb6519492
	google.golang.org/grpc v1.35.0
	k8s.io/api v0.20.6
	k8s.io/apimachinery v0.20.6
	k8s.io/client-go v0.20.6
	k8s.io/cri-api v0.20.6
)

replace (
	// Import local package for estargz.
	github.com/containerd/stargz-snapshotter/estargz => ./estargz

	// NOTE: github.com/containerd/containerd v1.4.0 depends on github.com/urfave/cli v1.22.1
	//       because of https://github.com/urfave/cli/issues/1092
	github.com/urfave/cli => github.com/urfave/cli v1.22.1
)
