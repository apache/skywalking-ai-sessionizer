// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package tests_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionview"
)

// TestDocumentExampleIsCurrent regenerates the complete asz.view example
// on the format page from the fixture scenario, the way make
// asz-view-example does, and fails when the committed file differs: a
// change to the document's shape must show in the docs in the same change.
func TestDocumentExampleIsCurrent(t *testing.T) {
	sc, err := scenario.Load("scenarios/fixture.yaml")
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(out)
	for {
		r, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session})
		if err != nil {
			t.Fatal(err)
		}
		if !r.Changed() || !r.More {
			break
		}
	}
	c, err := view.New(z, nil).Load(b.Session)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := c.Build()
	if err != nil {
		t.Fatal(err)
	}
	got, err := sessionview.MarshalYAML(doc)
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("..", "docs", "en", "formats", "asz-view-example.yaml")
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("%s is not what the fixture scenario produces now; run make asz-view-example and commit the result", path)
	}
}
