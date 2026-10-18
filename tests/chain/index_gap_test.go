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

package chain_test

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/index"
	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TestRecordsPastTheIndexAreParsedWhenTheTranscriptIsGone. A pass lands the
// next turn of a conversation and stops before it saves the index and the
// session's state, as a crash or a failed save leaves them. Claude Code then
// prunes the session's files, as it does by age, so no collector reads the
// session again. Parse brings the index up to the landed files itself: the
// chain reaches the newest landed file, the index is the one built from
// nothing, and a second pass lands nothing and writes no round. Retention,
// which waits for the chain to reach the newest landed file, then removes the
// session's files, or retires it.
func TestRecordsPastTheIndexAreParsedWhenTheTranscriptIsGone(t *testing.T) {
	r := newSentRoot(t)
	sc, err := scenario.Load(filepath.Join("..", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b := r.build(sc, scenario.Options{Through: sc.Checkpoints()[0]})
	r.pipelineAt(landedAt(0))
	session := b.Session
	kept := map[string][]byte{}
	for _, path := range []string{filepath.Join(r.zone.IndexDir(session), "entries.bin"),
		r.zone.IndexStatePath(session), r.zone.SessionStatePath(session)} {
		if kept[path], err = os.ReadFile(path); err != nil {
			t.Fatal(err)
		}
	}

	// The rest of the conversation lands, and the index, its state and the
	// session's state are put back as they were before the pass.
	r.build(sc, scenario.Options{})
	r.collectAt(landedAt(1))
	for path, data := range kept {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removed := 0
	err = filepath.WalkDir(r.source, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(path, session) {
			return err
		}
		removed++
		return os.Remove(path)
	})
	if err != nil || removed == 0 {
		t.Fatalf("removed %d source files of the session: %v", removed, err)
	}
	r.pipelineAt(landedAt(2))

	files, err := storage.LandedFiles(r.zone, session)
	if err != nil {
		t.Fatal(err)
	}
	newest := files[len(files)-1].Seq
	v, err := parse.View(r.dir, session)
	if err != nil {
		t.Fatal(err)
	}
	if v.ThroughSeq != newest {
		t.Fatalf("the chain reaches %d, the newest landed file is %d", v.ThroughSeq, newest)
	}
	extended, err := os.ReadFile(filepath.Join(r.zone.IndexDir(session), "entries.bin"))
	if err != nil {
		t.Fatal(err)
	}
	fresh := index.New(session)
	if _, err := index.Rebuild(r.zone, session, fresh, 0); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := fresh.Write(dir); err != nil {
		t.Fatal(err)
	}
	if built, err := os.ReadFile(filepath.Join(dir, "entries.bin")); err != nil || !bytes.Equal(built, extended) {
		t.Fatalf("the index parse extended is not the one built from nothing: %v", err)
	}
	if landed := r.collect(); landed != 0 {
		t.Fatalf("a second pass landed from %d sources", landed)
	}
	if written := r.parse(); written != 0 {
		t.Fatalf("a second parse with nothing new wrote %d rounds", written)
	}

	t.Run("metadata only", func(t *testing.T) {
		c := r.copyOf(t)
		res := retentionOf(c).Pass(retention.Input{})
		if len(res.Errors) > 0 || res.Files == 0 {
			t.Fatalf("the removal: %+v", res)
		}
		if landed, _ := sessionFiles(t, c.zone, session); landed != 0 {
			t.Fatalf("%d landed files of the session were kept", landed)
		}
	})
	t.Run("retired", func(t *testing.T) {
		c := r.copyOf(t)
		res := retirementOf(c).Pass(retention.Input{})
		if len(res.Errors) > 0 || len(res.Retired) != 1 || res.Retired[0] != session {
			t.Fatalf("the retirement: %+v", res)
		}
		retiredWhole(t, c.zone, session)
	})
}
