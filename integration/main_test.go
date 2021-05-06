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
	"os"
	"testing"

	shell "github.com/containerd/stargz-snapshotter/util/dockershell"
	"github.com/containerd/stargz-snapshotter/util/dockershell/compose"
	dexec "github.com/containerd/stargz-snapshotter/util/dockershell/exec"
	"github.com/containerd/stargz-snapshotter/util/dockershell/kind"
	"github.com/containerd/stargz-snapshotter/util/testutil"
)

const enableTestEnv = "ENABLE_INTEGRATION_TEST"

// TestMain is a main function for integration tests.
// This checks the system requirements the run tests.
func TestMain(m *testing.M) {
	if os.Getenv(enableTestEnv) != "true" {
		testutil.TestingL.Printf("%s is not true. skipping integration test", enableTestEnv)
		return
	}
	if err := shell.Supported(); err != nil {
		testutil.TestingL.Fatalf("shell pkg is not supported: %v", err)
	}
	if err := compose.Supported(); err != nil {
		testutil.TestingL.Fatalf("compose pkg is not supported: %v", err)
	}
	if err := kind.Supported(); err != nil {
		testutil.TestingL.Fatalf("kind pkg is not supported: %v", err)
	}
	if err := dexec.Supported(); err != nil {
		testutil.TestingL.Fatalf("dockershell/exec pkg is not supported: %v", err)
	}
	os.Exit(m.Run())
}
