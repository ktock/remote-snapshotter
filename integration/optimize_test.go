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

package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/containerd/containerd/content"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/stargz-snapshotter/estargz"
	"github.com/containerd/stargz-snapshotter/util/containerdutil"
	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/testutil"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/rs/xid"
)

// TestOptimize tests eStargz optimization works as expected.
func TestOptimize(t *testing.T) {
	t.Parallel()
	var (
		registryHost = "registry" + xid.New().String() + ".test"
		registryUser = "dummyuser"
		registryPass = "dummypass"
		orgImageTag  = registryHost + "/test/test:org-" + xid.New().String()
		buildkitURL  = fmt.Sprintf("https://github.com/moby/buildkit/releases/download/%s/buildkit-%s.linux-%s.tar.gz", testutil.BuildKitVersion, testutil.BuildKitVersion, runtime.GOARCH)
	)

	// Setup environment
	sh, _, done := newShellWithRegistry(t, registryHost, registryUser, registryPass)
	defer done()
	sh.Pipe(nil, shell.C("curl", "-Ls", buildkitURL), shell.C("tar", "zxv", "-C", "/usr/local"))

	// Startup necessary apps and prepare a sample image
	sh.
		Gox("buildkitd").
		Retry(100, "buildctl", "du").
		Gox("containerd", "--log-level", "debug").
		Retry(100, "nerdctl", "version").
		X("nerdctl", "build", "-t", orgImageTag, sampleContext(t, sh)).
		X("nerdctl", "push", orgImageTag)

	// Test optimizing image
	tests := []struct {
		name           string
		convertCommand []string
		wantLayers     [][]string
	}{
		{
			name:           "optimize",
			convertCommand: []string{"ctr-remote", "i", "optimize", "--oci", "--entrypoint", `[ "/accessor" ]`},
			wantLayers: [][]string{
				{"accessor", "a.txt", ".prefetch.landmark", "b.txt", "stargz.index.json"},
				{"c.txt", ".prefetch.landmark", "d.txt", "stargz.index.json"},
				{".no.prefetch.landmark", "e.txt", "stargz.index.json"},
			},
		},
		{
			name:           "no-optimize",
			convertCommand: []string{"ctr-remote", "i", "optimize", "--no-optimize", "--oci"},
			wantLayers: [][]string{
				{".no.prefetch.landmark", "a.txt", "accessor", "b.txt", "stargz.index.json"},
				{".no.prefetch.landmark", "c.txt", "d.txt", "stargz.index.json"},
				{".no.prefetch.landmark", "e.txt", "stargz.index.json"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testImage(t, sh, orgImageTag, registryHost, tt.convertCommand, tt.wantLayers...)
		})
	}
}

func sampleContext(t *testing.T, sh *shell.Shell) string {
	tmpContext, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	sampleDockerfile := `
FROM scratch

COPY ./a.txt ./b.txt accessor /
COPY ./c.txt ./d.txt /
COPY ./e.txt /

ENTRYPOINT ["/accessor"]
`
	if err := testutil.WriteFileContents(sh, filepath.Join(tmpContext, "Dockerfile"), []byte(sampleDockerfile), 0600); err != nil {
		t.Fatalf("failed to write dockerfile to %v: %v", tmpContext, err)
	}
	for _, sample := range []string{"a", "b", "c", "d", "e"} {
		if err := testutil.WriteFileContents(sh, filepath.Join(tmpContext, sample+".txt"), []byte(sample), 0600); err != nil {
			t.Fatalf("failed to write %v: %v", sample, err)
		}
	}
	accessorSrc := filepath.Join("/tmp", "accessor", "main.go")
	err = testutil.WriteFileContents(sh, accessorSrc, []byte(`
package main

import (
	"os"
)

func main() {
	targets := []string{"/a.txt", "/c.txt"}
	for _, t := range targets {
		f, err := os.Open(t)
		if err != nil {
			panic("failed to open file")
		}
		f.Close()
	}
}
`), 0600)
	if err != nil {
		t.Fatalf("failed to write sample go file: %v", err)
	}
	sh.X("go", "build", "-ldflags", `-extldflags "-static"`,
		"-o", filepath.Join(tmpContext, "accessor"), accessorSrc)

	return tmpContext
}

func testImage(t *testing.T, sh *shell.Shell, sampleImageTag string, registryHost string, testCmd []string, want ...[]string) {
	// convert and extract the image
	dstTag := registryHost + "/test/test:" + xid.New().String()
	imgDir, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	sh.
		X(append(testCmd, sampleImageTag, dstTag)...).
		Pipe(nil, shell.C("nerdctl", "save", dstTag), shell.C("tar", "xv", "-C", imgDir))

	// get target manifest from exported image directory
	cs, indexDigest := newOCILayoutProvider(t, sh, imgDir)
	desc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageIndex,
		Digest:    indexDigest,
	}
	mfstDesc, err := containerdutil.ManifestDesc(context.Background(), cs, desc, platforms.Default())
	if err != nil {
		t.Fatalf("failed to get manifest descriptor: %v", err)
	}
	ra, err := cs.ReaderAt(context.Background(), mfstDesc)
	if err != nil {
		t.Fatalf("failed to get manifest readerat: %v", err)
	}
	var mfst ocispec.Manifest
	if err := json.NewDecoder(io.NewSectionReader(ra, 0, ra.Size())).Decode(&mfst); err != nil {
		t.Fatalf("failed to decode manifest")
	}

	// Check layers have expected contents
	var toc [][]string
	for _, l := range mfst.Layers {
		toc = append(toc, strings.Fields(string(
			sh.O("tar", "--list", "-f", filepath.Join(imgDir, ociPathOf(l.Digest))),
		)))
	}
	if !reflect.DeepEqual(want, toc) {
		t.Fatalf("unexpected list of layers %+v; want %+v", toc, want)
	}

	// Check TOC digest is valid
	for _, l := range mfst.Layers {
		wantTOCDigestString, ok := l.Annotations[estargz.TOCJSONDigestAnnotation]
		if !ok {
			t.Fatalf("TOCJSON Digest annotation not found in layer %+v", l)
		}
		wantTOCDigest, err := digest.Parse(wantTOCDigestString)
		if err != nil {
			t.Fatalf("failed to parse TOC JSON Digest %v: %v", wantTOCDigestString, err)
		}
		gotTOCDigest := digest.FromBytes(sh.O("tar", "-xOf",
			filepath.Join(imgDir, ociPathOf(l.Digest)), "stargz.index.json"))
		if wantTOCDigest != gotTOCDigest {
			t.Fatalf("invalid TOC JSON got %v; want %v", gotTOCDigest, wantTOCDigest)
		}
	}
}

