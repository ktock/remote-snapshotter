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

package types

type Mode int

const (
	Legacy Mode = iota
	EStargz
	EStargzNoopt
)

func (m Mode) String() string {
	switch m {
	case Legacy:
		return "legacy"
	case EStargz:
		return "eStargz"
	case EStargzNoopt:
		return "eStargz-noopt"
	}
	return "<unknown>"
}

type BenchEchoHello struct{}

type BenchCmdArgWait struct {
	Args     []string
	Env      []string
	WaitLine string
}

type BenchCmdArg struct {
	Args []string
}

type BenchCmdStdin struct {
	Args   []string
	Stdin  string
	Mounts []MountInfo
}

type MountInfo struct {
	Src string
	Dst string
}

type Result struct {
	Mode                  string `json:"mode"`
	Image                 string `json:"image"`
	ElapsedPullMilliSec   int64  `json:"elapsed_pull"`
	ElapsedCreateMilliSec int64  `json:"elapsed_create"`
	ElapsedRunMilliSec    int64  `json:"elapsed_run"`
}
