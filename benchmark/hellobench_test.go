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

package benchmark

import (
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/containerd/stargz-snapshotter/benchmark/containerd"
	"github.com/containerd/stargz-snapshotter/benchmark/prepare"
	"github.com/containerd/stargz-snapshotter/benchmark/recorder"
	"github.com/containerd/stargz-snapshotter/benchmark/types"
	"github.com/containerd/stargz-snapshotter/util/testutil"
)

// Benchmark script based on HelloBench [FAST '16].
// https://github.com/Tintri/hello-bench

const (
	samplesEnv               = "BENCHMARK_SAMPLES_NUM"
	percentileEnv            = "BENCHMARK_PERCENTILE"
	percentileGranularityEnv = "BENCHMARK_PERCENTILE_GRANULARITY"
	repositoryEnv            = "BENCHMARK_TARGET_REPOSITORY"
	resultDirEnv             = "BENCHMARK_RESULT_DIR"
	targetsEnv               = "BENCHMARK_TARGETS"
	targetModesEnv           = "BENCHMARK_TARGET_MODES"
	logFileEnv               = "BENCHMARK_LOG_FILE"
)

func benchmarkTargets(pRoot string) map[string]interface{} {
	// List of supported benchmarking images
	return map[string]interface{}{

		// echo hello images
		"alpine:3.10.2": types.BenchEchoHello{},
		"fedora:30":     types.BenchEchoHello{},

		// server images
		"postgres:13.1": types.BenchCmdArgWait{
			WaitLine: "database system is ready to accept connections",
			Env:      []string{"POSTGRES_PASSWORD=abc"},
		},
		"rethinkdb:2.3.6": types.BenchCmdArgWait{
			WaitLine: "Server ready",
		},
		"glassfish:4.1-jdk8": types.BenchCmdArgWait{
			WaitLine: "Running GlassFish",
		},
		"drupal:8.7.6": types.BenchCmdArgWait{
			WaitLine: "apache2 -D FOREGROUND",
		},
		"jenkins:2.60.3": types.BenchCmdArgWait{
			WaitLine: "Jenkins is fully up and running",
		},
		"redis:5.0.5": types.BenchCmdArgWait{
			WaitLine: "Ready to accept connections",
		},
		"tomcat:10.0.0-jdk15-openjdk-buster": types.BenchCmdArgWait{
			WaitLine: "Server startup",
		},
		"mariadb:10.5": types.BenchCmdArgWait{
			WaitLine: "mysqld: ready for connections",
			Env:      []string{"MYSQL_ROOT_PASSWORD=abc"},
		},
		"wordpress:5.7": types.BenchCmdArgWait{
			WaitLine: "apache2 -D FOREGROUND",
		},

		// languages, etc.
		"php:7.3.8": types.BenchCmdStdin{
			Stdin: `php -r 'echo "hello";'; exit\n`,
		},
		"r-base:3.6.1": types.BenchCmdStdin{
			Stdin: `sprintf("hello")\nq()\n`,
			Args:  []string{"R", "--no-save"},
		},
		"jruby:9.2.8.0": types.BenchCmdStdin{
			Stdin: `jruby -e 'puts "hello"'; exit\n`,
		},
		"gcc:10.2.0": types.BenchCmdStdin{
			Stdin: "cd /src; gcc main.c; ./a.out; exit\n",
			Mounts: []types.MountInfo{
				{
					Src: filepath.Join(pRoot, "benchmark/src/gcc"),
					Dst: "/src",
				},
			},
		},
		"golang:1.12.9": types.BenchCmdStdin{
			Stdin: "cd /go/src; go run main.go; exit\n",
			Mounts: []types.MountInfo{
				{
					Src: filepath.Join(pRoot, "benchmark/src/go"),
					Dst: "/go/src",
				},
			},
		},
		"python:3.9": types.BenchCmdArg{
			Args: []string{"python", "-c", `print("hello")`},
		},
		"perl:5.30": types.BenchCmdArg{
			Args: []string{"perl", "-e", `print("hello")`},
		},
		"pypy:3.5": types.BenchCmdArg{
			Args: []string{"pypy3", "-c", `print("hello")`},
		},
		"node:13.13.0": types.BenchCmdArg{
			Args: []string{"node", "-e", `console.log("hello")`},
		},
	}
}