func ociPathOf(dgst digest.Digest) string {
	return filepath.Join("blobs", dgst.Algorithm().String(), dgst.Encoded())
}

func newOCILayoutProvider(t *testing.T, sh *shell.Shell, dir string) (cs *ociLayoutProvider, root digest.Digest) {
	var index ocispec.Index
	rawIndex := sh.O("cat", filepath.Join(dir, "index.json"))
	if err := json.Unmarshal(rawIndex, &index); err != nil {
		t.Fatalf("failed to parse index: %v", err)
	}
	indexDigest := digest.FromBytes(rawIndex)
	if err := testutil.WriteFileContents(sh, filepath.Join(dir, ociPathOf(indexDigest)), rawIndex, 0600); err != nil {
		t.Fatalf("failed to write OCI index: %v", err)
	}
	return &ociLayoutProvider{sh, dir}, indexDigest
}

type ociLayoutProvider struct {
	sh  *shell.Shell
	dir string
}

func (p *ociLayoutProvider) ReaderAt(ctx context.Context, desc ocispec.Descriptor) (content.ReaderAt, error) {
	return &nopCloser{bytes.NewReader(p.sh.O("cat", filepath.Join(p.dir, ociPathOf(desc.Digest))))}, nil
}

type nopCloser struct {
	*bytes.Reader
}

func (nc *nopCloser) Close() error {
	return nil
}

