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
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// removedRoot is a root whose growth conversation was collected, sent and
// removed by a pipeline pass with a retention of a moment. It is a plain
// root: a scenario build's own removal does not run there.
func removedRoot(t *testing.T) (string, string, *refresher) {
	t.Helper()
	root, b := scenarioRoot(t, "growth.yaml")
	if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
		t.Fatal(err)
	}
	rcv := receiver(t)
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	sendingTo(t, ref, rcv)
	ref.retain(time.Nanosecond)
	out := stdoutOf(t, func() {
		if err := ref.pass(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, " retention=") {
		t.Fatalf("the pass line does not count what retention removed:\n%s", out)
	}
	return root, b.Session, ref
}

func skeletonsOf(t *testing.T, root, session string) (skeletons, landed int) {
	t.Helper()
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		t.Fatal(err)
	}
	for _, lf := range files {
		if lf.Skeleton {
			skeletons++
		} else {
			landed++
		}
	}
	return skeletons, landed
}

// A pass removes what was sent and is idle for longer than the retention,
// after it sends. The page still lists the conversation and draws it, and
// the next pass sends nothing again and has nothing more to remove.
func TestAPassRemovesIdleSentConversations(t *testing.T) {
	root, session, ref := removedRoot(t)
	if sk, landed := skeletonsOf(t, root, session); sk == 0 || landed != 0 {
		t.Fatalf("%d skeletons and %d landed files after the pass", sk, landed)
	}
	ids, err := ref.srv.List()
	if err != nil {
		t.Fatal(err)
	}
	listed := false
	for _, id := range ids {
		listed = listed || id == session
	}
	if !listed {
		t.Fatal("the page stopped listing a conversation whose files were removed")
	}
	if err := ref.pass(); err != nil {
		t.Fatalf("the pass after a removal: %v", err)
	}
	if sk, landed := skeletonsOf(t, root, session); sk == 0 || landed != 0 {
		t.Fatalf("%d skeletons and %d landed files after the next pass", sk, landed)
	}
}

// A scenario root has its own removal, and retention never runs over it,
// even for a session the scenario's removal keeps.
func TestRetentionLeavesAScenarioRootAlone(t *testing.T) {
	root, b := scenarioRoot(t, "growth.yaml")
	rcv := receiver(t)
	// The changes adapter reads elsewhere, so the scenario's removal keeps
	// the session, and the pass sends it.
	ref := pipelineOver(t, root, bothAdapters(root, t.TempDir()))
	sendingTo(t, ref, rcv)
	ref.retain(time.Nanosecond)
	stderrOf(t, func() {
		if err := ref.pass(); err != nil {
			t.Fatal(err)
		}
	})
	if len(rcv.Requests()) == 0 {
		t.Fatal("the pass sent nothing")
	}
	if sk, landed := skeletonsOf(t, root, b.Session); sk != 0 || landed == 0 {
		t.Fatalf("retention ran over a scenario root: %d skeletons, %d landed files", sk, landed)
	}
}

// asz repack refuses a session retention removed files of, and one with a
// removal in progress, before it writes anything.
func TestRepackRefusesARemovedRoot(t *testing.T) {
	root, session, _ := removedRoot(t)
	saved := positional
	t.Cleanup(func() { positional = saved })
	cfg := config.Default()
	cfg.Storage.Root = root
	dest := filepath.Join(t.TempDir(), "dest")
	positional = []string{dest}
	var err error
	stdoutOf(t, func() { err = cmdRepack(cfg, config.Adapter{}, false) })
	if err == nil || !strings.Contains(err.Error(), "retention removed files") {
		t.Fatalf("a repack of a removed root returned %v", err)
	}
	if pathExists(dest) {
		t.Fatal("a refused repack made its destination")
	}

	zone := storage.NewZone(root)
	if err := storage.SaveRemoving(zone.RemovingPath(session), &storage.Removing{Generation: 2, Through: 1, Phase: storage.RemovingDelete}); err != nil {
		t.Fatal(err)
	}
	stdoutOf(t, func() { err = cmdRepack(cfg, config.Adapter{}, false) })
	if err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("a repack during a removal returned %v", err)
	}
}