// TestBenchmarkHelloBench measures performance of lazy pulling based on HelloBench [FAST '16].
// Note that we don't use Go's benchmarking system (BenchmarkXXX) because HelloBench
// doesn't seem to fit well to it. We'll switch to Go's benchmarking system once
// we find a good way to achieve that.
func TestBenchmarkHelloBench(t *testing.T) {
	if os.Getenv(targetsEnv) == "" {
		t.Skipf("%s is not specified. skipping benchmark test", targetsEnv)
	}
	if logfile := os.Getenv(logFileEnv); logfile != "" {
		done, err := testutil.StreamTestingLogToFile(logfile)
		if err != nil {
			t.Fatalf("failed to setup log streaming: %v", err)
		}
		defer done()
		testutil.TestingL.Printf("streaming log into %v", logfile)
	}
	if err := containerd.Supported(); err != nil {
		t.Fatalf("containerd runner is not supported: %v", err)
	}
	var (
		pRoot           = testutil.GetProjectRoot(t)
		benchmarkImages = benchmarkTargets(pRoot)

		// mandatory envvars
		targets               = strings.Split(getenvStr(t, targetsEnv), " ")
		samples               = getenvInt(t, samplesEnv)
		percentile            = getenvInt(t, percentileEnv)
		percentileGranularity = getenvInt(t, percentileGranularityEnv)
		repository            = getenvStr(t, repositoryEnv)

		// optional envvars
		resultDir   = os.Getenv(resultDirEnv)
		targetModes = getenvModes(t, targetModesEnv)
	)
	if len(targets) == 0 {
		for i := range benchmarkImages {
			targets = append(targets, i)
		}
	}
	var notfound []string
	for _, t := range targets {
		if _, ok := benchmarkImages[t]; !ok {
			notfound = append(notfound, t)
		}
	}
	if len(notfound) > 0 {
		t.Fatalf("some targets not found: %v", notfound)
	}
	var err error
	if resultDir == "" {
		resultDir, err = ioutil.TempDir("", "resultdir")
		if err != nil {
			t.Fatalf("failed to create result dir")
		}
		testutil.TestingL.Printf("Warn: result dir hasn't been specified: output into %v\n", resultDir)
	}

	// Run benchmark with containerd
	runner := containerd.NewRunner(t, repository)
	defer runner.Cleanup()
	var list []struct {
		name  string
		bench interface{}
	}
	for _, n := range targets {
		list = append(list, struct {
			name  string
			bench interface{}
		}{n, benchmarkImages[n]})
	}
	rec := recorder.NewResultRecorder(percentile, percentileGranularity)
	for i := 0; i < samples; i++ {
		x, err := testutil.RandomUInt64()
		if err != nil {
			t.Fatalf("failed to get random value: %v", err)
		}
		rand.Seed(int64(x))
		rand.Shuffle(len(targetModes), func(i, j int) { targetModes[i], targetModes[j] = targetModes[j], targetModes[i] })
		x, err = testutil.RandomUInt64()
		if err != nil {
			t.Fatalf("failed to get random value: %v", err)
		}
		rand.Seed(int64(x))
		rand.Shuffle(len(targets), func(i, j int) { targets[i], targets[j] = targets[j], targets[i] })
		for _, b := range list {
			for _, m := range targetModes {
				testutil.TestingL.Printf("===== Measuring [%d] %v (%v) ====\n", i, b.name, m)
				rec.Add(runner.Run(t, b.name, b.bench, m)) // TODO: retry on error
			}
		}
	}
	testutil.TestingL.Printf("RESULT: %v\n", resultDir)
	output(t, rec, resultDir)
}