// TestMountAndNetwork tests ctr-remote's mount and NW feature work.
func TestMountAndNetwork(t *testing.T) {
	t.Parallel()
	var (
		customCNIConflistPath = "/etc/tmp/cni/net.d/test.conflist"
		customCNIBinPath      = "/opt/tmp/cni/bin"
		cniVersion            = "v0.9.1"
		cniURL                = fmt.Sprintf("https://github.com/containernetworking/plugins/releases/download/%s/cni-plugins-linux-%s-%s.tgz", cniVersion, runtime.GOARCH, cniVersion)

		// Image for doing network-related tests
		// ```
		// FROM ubuntu:20.04
		// RUN apt-get update && apt-get install -y curl iproute2
		// ```
		networkMountTestOrgImageTag = "ghcr.io/stargz-containers/ubuntu:20.04-curl-ip"
	)

	// Prepare environment
	sh, done := newSnapshotterBaseShell(t)
	defer done()
	sh.
		X("apt-get", "update", "-y").
		X("apt-get", "--no-install-recommends", "install", "-y", "iptables").
		X("mkdir", "-p", customCNIBinPath).
		Pipe(nil, shell.C("curl", "-Ls", cniURL), shell.C("tar", "zxv", "-C", customCNIBinPath))
	cniConf := `
{
  "cniVersion": "0.4.0",
  "name": "test",
  "plugins" : [{
    "type": "bridge",
    "bridge": "test0",
    "isDefaultGateway": true,
    "forceAddress": false,
    "ipMasq": true,
    "hairpinMode": true,
    "ipam": {
      "type": "host-local",
      "subnet": "10.10.0.0/16"
    }
  },
  {
    "type": "loopback"
  }]
}
`
	if err := testutil.WriteFileContents(sh, customCNIConflistPath, []byte(cniConf), 0755); err != nil {
		t.Fatalf("failed to write cni config to %v: %v", customCNIConflistPath, err)
	}
	sh.
		Gox("containerd", "--log-level", "debug").
		Retry(100, "nerdctl", "version").
		X("nerdctl", "pull", networkMountTestOrgImageTag).

		// Make bridge plugin manipulate iptables instead of nftables as this test runs
		// in a Docker container that network is configured with iptables.
		// c.f. https://github.com/moby/moby/issues/26824
		X("update-alternatives", "--set", "iptables", "/usr/sbin/iptables-legacy")

	// Run sample optimize comand
	mountDir, err := testutil.TempDir(sh)
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	testHosts := map[string]string{
		"testhost": "1.2.3.4",
		"test2":    "5.6.7.8",
	}
	sh.X(
		"ctr-remote", "i", "optimize", "--oci", "--period=20", "--cni",
		"--cni-plugin-conf-dir", filepath.Dir(customCNIConflistPath),
		"--cni-plugin-dir", "/opt/tmp/cni/bin",
		"--add-hosts", map2hostsOpts(testHosts),
		"--dns-nameservers", "8.8.8.8",
		"--mount", fmt.Sprintf("type=bind,src=%s,dst=/mnt,options=bind", mountDir),
		"--entrypoint", `[ "/bin/bash", "-c" ]`,
		"--args", `[ "curl example.com > /mnt/result_page && ip a show dev eth0 ; echo -n $? > /mnt/if_exists && ip a > /mnt/if_info && cat /etc/hosts > /mnt/hosts" ]`,
		networkMountTestOrgImageTag, "test:1",
	)

	// Check all necerssary files are created.
	for _, f := range []string{"if_exists", "result_page", "if_info", "hosts"} {
		sh.X("test", "-f", filepath.Join(mountDir, f))
	}
	gotHosts := parseHosts(sh.O("cat", filepath.Join(mountDir, "hosts")))
	for host, wantIP := range testHosts {
		gotIP, ok := gotHosts[host]
		if !ok {
			t.Fatalf("IP for host %q not configured", host)
		}
		if gotIP != wantIP {
			t.Fatalf("unexpected IP %q; want %q", gotIP, wantIP)
		}
	}
	if ifData := sh.O("cat", filepath.Join(mountDir, "if_exists")); string(ifData) != "0" {
		t.Fatalf("interface didn't configured: %v", string(ifData))
	}
	resp, err := http.Get("https://example.com/")
	if err != nil {
		t.Fatalf("failed to get sample page: %v", err)
	}
	defer resp.Body.Close()
	samplePage, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read sample page: %v", err)
	}
	rpData := sh.O("cat", filepath.Join(mountDir, "result_page"))
	spDgst := digest.FromBytes(samplePage)
	rpDgst := digest.FromBytes(rpData)
	if spDgst != rpDgst {
		t.Fatalf("unexpected page contents %v; want %v", spDgst, rpDgst)
		t.Logf("got page: %v", string(rpData))
	}
}

func parseHosts(data []byte) map[string]string {
	resolve := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		conf := strings.Fields(scanner.Text())
		if len(conf) < 1 {
			continue
		}
		if strings.HasPrefix(conf[0], "#") {
			continue
		}
		ip := conf[0]
		for _, h := range conf[1:] {
			resolve[h] = ip
		}
	}
	return resolve
}

func map2hostsOpts(m map[string]string) (o string) {
	for h, ip := range m {
		o = o + "," + h + ":" + ip
	}
	if len(o) < 1 {
		return ""
	}
	return o[1:]
}
