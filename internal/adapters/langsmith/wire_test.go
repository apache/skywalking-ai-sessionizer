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

package langsmith

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// corpus is the demo application's captures: what one real client sent.
const corpus = "../../../tests/apps/langchain/testdata"

type capturedMeta struct {
	Headers map[string]string `json:"headers"`
	Parts   []struct {
		Name  string `json:"name"`
		Bytes int    `json:"bytes"`
	} `json:"parts"`
}

// captured walks the corpus, yielding each request's meta and body.
func captured(t *testing.T, fn func(kase, request string, meta capturedMeta, body []byte)) {
	t.Helper()
	cases, err := os.ReadDir(corpus)
	if err != nil {
		t.Skipf("no corpus at %s: %v", corpus, err)
	}
	seen := 0
	for _, c := range cases {
		if !c.IsDir() {
			continue
		}
		requests, err := os.ReadDir(filepath.Join(corpus, c.Name()))
		if err != nil {
			t.Fatalf("%s: %v", c.Name(), err)
		}
		for _, r := range requests {
			dir := filepath.Join(corpus, c.Name(), r.Name())
			raw, err := os.ReadFile(filepath.Join(dir, "meta.json"))
			if err != nil {
				continue
			}
			var meta capturedMeta
			if err := json.Unmarshal(raw, &meta); err != nil {
				t.Fatalf("%s: meta: %v", dir, err)
			}
			body, err := os.ReadFile(filepath.Join(dir, "body.bin"))
			if err != nil {
				t.Fatalf("%s: body: %v", dir, err)
			}
			seen++
			fn(c.Name(), r.Name(), meta, body)
		}
	}
	if seen == 0 {
		t.Fatal("the corpus holds no requests")
	}
}

// TestParsesWhatTheClientSent reads every captured request with the parser the
// receiver uses. A part this cannot name is a client that changed, and the
// corpus is the only place that shows up before a deployment does.
func TestParsesWhatTheClientSent(t *testing.T) {
	operations, requests := 0, 0
	captured(t, func(kase, request string, meta capturedMeta, body []byte) {
		got, err := ParseMultipart(bytes.NewReader(body), meta.Headers["Content-Type"])
		if err != nil {
			t.Fatalf("%s/%s: %v", kase, request, err)
		}
		// Every part of the request is accounted for: one envelope or one
		// field each, plus whatever was accepted and not landed.
		parts := 0
		for _, op := range got.Operations {
			parts += 1 + len(op.Fields)
			if len(op.Envelope) == 0 {
				t.Fatalf("%s/%s: %s has no envelope", kase, request, op.RunID)
			}
		}
		parts += got.Feedback + got.Attachments
		if parts != len(meta.Parts) {
			t.Fatalf("%s/%s: read %d parts, the request carried %d",
				kase, request, parts, len(meta.Parts))
		}
		operations += len(got.Operations)
		requests++
	})
	t.Logf("%d requests, %d operations", requests, operations)
}

// run is what a test needs from a run's envelope.
type run struct {
	ID      string `json:"id"`
	Trace   string `json:"trace_id"`
	Parent  string `json:"parent_run_id"`
	Dotted  string `json:"dotted_order"`
	Type    string `json:"run_type"`
	Name    string `json:"name"`
	Start   string `json:"start_time"`
	End     string `json:"end_time"`
	Session string `json:"session_name"`
}

func operationsOf(t *testing.T, kase string) []Operation {
	t.Helper()
	var all []Operation
	captured(t, func(c, request string, meta capturedMeta, body []byte) {
		if c != kase {
			return
		}
		got, err := ParseMultipart(bytes.NewReader(body), meta.Headers["Content-Type"])
		if err != nil {
			t.Fatalf("%s/%s: %v", c, request, err)
		}
		all = append(all, got.Operations...)
	})
	return all
}

func decode(t *testing.T, op Operation) run {
	t.Helper()
	var r run
	if err := json.Unmarshal(op.Envelope, &r); err != nil {
		t.Fatalf("%s: envelope: %v", op.RunID, err)
	}
	return r
}

// TestRunsArriveUnfinished holds the fact the whole update contract rests on.
// If a client ever stopped posting work in progress, the contract would be
// carrying weight nothing needs, and this is where that shows.
func TestRunsArriveUnfinished(t *testing.T) {
	for _, kase := range []string{"slow-tool", "abandoned-run"} {
		open, patches := 0, 0
		for _, op := range operationsOf(t, kase) {
			if op.Op == "patch" {
				patches++
				continue
			}
			if decode(t, op).End == "" {
				open++
			}
		}
		if open == 0 {
			t.Errorf("%s: no run arrived unfinished", kase)
		}
		t.Logf("%s: %d unfinished on arrival, %d completed later", kase, open, patches)
	}
	// A process that died mid-turn completes nothing: the open runs are all
	// the evidence there will ever be, and the receiver must not wait for
	// more.
	for _, op := range operationsOf(t, "abandoned-run") {
		if op.Op == "patch" {
			t.Fatal("abandoned-run: a run was completed; the case no longer shows a crash")
		}
	}
}

// TestThreadKeysAreNotPaths keeps the identity contract honest. These are not
// hypothetical: they are what the corpus carries, because an application
// chooses its own thread key and nothing stops it choosing these.
func TestThreadKeysAreNotPaths(t *testing.T) {
	var keys []string
	for _, op := range operationsOf(t, "unsafe-thread-key") {
		var extra struct {
			Metadata map[string]any `json:"metadata"`
		}
		if raw, ok := op.Fields["extra"]; ok {
			_ = json.Unmarshal(raw, &extra)
		}
		if v, ok := extra.Metadata["thread_id"].(string); ok {
			keys = append(keys, v)
		}
	}
	sort.Strings(keys)
	want := map[string]bool{"../outside": false, "team/customer": false,
		"_hidden": false, "会话-1": false}
	for _, k := range keys {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, found := range want {
		if !found {
			t.Errorf("the corpus no longer carries the thread key %q", k)
		}
	}
}
