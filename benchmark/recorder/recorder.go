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

package recorder

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/containerd/stargz-snapshotter/benchmark/types"
	"github.com/containerd/stargz-snapshotter/util/testutil"
)

// ResultRecorder holds benchmarking result and exports them in
// various supported formats (e.g. GNU plot, CSV, etc.).
type ResultRecorder struct {
	percentile            int
	percentileGranularity int
	results               map[string]map[string][]types.Result // TODO: record on disk?
}

var modeOrder = map[string]int{
	types.Legacy.String():       1,
	types.EStargzNoopt.String(): 2,
	types.EStargz.String():      3,
}

func sortImageKey(results map[string]map[string][]types.Result) []string {
	var keys []string
	for img := range results {
		keys = append(keys, img)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] > keys[j] })
	return keys
}

func sortModeKey(imgRes map[string][]types.Result) []string {
	var keys []string
	for m := range imgRes {
		keys = append(keys, m)
	}
	sort.Slice(keys, func(i, j int) bool { return modeOrder[keys[i]] < modeOrder[keys[j]] })
	return keys
}

func NewResultRecorder(percentile, percentileGranularity int) *ResultRecorder {
	return &ResultRecorder{
		percentile:            percentile,
		percentileGranularity: percentileGranularity,
	}
}

func (rr *ResultRecorder) Add(r types.Result) {
	if rr.results == nil {
		rr.results = make(map[string]map[string][]types.Result)
	}
	if _, ok := rr.results[r.Image]; !ok {
		rr.results[r.Image] = make(map[string][]types.Result)
	}
	rr.results[r.Image][r.Mode] = append(rr.results[r.Image][r.Mode], r)
}

