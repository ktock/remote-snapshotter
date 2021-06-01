/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package main

import (
	"context"
	"flag"
	"fmt"
	golog "log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"time"

	snapshotsapi "github.com/containerd/containerd/api/services/snapshots/v1"
	"github.com/containerd/containerd/contrib/snapshotservice"
	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/dialer"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/stargz-snapshotter/fusemanager"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/cri"
	"github.com/containerd/stargz-snapshotter/service/keychain/dockerconfig"
	"github.com/containerd/stargz-snapshotter/service/keychain/kubeconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	snbase "github.com/containerd/stargz-snapshotter/snapshot"
	"github.com/containerd/stargz-snapshotter/version"
	sddaemon "github.com/coreos/go-systemd/v22/daemon"
	metrics "github.com/docker/go-metrics"
	"github.com/pelletier/go-toml"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"
)

const (
	defaultAddress             = "/run/containerd-stargz-grpc/containerd-stargz-grpc.sock"
	defaultConfigPath          = "/etc/containerd-stargz-grpc/config.toml"
	defaultLogLevel            = logrus.InfoLevel
	defaultRootDir             = "/var/lib/containerd-stargz-grpc"
	defaultImageServiceAddress = "/run/containerd/containerd.sock"
	defaultFuseManagerAddress  = "/run/containerd-stargz-grpc/fuse-namanger.sock"

	fuseManagerBin     = "stargz-fuse-manager"
	fuseManagerAddress = "fuse-mananger.sock"
)

var (
	address           = flag.String("address", defaultAddress, "address for the snapshotter's GRPC server")
	configPath        = flag.String("config", defaultConfigPath, "path to the configuration file")
	logLevel          = flag.String("log-level", defaultLogLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	rootDir           = flag.String("root", defaultRootDir, "path to the root directory for this snapshotter")
	detachFuseManager = flag.Bool("detach-fuse-manager", false, "whether detach fusemanager or not")
	printVersion      = flag.Bool("version", false, "print the version")
)

type snapshotterConfig struct {
	service.Config

	// MetricsAddress is address for the metrics API
	MetricsAddress string `toml:"metrics_address"`

	// FuseManagerAddress is address for the fusemanager's GRPC server
	FuseManagerAddress string `toml:"fusemanager_address"`

	// FuseManagerPath is path to the fusemanager's executable
	FuseManagerPath string `toml:"fusemanager_path"`
}

func main() {
	flag.Parse()
	lvl, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}
	if *printVersion {
		fmt.Println("containerd-stargz-grpc", version.Version, version.Revision)
		return
	}
	logrus.SetLevel(lvl)
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
	})

	var (
		ctx    = log.WithLogger(context.Background(), log.L)
		config snapshotterConfig
	)
	// Streams log of standard lib (go-fuse uses this) into debug log
	// Snapshotter should use "github.com/containerd/containerd/log" otherwize
	// logs are always printed as "debug" mode.
	golog.SetOutput(log.G(ctx).WriterLevel(logrus.DebugLevel))

	// Get configuration from specified file
	tree, err := toml.LoadFile(*configPath)
	if err != nil && !(os.IsNotExist(err) && *configPath == defaultConfigPath) {
		log.G(ctx).WithError(err).Fatalf("failed to load config file %q", *configPath)
	}
	if err := tree.Unmarshal(&config); err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to unmarshal config file %q", *configPath)
	}

	if err := service.Supported(*rootDir); err != nil {
		log.G(ctx).WithError(err).Fatalf("snapshotter is not supported")
	}

	// Create a gRPC server
	rpc := grpc.NewServer()

	// Configure keychain
	credsFuncs := []resolver.Credential{dockerconfig.NewDockerconfigKeychain(ctx)}
	if config.Config.KubeconfigKeychainConfig.EnableKeychain {
		var opts []kubeconfig.Option
		if kcp := config.Config.KubeconfigKeychainConfig.KubeconfigPath; kcp != "" {
			opts = append(opts, kubeconfig.WithKubeconfigPath(kcp))
		}
		credsFuncs = append(credsFuncs, kubeconfig.NewKubeconfigKeychain(ctx, opts...))
	}
	if config.Config.CRIKeychainConfig.EnableKeychain {
		// connects to the backend CRI service (defaults to containerd socket)
		criAddr := defaultImageServiceAddress
		if cp := config.CRIKeychainConfig.ImageServicePath; cp != "" {
			criAddr = cp
		}
		connectCRI := func() (runtime.ImageServiceClient, error) {
			// TODO: make gRPC options configurable from config.toml
			backoffConfig := backoff.DefaultConfig
			backoffConfig.MaxDelay = 3 * time.Second
			connParams := grpc.ConnectParams{
				Backoff: backoffConfig,
			}
			gopts := []grpc.DialOption{
				grpc.WithInsecure(),
				grpc.WithConnectParams(connParams),
				grpc.WithContextDialer(dialer.ContextDialer),
				grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(defaults.DefaultMaxRecvMsgSize)),
				grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(defaults.DefaultMaxSendMsgSize)),
			}
			conn, err := grpc.Dial(dialer.DialAddress(criAddr), gopts...)
			if err != nil {
				return nil, err
			}
			return runtime.NewImageServiceClient(conn), nil
		}
		f, criServer := cri.NewCRIKeychain(ctx, connectCRI)
		runtime.RegisterImageServiceServer(rpc, criServer)
		credsFuncs = append(credsFuncs, f)
	}

	var rs snapshots.Snapshotter
	if *detachFuseManager {
		fmPath := config.FuseManagerPath
		if fmPath == "" {
			var err error
			fmPath, err = exec.LookPath(fuseManagerBin)
			if err != nil {
				log.G(ctx).WithError(err).Fatalf("failed to find fusemanager bin")
			}
		}
		fmAddr := config.FuseManagerAddress
		if fmAddr == "" {
			var err error
			fmAddr, err = exec.LookPath(fuseManagerAddress)
			if err != nil {
				fmAddr = defaultFuseManagerAddress
			}
		}
		err := service.StartFuseManager(ctx, fmPath, fmAddr, filepath.Join(*rootDir, "fusestore.db"), *logLevel, filepath.Join(*rootDir, "stargz-fuse-manager.log"))
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to start fusemanager")
		}
		fs, err := fusemanager.NewManagerClient(ctx, *rootDir, fmAddr, &config.Config)
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure fusemanager")
		}
		rs, err = snbase.NewSnapshotter(ctx, filepath.Join(*rootDir, "snapshotter"), fs, snbase.AsynchronousRemove)
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure snapshotter")
		}
		log.G(ctx).Infof("Start snapshotter with fusemanager mode")
	} else {
		rs, err = service.NewStargzSnapshotterService(ctx, *rootDir, &config.Config, service.WithCredsFuncs(credsFuncs...))
		if err != nil {
			log.G(ctx).WithError(err).Fatalf("failed to configure snapshotter")
		}
		log.G(ctx).Infof("Start snapshotter with builtin mode")
	}

	cleanup, err := serve(ctx, rpc, *address, rs, config)
	if err != nil {
		log.G(ctx).WithError(err).Fatalf("failed to serve snapshotter")
	}

	if cleanup {
		log.G(ctx).Debug("Closing the snapshotter")
		rs.Close()
	}
	log.G(ctx).Info("Exiting")
}

