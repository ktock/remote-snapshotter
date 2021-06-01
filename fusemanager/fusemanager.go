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
	"flag"
	"fmt"
	golog "log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/containerd/containerd/log"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"

	pb "github.com/containerd/stargz-snapshotter/fusemanager/api"
	"github.com/containerd/stargz-snapshotter/version"
)

var (
	debugFlag     bool
	versionFlag   bool
	fuseStoreAddr string
	address       string
	logLevel      string
	logPath       string
	action        string
)

func parseFlags() {
	flag.BoolVar(&debugFlag, "debug", false, "enable debug output in logs")
	flag.BoolVar(&versionFlag, "v", false, "show the fusemanager version and exit")
	flag.StringVar(&action, "action", "", "action of fusemanager")
	flag.StringVar(&fuseStoreAddr, "fusestore-path", "/var/lib/containerd-stargz-grpc/fusestore.db", "address for the fusemanager's store")
	flag.StringVar(&address, "address", "/run/containerd-stargz-grpc/fuse-manager.sock", "address for the fusemanager's gRPC socket")
	flag.StringVar(&logLevel, "log-level", logrus.InfoLevel.String(), "set the logging level [trace, debug, info, warn, error, fatal, panic]")
	flag.StringVar(&logPath, "log-path", "", "path to fusemanager's logs, no log recorded if empty")

	flag.Parse()
}

func Run() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "failed to run fusemanager: %v", err)
		os.Exit(1)
	}
}

func run() error {
	parseFlags()
	if versionFlag {
		fmt.Printf("%s:\n", os.Args[0])
		fmt.Println("  Version: ", version.Version)
		fmt.Println("  Revision:", version.Revision)
		fmt.Println("")
		return nil
	}

	if fuseStoreAddr == "" || address == "" {
		return fmt.Errorf("fusemanager fusestore and socket path cannot be empty")
	}

	ctx := log.WithLogger(context.Background(), log.L)

	switch action {
	case "start":
		return startNew(ctx, logPath, address, fuseStoreAddr, logLevel)
	default:
		return runFuseManager(ctx)
	}
}

func startNew(ctx context.Context, logPath, address, fusestore, logLevel string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	args := []string{
		"-address", address,
		"-fusestore-path", fusestore,
		"-log-level", logLevel,
	}

	// we use shim-like approach to start new fusemanager process by self-invoking in the background
	// and detach it from parent
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid: true,
	}

	if logPath != "" {
		err := os.Remove(logPath)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		file, err := os.Create(logPath)
		if err != nil {
			return err
		}
		cmd.Stdout = file
		cmd.Stderr = file
	}

	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()

	if ready, err := waitUntilReady(ctx, 10); err != nil || !ready {
		if err != nil {
			return errors.Wrapf(err, "failed to start new fusemanager")
		}
		if !ready {
			return errors.Errorf("failed to start new fusemanager, fusemanager not ready")
		}
	}

	return nil
}

// waitUntilReady waits until fusemanager is ready to accept requests with timeout
func waitUntilReady(ctx context.Context, timeout int) (bool, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	grpcCli, err := newClient(timeoutCtx, address)
	if err != nil {
		return false, err
	}

	resp, err := grpcCli.Status(ctx, &pb.StatusRequest{})
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to call Status")
		return false, err
	}

	if resp.Status == FuseManagerNotReady {
		return false, nil
	}

	return true, nil
}

func runFuseManager(ctx context.Context) error {
	lvl, err := logrus.ParseLevel(logLevel)
	if err != nil {
		log.L.WithError(err).Fatal("failed to prepare logger")
	}

	logrus.SetLevel(lvl)
	logrus.SetFormatter(&logrus.JSONFormatter{
		TimestampFormat: log.RFC3339NanoFixed,
	})

	golog.SetOutput(log.G(ctx).WriterLevel(logrus.DebugLevel))

	// Prepare the directory for the socket
	if err := os.MkdirAll(filepath.Dir(address), 0700); err != nil {
		log.G(ctx).WithError(err).Errorf("failed to create directory %s", filepath.Dir(address))
		return err
	}

	// Try to remove the socket file to avoid EADDRINUSE
	if err := os.Remove(address); err != nil && !os.IsNotExist(err) {
		log.G(ctx).WithError(err).Error("failed to remove old socket file")
		return err
	}

	l, err := net.Listen("unix", address)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to listen socket")
		return err
	}

	server := grpc.NewServer()
	fm, err := NewFuseManager(ctx, l, server, fuseStoreAddr)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to configure manager server")
		return err
	}

	err = fm.clearMounts(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Errorf("failed to clear mounts")
		return err
	}

	pb.RegisterStargzFuseManagerServiceServer(server, fm)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, unix.SIGINT, unix.SIGTERM)
	go func() {
		sig := <-sigCh
		log.G(ctx).Infof("Got %v", sig)
		fm.server.Stop()
	}()

	if err = server.Serve(l); err != nil {
		log.G(ctx).WithError(err).Error("failed to serve fuse manager")
		return err
	}

	err = fm.Close(ctx)
	if err != nil {
		log.G(ctx).WithError(err).Error("failed to close fusemanager")
		return err
	}

	return err
}
