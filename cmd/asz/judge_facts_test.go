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

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// A pass records, for the judge policy, each session's collection outcome
// and the head its parse reached: a transcript ending in a line still being
// written makes the session incomplete, and once the line is finished the
// next pass records it complete at the head that line moved it to.
func TestAPassRecordsWhatTheJudgePolicyReads(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	source := filepath.Join(root, "_source")
	ads := []config.Adapter{{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: source,
		Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}}}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, noMetrics, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	_ = ref.pass()
	first := ref.judgeFacts()[b.Session]
	if first.Outcome != storage.OutcomeComplete || first.Round == 0 || first.Digest == "" || first.Landed == 0 {
		t.Fatalf("after a whole pass: %+v", first)
	}
	var transcript string
	_ = filepath.WalkDir(source, func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && filepath.Base(p) == b.Session+".jsonl" {
			transcript = p
		}
		return nil
	})
	if transcript == "" {
		t.Fatal("no transcript")
	}
	appendTo := func(text string) {
		f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
	appendTo(`{"type":"user","uuid":"u-late","message":{"role":"user","content":"one more"}`)
	_ = ref.pass()
	if f := ref.judgeFacts()[b.Session]; f.Outcome != storage.OutcomeIncomplete {
		t.Fatalf("with a line still being written: %+v", f)
	}
	appendTo("}\n")
	_ = ref.pass()
	last := ref.judgeFacts()[b.Session]
	if last.Outcome != storage.OutcomeComplete || last.Landed <= first.Landed || last.Round <= first.Round {
		t.Fatalf("once the line is finished: %+v, before %+v", last, first)
	}
}

// A session the changes or provider adapter read whose transcripts were not
// found this pass joins its transcripts' last outcome with theirs, and one
// that recovers counts as recovered.
func TestAFeedersOutcomeCountsWithoutItsTranscripts(t *testing.T) {
	r := &refresher{}
	pass := func(transcripts, provider *storage.Outcomes) storage.Outcome {
		t.Helper()
		r.noteOutcomes([]sourceOutcomes{
			{name: config.AdapterClaudeCodeLocal, family: familyClaudeCode, primary: true, o: transcripts},
			{name: config.AdapterClaudeCodeProvider, family: familyClaudeCode, o: provider},
		})
		return r.judgeFacts()["s1"].Outcome
	}
	seen, left, fine := storage.Outcomes{}, storage.Outcomes{}, storage.Outcomes{}
	seen.Saw("s1")
	left.Leave("s1")
	fine.Saw("s1")
	if o := pass(&seen, &storage.Outcomes{}); o != storage.OutcomeComplete {
		t.Fatalf("read whole: %s", o)
	}
	if o := pass(nil, &left); o != storage.OutcomeIncomplete {
		t.Fatalf("a provider conflict with the transcripts gone: %s", o)
	}
	if o := pass(nil, &fine); o != storage.OutcomeComplete {
		t.Fatalf("once the provider recovered: %s", o)
	}
	if o := pass(nil, &storage.Outcomes{Unattributed: true}); o != storage.OutcomeUnknown {
		t.Fatalf("a failure that names no session: %s", o)
	}
	if o := pass(nil, &storage.Outcomes{}); o != storage.OutcomeComplete {
		t.Fatalf("the pass after that failure: %s", o)
	}
}

// Another process can land a session's files and stop before it parses
// them. Nothing local moves, so the pass parses the session again because
// its newest landed file is not the one its facts reached.
func TestAPassParsesWhatAnotherWriterLanded(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	source := filepath.Join(root, "_source")
	ads := []config.Adapter{{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: source,
		Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}}}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, noMetrics, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	ref.policy = &judge.Policy{}
	_ = ref.pass()
	before := ref.judgeFacts()[b.Session]
	var transcript string
	_ = filepath.WalkDir(source, func(p string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() && filepath.Base(p) == b.Session+".jsonl" {
			transcript = p
		}
		return nil
	})
	f, err := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"user","uuid":"u-late","message":{"role":"user","content":"one more"}}` + "\n")
	_ = f.Close()
	// The other writer lands the line and stops before it parses.
	if _, err := claudecode.New(source, zone, 1<<20).CollectAll(nil); err != nil {
		t.Fatal(err)
	}
	_ = ref.pass()
	after := ref.judgeFacts()[b.Session]
	if _, landed, err := storage.Collected(zone, b.Session); err != nil || after.Landed != landed || after.Round <= before.Round {
		t.Fatalf("facts %+v, before %+v, landed %d %v", after, before, landed, err)
	}
}
