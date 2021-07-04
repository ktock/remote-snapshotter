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

package fusemanager

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/containerd/containerd/defaults"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/pkg/dialer"
	"github.com/moby/sys/mountinfo"
	"github.com/pkg/errors"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	pb "github.com/containerd/stargz-snapshotter/fusemanager/api"
	"github.com/containerd/stargz-snapshotter/service"
	"github.com/containerd/stargz-snapshotter/service/keychain/cri"
	"github.com/containerd/stargz-snapshotter/service/keychain/dockerconfig"
	"github.com/containerd/stargz-snapshotter/service/keychain/kubeconfig"
	"github.com/containerd/stargz-snapshotter/service/resolver"
	"github.com/containerd/stargz-snapshotter/snapshot"
)

const (
	FuseManagerNotReady = iota
	FuseManagerWaitInit
	FuseManagerReady

	defaultImageServiceAddress = "/run/containerd/containerd.sock"
)

type Server struct {
	pb.UnimplementedStargzFuseManagerServiceServer

	lock   sync.RWMutex
	status int32

	listener net.Listener
	server   *grpc.Server

	// root is the latest root passed from containerd-stargz-grpc
	root string
	// config is the latest config passed from containerd-stargz-grpc
	config *service.Config
	// fsMap maps mountpoint to its filesystem instance to ensure Mount/Check/Unmount
	// call the proper filesystem
	fsMap sync.Map
	// curFs is filesystem created by latest config
	curFs snapshot.FileSystem
	ms    *bolt.DB
}

func NewFuseManager(ctx context.Context, listener net.Listener, server *grpc.Server, fuseStoreAddr string) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(fuseStoreAddr), 0700); err != nil {
		return nil, errors.Wrapf(err, "failed to create directory %q", filepath.Dir(fuseStoreAddr))
	}

	db, err := bolt.Open(fuseStoreAddr, 0666, &bolt.Options{Timeout: 10 * time.Second, ReadOnly: false})
	if err != nil {
		return nil, errors.Wrap(err, "failed to configure fusestore")
	}

	fm := &Server{
		status:   FuseManagerWaitInit,
		lock:     sync.RWMutex{},
		fsMap:    sync.Map{},
		ms:       db,
		listener: listener,
		server:   server,
	}

	return fm, nil
}

func (fm *Server) Status(ctx context.Context, _ *pb.StatusRequest) (*pb.StatusResponse, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()

	return &pb.StatusResponse{
		Status: fm.status,
	}, nil
}

func (fm *Server) Init(ctx context.Context, req *pb.InitRequest) (*pb.Response, error) {
	fm.lock.Lock()
	fm.status = FuseManagerWaitInit
	defer func() {
		fm.status = FuseManagerReady
		fm.lock.Unlock()
	}()

	ctx = log.WithLogger(ctx, log.G(ctx))

	config := &service.Config{}
	err := json.Unmarshal(req.Config, config)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to get config")
		return &pb.Response{}, err
	}
	fm.root = req.Root
	fm.config = config

	// Configure keychain
	credsFuncs := []resolver.Credential{dockerconfig.NewDockerconfigKeychain(ctx)}
	if config.KubeconfigKeychainConfig.EnableKeychain {
		var opts []kubeconfig.Option
		if kcp := config.KubeconfigKeychainConfig.KubeconfigPath; kcp != "" {
			opts = append(opts, kubeconfig.WithKubeconfigPath(kcp))
		}
		credsFuncs = append(credsFuncs, kubeconfig.NewKubeconfigKeychain(ctx, opts...))
	}
	if config.CRIKeychainConfig.EnableKeychain {
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
		f, _ := cri.NewCRIKeychain(ctx, connectCRI)
		credsFuncs = append(credsFuncs, f)
	}

	opts := []service.Option{service.WithCredsFuncs(credsFuncs...)}

	fs, err := service.NewFileSystem(ctx, fm.root, fm.config, opts...)
	if err != nil {
		return &pb.Response{}, err
	}
	fm.curFs = fs

	err = fm.restoreFuseInfo(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to restore fuse info")
		return &pb.Response{}, err
	}

	return &pb.Response{}, nil
}

