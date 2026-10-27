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

package view

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// A snapshot's files are verified against the rounds its load checked
// against the marker, never against the chain listed again. Between the
// checked fold and the check of the files, the rounds can go and a file can
// change in place, keeping its identity: a check that lists the chain again
// then finds no round to hold the file to, and passes changed text.
func TestASnapshotVerifiesTheRoundsItsLoadChecked(t *testing.T) {
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "fixture.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	b, err := scenario.Build(sc, scenario.FormatSD, out, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	z := storage.NewZone(out)
	if _, err := parse.Session(z, parse.Options{Conversation: b.Session, Session: b.Session}); err != nil {
		t.Fatal(err)
	}
	id := b.Session
	chain := sessionflow.OpenChain(z.Root(), id)
	v, err := chain.Fold()
	if err != nil {
		t.Fatal(err)
	}
	files, err := storage.LandedFiles(z, v.Session)
	if err != nil || len(files) == 0 {
		t.Fatalf("%d landed files: %v", len(files), err)
	}
	landed := map[uint64]string{}
	for _, lf := range files {
		rel, _ := filepath.Rel(z.Root(), lf.Path)
		landed[lf.Seq] = filepath.ToSlash(rel)
	}
	snap := &Snapshot{Conversation: id, Session: v.Session, Round: v.Round, Digest: v.Digest, Landed: landed, Fatal: func(error) {}}
	srv := New(z, nil)
	srv.SetSnapshot(snap)
	c, err := srv.Load(id)
	if err != nil {
		t.Fatal(err)
	}

	// What a pause after the checked fold would let happen.
	rounds, err := chain.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, rf := range rounds {
		if err := os.Remove(rf.Path); err != nil {
			t.Fatal(err)
		}
	}
	changeInPlace(t, files[0].Path)
	c.shared = nil
	if _, err := c.foldParts(); !errors.Is(err, ErrEvidence) {
		t.Fatalf("a changed file under rounds that went: %v", err)
	}
}

// changeInPlace rewrites a landed file as valid Session Data with one text
// changed, through the same file, so its identity stays.
func changeInPlace(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h, recs, err := sessiondata.All(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	changed := false
	for _, r := range recs {
		for i := range r.Parts {
			if r.Parts[i].Text != "" && !changed {
				r.Parts[i].Text += " (changed)"
				changed = true
			}
		}
	}
	if !changed {
		t.Fatal("no text to change")
	}
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &h)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recs {
		if err := w.Write(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}