func (rr *ResultRecorder) Table(w io.Writer) error {
	samples := rr.samplesNum()
	top := fmt.Sprintf(`
# Benchmarking Result (%d pctl.,samples=%d)

Runs on the ubuntu-18.04 runner on Github Actions.
`, rr.percentile, samples)
	if _, err := w.Write([]byte(top)); err != nil {
		return err
	}
	for _, img := range sortImageKey(rr.results) {
		imgRes := rr.results[img]
		top = fmt.Sprintf(`

## %s

|mode|pull(sec)|create(sec)|run(sec)|
---|---|---|---
`, img)
		if _, err := w.Write([]byte(top)); err != nil {
			return err
		}
		for _, m := range sortModeKey(imgRes) {
			modeRes := imgRes[m]
			pull, create, run, err := extractSec(rr.percentile, samples, modeRes)
			if err != nil {
				return err
			}
			if _, err := w.Write([]byte("|" + m + "|" + strings.Join([]string{
				fmt.Sprintf("%.3f", pull),
				fmt.Sprintf("%.3f", create),
				fmt.Sprintf("%.3f", run),
			}, "|") + "|\n")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (rr *ResultRecorder) CSV(w io.Writer) error {
	samples := rr.samplesNum()
	var indexL string
	for _, img := range sortImageKey(rr.results) {
		imgRes := rr.results[img]
		for _, m := range sortModeKey(imgRes) {
			indexL += "," + m
		}
	}
	if _, err := w.Write([]byte("image,operation" + indexL + "\n")); err != nil {
		return err
	}
	for _, img := range sortImageKey(rr.results) {
		imgRes := rr.results[img]
		var pullL, createL, runL string
		for _, m := range sortModeKey(imgRes) {
			modeRes := imgRes[m]
			pull, create, run, err := extractSec(rr.percentile, samples, modeRes)
			if err != nil {
				return err
			}
			pullL += "," + fmt.Sprintf("%.3f", pull)
			createL += "," + fmt.Sprintf("%.3f", create)
			runL += "," + fmt.Sprintf("%.3f", run)
		}
		if _, err := w.Write([]byte(img + ",pull" + pullL + "\n")); err != nil {
			return err
		}
		if _, err := w.Write([]byte(img + ",create" + createL + "\n")); err != nil {
			return err
		}
		if _, err := w.Write([]byte(img + ",run" + runL + "\n")); err != nil {
			return err
		}
	}
	return nil
}

func (rr *ResultRecorder) GNUPlot(w io.Writer, dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("dir path %v is not absolute", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	samples := rr.samplesNum()
	idx := 0
	var plots []string
	for _, img := range sortImageKey(rr.results) {
		imgRes := rr.results[img]
		data := []string{"mode pull create run"}
		for _, m := range sortModeKey(imgRes) {
			modeRes := imgRes[m]
			pull, create, run, err := extractSec(rr.percentile, samples, modeRes)
			if err != nil {
				return err
			}
			data = append(data, strings.Join([]string{m,
				fmt.Sprintf("%.3f", pull),
				fmt.Sprintf("%.3f", create),
				fmt.Sprintf("%.3f", run),
			}, " "))
		}
		dName := filepath.Join(dir, strings.ReplaceAll(img+".dat", ":", "-"))
		if err := ioutil.WriteFile(dName, []byte(strings.Join(data, "\n")), 0666); err != nil {
			return err
		}
		nt := ""
		if idx > 0 {
			nt = "notitle"
		}
		plots = append(plots, strings.Join([]string{
			`newhistogram "` + img + `"`,
			`"` + dName + `" u 2:xtic(1) fs pattern 1 lt -1 ` + nt,
			`"" u 3 fs pattern 2 lt -1 ` + nt,
			`"" u 4 fs pattern 3 lt -1 ` + nt,
		}, ", "))
		idx++
	}
	p, err := testutil.ApplyTextTemplateErr(`
set title "Time to take for starting up containers({{.Percentile}} pctl., {{.SamplesNum}} samples)"
set terminal png size 1000, 750
set style data histogram
set style histogram rowstack gap 1
set style fill solid 1.0 border -1
set key autotitle columnheader
set xtics rotate by -45
set ylabel 'time[sec]'
set lmargin 10
set rmargin 5
set tmargin 5
set bmargin 7
plot \
{{.PlotLines}}
`, struct {
		Percentile int
		SamplesNum int
		PlotLines  string
	}{
		Percentile: rr.percentile,
		SamplesNum: samples,
		PlotLines:  strings.Join(plots, `, \`+"\n"),
	})
	if err != nil {
		return err
	}
	pName := filepath.Join(dir, "result.plt")
	if err := ioutil.WriteFile(pName, p, 0666); err != nil {
		return err
	}
	cmd := exec.Command("gnuplot", pName)
	_, stderr := testutil.TestingLogDest()
	cmd.Stdout = w
	cmd.Stderr = stderr
	return cmd.Run()
}

func (rr *ResultRecorder) GNUPlotGranularity(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("dir path %v is not absolute", dir)
	}
	if _, err := os.Stat(dir); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
	}
	var (
		csvDir = filepath.Join(dir, "csv")
		pngDir = filepath.Join(dir, "png")
		pltDir = filepath.Join(dir, "plt")
		rawDir = filepath.Join(dir, "raw")
	)
	for _, d := range []string{csvDir, pngDir, pltDir, rawDir} {
		if err := os.Mkdir(d, 0755); err != nil {
			return err
		}
	}
	samples := rr.samplesNum()
	for _, img := range sortImageKey(rr.results) {
		imgRes := rr.results[img]
		csvF, err := os.Create(filepath.Join(csvDir, "result-"+img+".csv"))
		if err != nil {
			return err
		}
		defer csvF.Close()
		if _, err := csvF.Write([]byte("image,mode,operation,percentile,time\n")); err != nil {
			return err
		}
		for _, m := range sortModeKey(imgRes) {
			modeRes := imgRes[m]
			opRes := map[string][]int64{}
			for _, r := range modeRes {
				opRes["pull"] = append(opRes["pull"], r.ElapsedPullMilliSec)
				opRes["create"] = append(opRes["create"], r.ElapsedCreateMilliSec)
				opRes["run"] = append(opRes["run"], r.ElapsedRunMilliSec)
			}
			for op, res := range opRes {
				var data []string
				for i := 0; i < 100; i += rr.percentileGranularity {
					pctlM, err := percentile(i, res)
					if err != nil {
						return err
					}
					pctl := float64(pctlM) / 1000.0 // convert to sec
					data = append(data, fmt.Sprintf("%d %.3f", i, pctl))
					csvL := fmt.Sprintf("%s,%s,%s,%d,%.3f\n", img, m, op, i, pctl)
					if _, err := csvF.Write([]byte(csvL)); err != nil {
						return err
					}
				}
				dName := filepath.Join(rawDir, "result-"+img+"-"+m+"-"+op+".dat")
				if err := ioutil.WriteFile(dName, []byte(strings.Join(data, "\n")), 0666); err != nil {
					return err
				}
				p, err := testutil.ApplyTextTemplateErr(`
set output '{{.GraphFile}}'
set title "{{.Operation}} of {{.Image}}/{{.Mode}} ({{.SamplesNum}} samples)"
set terminal png size 500, 375
set boxwidth 0.5 relative
set style fill solid 1.0 border -1
set xlabel 'percentile'
set ylabel 'time[sec]'
plot "{{.DataFileName}}" using 0:2:xtic(1) with boxes lw 1 lc rgb "black" notitle
`, struct {
					GraphFile    string
					Operation    string
					Image        string
					Mode         string
					SamplesNum   int
					DataFileName string
				}{
					GraphFile:    filepath.Join(pngDir, "result-"+img+"-"+m+"-"+op+".png"),
					Operation:    op,
					Image:        img,
					Mode:         m,
					SamplesNum:   samples,
					DataFileName: dName,
				})
				if err != nil {
					return err
				}
				pName := filepath.Join(pltDir, "result-"+img+"-"+m+"-"+op+".plt")
				if err := ioutil.WriteFile(pName, p, 0666); err != nil {
					return err
				}
				cmd := exec.Command("gnuplot", pName)
				stdout, stderr := testutil.TestingLogDest()
				cmd.Stdout = stdout
				cmd.Stderr = stderr
				if err := cmd.Run(); err != nil {
					return err
				}

			}
		}
	}
	return nil
}

func (rr *ResultRecorder) samplesNum() int {
	samplesNum := -1
	for _, img := range sortImageKey(rr.results) {
		imgRes := rr.results[img]
		for _, m := range sortModeKey(imgRes) {
			modeRes := imgRes[m]
			if samplesNum < 0 || len(modeRes) < samplesNum {
				samplesNum = len(modeRes)
			}
		}
	}
	return samplesNum
}

func extractSec(pctl int, samples int, res []types.Result) (float64, float64, float64, error) {
	var pull, create, run []int64
	for _, r := range res {
		pull = append(pull, r.ElapsedPullMilliSec)
		create = append(create, r.ElapsedCreateMilliSec)
		run = append(run, r.ElapsedRunMilliSec)
	}
	var pctls []int64
	for _, x := range [][]int64{pull, create, run} {
		i, err := testutil.RandomUInt64()
		if err != nil {
			return 0, 0, 0, err
		}
		rand.Seed(int64(i))
		rand.Shuffle(len(x), func(i, j int) { x[i], x[j] = x[j], x[i] })
		p, err := percentile(pctl, x[:samples])
		if err != nil {
			return 0, 0, 0, err
		}
		pctls = append(pctls, p)
	}

	return float64(pctls[0]) / 1000.0, float64(pctls[1]) / 1000.0, float64(pctls[2]) / 1000.0, nil
}

// percentile function returns the specified percentile value relying on numpy.
// See also: https://numpy.org/doc/stable/reference/generated/numpy.percentile.html
func percentile(p int, v []int64) (int64, error) {
	f, err := ioutil.TempFile("", "pctlcalctmp")
	if err != nil {
		return 0, fmt.Errorf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	for _, vn := range v {
		if _, err := f.Write([]byte(fmt.Sprintf("%d\n", vn))); err != nil {
			return 0, fmt.Errorf("failed to write to temp file: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("failed to close temp file: %v", err)
	}
	pBin := "python"
	if _, err := exec.LookPath(pBin); err != nil {
		pBin = "python3"
		if _, err := exec.LookPath(pBin); err != nil {
			return 0, fmt.Errorf("python not found")
		}
	}
	cmd := exec.Command(pBin)
	cmd.Stdin = bytes.NewReader([]byte(fmt.Sprintf(`
import numpy as np
f = open('%s', 'r')
arr = []
for line in f.readlines():
    arr.append(float(line))
f.close()
print(int(np.percentile(a=np.array(arr), q=%d, interpolation='linear')))
`, f.Name(), p)))
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("failed to run command: %v", err)
	}
	res, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("failed to parse data %v: %v", string(out), err)
	}
	return res, nil
}