func (fm *Server) Mount(ctx context.Context, req *pb.MountRequest) (*pb.Response, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()
	if fm.status != FuseManagerReady {
		return &pb.Response{}, fmt.Errorf("fuse manager not ready")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("mountpoint", req.Mountpoint))

	err := fm.mount(ctx, req.Mountpoint, req.Labels)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to mount stargz")
		return &pb.Response{}, err
	}

	fm.storeFuseInfo(&fuseInfo{
		Root:       fm.root,
		Mountpoint: req.Mountpoint,
		Labels:     req.Labels,
		Config:     *fm.config,
	})

	return &pb.Response{}, nil
}

func (fm *Server) Check(ctx context.Context, req *pb.CheckRequest) (*pb.Response, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()
	if fm.status != FuseManagerReady {
		return &pb.Response{}, fmt.Errorf("fuse manager not ready")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("mountpoint", req.Mountpoint))

	obj, found := fm.fsMap.Load(req.Mountpoint)
	if !found {
		err := fmt.Errorf("failed to find filesystem of mountpoint %s", req.Mountpoint)
		log.G(ctx).WithError(err).Errorf("failed to check filesystem")
		return &pb.Response{}, err
	}

	fs := obj.(snapshot.FileSystem)
	err := fs.Check(ctx, req.Mountpoint, req.Labels)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to check filesystem")
		return &pb.Response{}, err
	}

	return &pb.Response{}, nil
}

func (fm *Server) Unmount(ctx context.Context, req *pb.UnmountRequest) (*pb.Response, error) {
	fm.lock.RLock()
	defer fm.lock.RUnlock()
	if fm.status != FuseManagerReady {
		return &pb.Response{}, fmt.Errorf("fuse manager not ready")
	}

	ctx = log.WithLogger(ctx, log.G(ctx).WithField("mountpoint", req.Mountpoint))

	obj, found := fm.fsMap.Load(req.Mountpoint)
	if !found {
		// check whether already unmounted
		mounts, err := mountinfo.GetMounts(func(info *mountinfo.Info) (skip, stop bool) {
			if info.Mountpoint == req.Mountpoint {
				return false, true
			}
			return true, false
		})
		if err != nil {
			log.G(ctx).WithError(err).Errorf("failed to get mount info")
			return &pb.Response{}, err
		}

		if len(mounts) <= 0 {
			return &pb.Response{}, nil
		}
		err = fmt.Errorf("failed to find filesystem of mountpoint %s", req.Mountpoint)
		log.G(ctx).WithError(err).Errorf("failed to unmount filesystem")
		return &pb.Response{}, err
	}

	fs := obj.(snapshot.FileSystem)
	err := fs.Unmount(ctx, req.Mountpoint)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to unmount filesystem")
		return &pb.Response{}, err
	}

	fm.fsMap.Delete(req.Mountpoint)
	fm.removeFuseInfo(&fuseInfo{
		Mountpoint: req.Mountpoint,
	})

	return &pb.Response{}, nil
}

func (fm *Server) Close(ctx context.Context) error {
	fm.lock.Lock()
	defer fm.lock.Unlock()
	fm.status = FuseManagerNotReady

	err := fm.clearMounts(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to clear mounts")
		return err
	}

	err = fm.ms.Close()
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to close fusestore")
		return err
	}

	return nil
}

func (fm *Server) mount(ctx context.Context, mountpoint string, labels map[string]string) error {
	// mountpoint in fsMap means layer is already mounted, skip it
	if _, found := fm.fsMap.Load(mountpoint); found {
		return nil
	}

	err := fm.curFs.Mount(ctx, mountpoint, labels)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to mount stargz")
		return err
	}

	fm.fsMap.Store(mountpoint, fm.curFs)
	return nil
}

func (fm *Server) clearMounts(ctx context.Context) error {
	mountpoints, err := fm.listMountpoints(ctx)
	if err != nil {
		return err
	}

	for _, mp := range mountpoints {
		if err := syscall.Unmount(mp, syscall.MNT_FORCE); err != nil {
			return err
		}
	}

	return nil
}