func serve(ctx context.Context, rpc *grpc.Server, addr string, rs snapshots.Snapshotter, config snapshotterConfig) (bool, error) {
	// Convert the snapshotter to a gRPC service,
	snsvc := snapshotservice.FromSnapshotter(rs)

	// Register the service with the gRPC server
	snapshotsapi.RegisterSnapshotsServer(rpc, snsvc)

	// Prepare the directory for the socket
	if err := os.MkdirAll(filepath.Dir(addr), 0700); err != nil {
		return false, errors.Wrapf(err, "failed to create directory %q", filepath.Dir(addr))
	}

	// Try to remove the socket file to avoid EADDRINUSE
	if err := os.RemoveAll(addr); err != nil {
		return false, errors.Wrapf(err, "failed to remove %q", addr)
	}

	errCh := make(chan error, 1)

	if config.MetricsAddress != "" {
		l, err := net.Listen("tcp", config.MetricsAddress)
		if err != nil {
			return false, errors.Wrapf(err, "failed to get listener for metrics endpoint")
		}
		m := http.NewServeMux()
		m.Handle("/metrics", metrics.Handler())
		go func() {
			if err := http.Serve(l, m); err != nil {
				errCh <- errors.Wrapf(err, "error on serving metrics via socket %q", addr)
			}
		}()
	}

	// Listen and serve
	l, err := net.Listen("unix", addr)
	if err != nil {
		return false, errors.Wrapf(err, "error on listen socket %q", addr)
	}
	go func() {
		if err := rpc.Serve(l); err != nil {
			errCh <- errors.Wrapf(err, "error on serving via socket %q", addr)
		}
	}()

	if os.Getenv("NOTIFY_SOCKET") != "" {
		notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyReady)
		log.G(ctx).Debugf("SdNotifyReady notified=%v, err=%v", notified, notifyErr)
	}
	defer func() {
		if os.Getenv("NOTIFY_SOCKET") != "" {
			notified, notifyErr := sddaemon.SdNotify(false, sddaemon.SdNotifyStopping)
			log.G(ctx).Debugf("SdNotifyStopping notified=%v, err=%v", notified, notifyErr)
		}
	}()

	var s os.Signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	select {
	case s = <-sigCh:
		log.G(ctx).Infof("Got %v", s)
	case err := <-errCh:
		return false, err
	}
	if s == unix.SIGINT {
		return true, nil // do cleanup on SIGINT
	}
	return false, nil
}
