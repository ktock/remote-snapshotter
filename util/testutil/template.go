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

package testutil

import (
	"bytes"
	"testing"
	"text/template"

	"github.com/opencontainers/go-digest"
)

// ApplyTextTemplate applies the config to the specified template. testing.T.Fatalf will
// be called on error.
func ApplyTextTemplate(t *testing.T, temp string, config interface{}) string {
	data, err := ApplyTextTemplateErr(temp, config)
	if err != nil {
		t.Fatalf("failed to apply config %v to template", config)
	}
	return string(data)
}

// ApplyTextTemplateErr applies the config to the specified template.
func ApplyTextTemplateErr(temp string, conf interface{}) ([]byte, error) {
	var buf bytes.Buffer
	if err := template.Must(template.New(digest.FromString(temp).String()).Parse(temp)).Execute(&buf, conf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