// asz show says a record's content was removed, and what the receiver
// confirmed; asz verify reports the removed files and does not count them
// as verified.
func TestShowAndVerifySayWhatWasRemoved(t *testing.T) {
	root, session, _ := removedRoot(t)
	saved := positional
	t.Cleanup(func() { positional = saved })
	cfg := config.Default()
	cfg.Storage.Root = root

	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		t.Fatal(err)
	}
	f, err := sessiondata.Open(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	_, recs, err := sessiondata.All(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, r := range recs {
		if r.ID != "" {
			id = r.ID
			break
		}
	}
	if id == "" {
		t.Fatal("no record of the first file has an id")
	}
	positional = []string{session, id}
	out := stdoutOf(t, func() {
		if err := cmdShow(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{"removed: ", "by retention", "the receiver accepted the file", ".sk", "removed, "} {
		if !strings.Contains(out, want) {
			t.Errorf("show output lacks %q:\n%s", want, out)
		}
	}

	positional = nil
	out = stdoutOf(t, func() {
		if err := cmdVerify(cfg, config.Adapter{}, false); err != nil {
			t.Fatalf("verify over a removed root: %v", err)
		}
	})
	if !strings.Contains(out, "were removed by retention and are not verified: the receiver accepted") {
		t.Fatalf("verify does not report the removed files:\n%s", out)
	}
}

// TestUnreadObservationsAreSaidBySession. What a deriver could not derive
// is said once a session, in session order, and nothing when it is empty.
func TestUnreadObservationsAreSaidBySession(t *testing.T) {
	out := stderrOf(t, func() { sayUnread(map[string]int{"s-b": 2, "s-a": 1}) })
	want := "  metrics: s-a: 1 execution observations lack retained details; their MCP metrics cannot be derived\n" +
		"  metrics: s-b: 2 execution observations lack retained details; their MCP metrics cannot be derived\n"
	if out != want {
		t.Errorf("said %q, want %q", out, want)
	}
	if out := stderrOf(t, func() { sayUnread(nil) }); out != "" {
		t.Errorf("said %q for nothing unread", out)
	}
}

// unsentRemovedRoot is a root whose conversation a pipeline with no endpoint
// collected and then kept only the metadata of. Nothing was sent, so no
// metrics were derived either.
func unsentRemovedRoot(t *testing.T, scenarioName string) (string, string) {
	t.Helper()
	root, b := scenarioRoot(t, scenarioName)
	if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	ref, err := newRefresher(view.New(zone, nil), zone, bothAdapters(root, ""), pipelineMetrics(&config.Config{Metrics: allMetrics}), 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	ref.retain(time.Nanosecond)
	stdoutOf(t, func() {
		if err := ref.pass(); err != nil {
			t.Fatal(err)
		}
	})
	if sk, landed := skeletonsOf(t, root, b.Session); sk == 0 || landed != 0 {
		t.Fatalf("a pipeline with no endpoint kept %d landed files and wrote %d skeletons", landed, sk)
	}
	return root, b.Session
}

// A pipeline with no endpoint keeps only the metadata of an idle
// conversation it never sent. asz show says no receiver confirmed the file,
// and asz verify counts it among the removed.
func TestAPipelineWithNoEndpointKeepsOnlyMetadata(t *testing.T) {
	root, session := unsentRemovedRoot(t, "growth.yaml")
	saved := positional
	t.Cleanup(func() { positional = saved })
	cfg := config.Default()
	cfg.Storage.Root = root
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		t.Fatal(err)
	}
	f, err := sessiondata.Open(files[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	_, recs, err := sessiondata.All(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	id := ""
	for _, r := range recs {
		if r.ID != "" {
			id = r.ID
			break
		}
	}
	positional = []string{session, id}
	out := stdoutOf(t, func() {
		if err := cmdShow(cfg, config.Adapter{}, false); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "no receiver confirmed the file") {
		t.Errorf("show output does not say no receiver confirmed the file:\n%s", out)
	}
	positional = nil
	out = stdoutOf(t, func() {
		if err := cmdVerify(cfg, config.Adapter{}, false); err != nil {
			t.Fatalf("verify over a root with nothing sent: %v", err)
		}
	})
	if !strings.Contains(out, "accepted 0, accepted 0 with rejected records, and confirmed nothing of") {
		t.Fatalf("verify does not report the removed files as confirmed by no receiver:\n%s", out)
	}
}

// Metrics turned on after a conversation kept only its metadata: the first
// pass of a pipeline says how many of its execution observations their
// metrics cannot be derived for, and the pass after it does not. asz push,
// a process of its own, says it again.
func TestLateMetricsAreSaidByThePipelineAndByPush(t *testing.T) {
	root, session := unsentRemovedRoot(t, "mcp-calls.yaml")
	line := "  metrics: " + session + ": "
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	var out string
	stdoutOf(t, func() {
		out = stderrOf(t, func() {
			if err := ref.pass(); err != nil {
				t.Fatal(err)
			}
		})
	})
	if !strings.Contains(out, line) || !strings.Contains(out, " execution observations lack retained details; their MCP metrics cannot be derived") {
		t.Fatalf("the first pass with metrics on said:\n%s", out)
	}
	stdoutOf(t, func() {
		out = stderrOf(t, func() {
			if err := ref.pass(); err != nil {
				t.Fatal(err)
			}
		})
	})
	if strings.Contains(out, line) {
		t.Fatalf("the next pass said it again:\n%s", out)
	}

	rcv := receiver(t)
	cfg := &config.Config{Metrics: allMetrics}
	cfg.Storage.Root = root
	o := rcv.Options(otlp.ProtocolGRPC)
	cfg.Export.OTLP.Protocol, cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.ServiceName = o.Protocol, o.Endpoint, "Push Test"
	stdoutOf(t, func() {
		out = stderrOf(t, func() {
			if err := cmdPush(cfg, config.Adapter{}, false); err != nil {
				t.Fatal(err)
			}
		})
	})
	if !strings.Contains(out, line) {
		t.Fatalf("asz push did not say it:\n%s", out)
	}
}