func output(t *testing.T, rec *recorder.ResultRecorder, resultDir string) {
	pImg, err := os.Create(filepath.Join(resultDir, "result.png"))
	if err != nil {
		t.Fatalf("failed to create plot result png file: %v", err)
	}
	defer pImg.Close()
	if err := rec.GNUPlot(pImg, resultDir); err != nil {
		t.Fatalf("failed to plot result: %v", err)
	}

	if err := rec.GNUPlotGranularity(resultDir); err != nil {
		t.Fatalf("failed to plot granularity result: %v", err)
	}

	tableF, err := os.Create(filepath.Join(resultDir, "result.md"))
	if err != nil {
		t.Fatalf("failed to create table result md file: %v", err)
	}
	defer tableF.Close()
	if err := rec.Table(tableF); err != nil {
		t.Fatalf("failed to create table result: %v", err)
	}

	csvF, err := os.Create(filepath.Join(resultDir, "result.csv"))
	if err != nil {
		t.Fatalf("failed to create result csv file: %v", err)
	}
	defer csvF.Close()
	if err := rec.CSV(csvF); err != nil {
		t.Fatalf("failed to plot result: %v", err)
	}
}

func getenvStr(t *testing.T, env string) string {
	v := os.Getenv(env)
	if v == "" {
		t.Fatalf("env %v must be specified", env)
	}
	return v
}

func getenvInt(t *testing.T, env string) int {
	v, err := strconv.ParseInt(os.Getenv(env), 10, 32)
	if err != nil {
		t.Fatalf("failed to parse env %v: %v", env, err)
	}
	return int(v)
}

func getenvModes(t *testing.T, env string) []types.Mode {
	var res []types.Mode
	mStr := strings.Split(os.Getenv(env), " ")
	for _, m := range mStr {
		switch m {
		case "legacy":
			res = append(res, types.Legacy)
		case "estargz", "eStargz":
			res = append(res, types.EStargz)
		case "estargz-noopt", "eStargz-noopt":
			res = append(res, types.EStargzNoopt)
		case "":
			// nop
		default:
			t.Fatalf("unknown mode %v", m)
		}
	}
	if len(res) == 0 { // target to all modes by default
		res = []types.Mode{types.Legacy, types.EStargz, types.EStargzNoopt}
	}
	return res
}

const (
	prepareTargetImagesEnv = "PREPARE_TARGETS"
	prepareTargetRepoEnv   = "PREPARE_TARGET_REPOSITORY"
	prepareTargetModesEnv  = "PREPARE_TARGET_MODES"
	prepareEnable          = "PREPARE_ENABLE"
)

// TestBenchmarkPrepare can be used for preparing images used for benchmark
// This is enabled only when ENABLE_PREPARE=true is specified
func TestBenchmarkPrepare(t *testing.T) {
	if os.Getenv(prepareEnable) != "true" {
		t.Skipf("preparation is not enabled. specify %s=true to enable; skipping", prepareEnable)
	}
	if err := prepare.Supported(); err != nil {
		t.Fatalf("preparation is not supported: %v", err)
	}
	var (
		pRoot           = testutil.GetProjectRoot(t)
		benchmarkImages = benchmarkTargets(pRoot)

		targetRepo   = getenvStr(t, prepareTargetRepoEnv)
		targetImages = strings.Split(getenvStr(t, prepareTargetImagesEnv), " ")
		targetModes  = getenvModes(t, prepareTargetModesEnv)
	)
	var notfound []string
	for _, t := range targetImages {
		if _, ok := benchmarkImages[t]; !ok {
			notfound = append(notfound, t)
		}
	}
	if len(notfound) > 0 {
		t.Fatalf("some targets not found: %v", notfound)
	}

	p := prepare.NewPreparer(t)
	defer p.Cleanup()
	for _, target := range targetImages {
		for _, mode := range targetModes {
			b, ok := benchmarkImages[target]
			if !ok {
				t.Fatalf("unknown target %v", target)
			}
			p.Prepare(t, target, b, mode, targetRepo)
		}
	}
}
