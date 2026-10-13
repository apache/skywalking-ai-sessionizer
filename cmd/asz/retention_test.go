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
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
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
