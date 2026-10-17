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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/kiro"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
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
	ref.retain(time.Nanosecond, 0)
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
	ref.retain(time.Nanosecond, 0)
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
	ref.retain(time.Nanosecond, 0)
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

// retiredRoot is a root whose growth conversation a pipeline collected and
// parsed, then retired, with nothing left of it but its marker. Its source
// is still there.
func retiredRoot(t *testing.T) (string, string, *refresher) {
	t.Helper()
	root, b := scenarioRoot(t, "growth.yaml")
	if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
		t.Fatal(err)
	}
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	stdoutOf(t, func() {
		if err := ref.pass(); err != nil {
			t.Fatal(err)
		}
	})
	zone := storage.NewZone(root)
	if err := os.MkdirAll(filepath.Join(root, storage.RetiredDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(zone.RetiredPath(b.Session), nil, 0o444); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{zone.SessionDir(b.Session), filepath.Join(root, "_conversations", b.Session)} {
		if err := storage.DeleteTree(dir, nil); err != nil {
			t.Fatal(err)
		}
	}
	return root, b.Session, ref
}

// A pass says once a process that it found a retired session in its sources
// and did not collect it, and the next pass does not say it again.
func TestAPassSaysARetiredSessionOnce(t *testing.T) {
	root, session, ref := retiredRoot(t)
	line := "  retired: " + session + ": found and not collected"
	var out string
	stdoutOf(t, func() {
		out = stderrOf(t, func() {
			if err := ref.pass(); err != nil {
				t.Fatal(err)
			}
		})
	})
	if n := strings.Count(out, line); n != 1 {
		t.Fatalf("the pass said it %d times, want once:\n%s", n, out)
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
	if _, err := os.Stat(storage.NewZone(root).SessionDir(session)); !os.IsNotExist(err) {
		t.Fatalf("the pass made the retired session's directory again: %v", err)
	}
}

// asz verify says how many sessions are retired, asz sources names a source
// session that is retired, and asz repack refuses a root that holds one,
// whichever session it is asked for, before it makes anything.
func TestCommandsSayWhatIsRetired(t *testing.T) {
	root, session, _ := retiredRoot(t)
	saved := positional
	t.Cleanup(func() { positional = saved })
	cfg := config.Default()
	cfg.Storage.Root = root

	positional = nil
	out := stdoutOf(t, func() {
		if err := cmdVerify(cfg, config.Adapter{}, false); err != nil {
			t.Fatalf("verify: %v", err)
		}
	})
	if !strings.Contains(out, "1 session(s) are retired, and none is collected again") {
		t.Errorf("verify does not say the session is retired:\n%s", out)
	}

	ad := config.Adapter{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: filepath.Join(root, "_source")}
	out = stdoutOf(t, func() {
		if err := cmdSources(cfg, ad, false); err != nil {
			t.Fatalf("sources: %v", err)
		}
	})
	if !strings.Contains(out, "1 session(s) are retired in "+root+" and are not collected again:\n  "+session) {
		t.Errorf("sources does not name the retired session:\n%s", out)
	}

	for _, want := range []string{"", "another-session"} {
		dest := filepath.Join(t.TempDir(), "dest")
		positional = []string{dest}
		if want != "" {
			positional = append(positional, want)
		}
		var err error
		stdoutOf(t, func() { err = cmdRepack(cfg, config.Adapter{}, false) })
		if err == nil || !strings.Contains(err.Error(), "retired session") {
			t.Errorf("a repack of a root holding a retired session, asked for %q, returned %v", want, err)
		}
		if pathExists(dest) {
			t.Errorf("a refused repack, asked for %q, made its destination", want)
		}
	}
}

// Each adapter's asz sources names a session of its source that is retired.
func TestEverySourcesListNamesARetiredSession(t *testing.T) {
	saved := positional
	t.Cleanup(func() { positional = saved })
	kiroSource := filepath.Join("..", "..", "internal", "adapters", "kiro", "testdata")
	kiroSessions, _, _, err := kiro.Discover(kiroSource)
	if err != nil || len(kiroSessions) == 0 {
		t.Fatalf("%v: no Kiro fixture to list", err)
	}
	for _, c := range []struct {
		name     string
		scenario string
		adapter  func(root string) config.Adapter
		session  func(b string) string
	}{
		{"changes", "all-kinds.yaml", func(root string) config.Adapter {
			return config.Adapter{Name: config.AdapterChanges, Enabled: true, SourceRoot: filepath.Join(root, "_source", "plugins", "data")}
		}, func(b string) string { return b }},
		{"provider", "provider-bodies-long.yaml", func(root string) config.Adapter {
			return config.Adapter{Name: config.AdapterClaudeCodeProvider, Enabled: true, SourceRoot: filepath.Join(root, "_source", scenario.ProviderBodyDir)}
		}, func(b string) string { return b }},
		{"kiro", "", func(string) config.Adapter {
			return config.Adapter{Name: config.AdapterKiroLocal, Enabled: true, SourceRoot: kiroSource}
		}, func(string) string { return kiroSessions[0].ID }},
	} {
		t.Run(c.name, func(t *testing.T) {
			root, built := t.TempDir(), ""
			if c.scenario != "" {
				r, b := scenarioRoot(t, c.scenario)
				root, built = r, b.Session
			}
			session := c.session(built)
			zone := storage.NewZone(root)
			if err := os.MkdirAll(filepath.Join(root, storage.RetiredDir), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(zone.RetiredPath(session), nil, 0o444); err != nil {
				t.Fatal(err)
			}
			cfg := config.Default()
			cfg.Storage.Root = root
			out := stdoutOf(t, func() {
				if err := cmdSources(cfg, c.adapter(root), false); err != nil {
					t.Fatalf("sources: %v", err)
				}
			})
			if !strings.Contains(out, "are retired in "+root+" and are not collected again:\n  "+session) {
				t.Errorf("sources does not name the retired session %s:\n%s", session, out)
			}
		})
	}
}

// A pass retires a conversation idle for retire_after, after it sends, and
// its line counts it. The page stops listing it. The next pass retires
// nothing more, and its source, which is still there, is not collected
// again.
func TestAPassRetiresAnIdleConversation(t *testing.T) {
	root, b := scenarioRoot(t, "growth.yaml")
	if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
		t.Fatal(err)
	}
	rcv := receiver(t)
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	sendingTo(t, ref, rcv)
	ref.retain(time.Nanosecond, time.Nanosecond)
	out := stdoutOf(t, func() {
		if err := ref.pass(); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, " retired=1") {
		t.Fatalf("the pass line does not count the retirement:\n%s", out)
	}
	zone := storage.NewZone(root)
	if retired, err := storage.Retired(zone, b.Session); err != nil || !retired {
		t.Fatalf("no marker: %v", err)
	}
	if pathExists(zone.SessionDir(b.Session)) || pathExists(filepath.Join(root, "_conversations", b.Session)) {
		t.Fatal("a directory of the retired session is left")
	}
	if ids, err := ref.srv.List(); err != nil || len(ids) != 0 {
		t.Fatalf("the page lists %v, %v", ids, err)
	}
	stderrOf(t, func() {
		out = stdoutOf(t, func() {
			if err := ref.pass(); err != nil {
				t.Fatal(err)
			}
		})
	})
	if strings.Contains(out, " retired=") {
		t.Fatalf("the next pass retired something:\n%s", out)
	}
	if pathExists(zone.SessionDir(b.Session)) {
		t.Fatal("the next pass collected the retired session again")
	}
}

// A retired conversation may never be read again. The pass must drop it
// from the page's memory, whether retirement finishes in the pass or in
// recovery. Only the pass that finishes the retirement counts it.
func TestAPassForgetsARetiredConversation(t *testing.T) {
	for _, finish := range []string{"pass", "recovery"} {
		t.Run(finish, func(t *testing.T) {
			root, b := scenarioRoot(t, "growth.yaml")
			if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
				t.Fatal(err)
			}
			rcv := receiver(t)
			ref := pipelineOver(t, root, bothAdapters(root, ""))
			sendingTo(t, ref, rcv)
			stdoutOf(t, func() {
				if err := ref.pass(); err != nil {
					t.Fatal(err)
				}
			})
			if _, err := ref.srv.Load(b.Session); err != nil {
				t.Fatal(err)
			}
			if !ref.srv.Kept(b.Session) {
				t.Fatal("the page did not keep the conversation it loaded")
			}

			zone := storage.NewZone(root)
			chain := filepath.Join(root, "_conversations", b.Session)
			ref.retain(time.Nanosecond, time.Nanosecond)
			if finish == "recovery" {
				stopped := errors.New("stopped as a crash would stop it")
				ref.retention.Stop = func(p retention.Point) error {
					if p.Phase == retention.PhaseRetireMarked {
						return stopped
					}
					return nil
				}
				var err error
				var out string
				stderrOf(t, func() {
					out = stdoutOf(t, func() { err = ref.pass() })
				})
				if !errors.Is(err, stopped) {
					t.Fatalf("the retirement did not stop at its marker: %v", err)
				}
				if !strings.Contains(out, "] refreshed:") || strings.Contains(out, " retired=") {
					t.Fatalf("the stopped pass should print its line without counting a retirement:\n%s", out)
				}
				if !pathExists(zone.RetiredPath(b.Session)) || !pathExists(zone.SessionDir(b.Session)) || !pathExists(chain) {
					t.Fatal("the stopped retirement should leave its marker, session and chain")
				}
				if _, err := ref.srv.Load(b.Session); err != nil {
					t.Fatal(err)
				}
				if !ref.srv.Kept(b.Session) {
					t.Fatal("the page did not keep the conversation before recovery")
				}
				ref.retention.Stop = nil
			}

			var out string
			stderrOf(t, func() {
				out = stdoutOf(t, func() {
					if err := ref.pass(); err != nil {
						t.Fatal(err)
					}
				})
			})
			if ref.srv.Kept(b.Session) {
				t.Error("the page still keeps the retired conversation without another read")
			}
			if !strings.Contains(out, "] refreshed:") || !strings.Contains(out, " retired=1 ") {
				t.Errorf("the pass line does not count the finished retirement:\n%s", out)
			}
			if !pathExists(zone.RetiredPath(b.Session)) || pathExists(zone.SessionDir(b.Session)) || pathExists(chain) {
				t.Error("the retirement should leave its marker and remove the session and chain")
			}
		})
	}
}

// The pipeline says at startup what each level does. Equal levels go
// straight to retirement, so level 1 is not said then.
func TestThePipelineSaysWhatEachLevelDoes(t *testing.T) {
	root, _ := scenarioRoot(t, "growth.yaml")
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	cfg := config.Default()
	const metadataOnly, retired = "keeps only its metadata", "is retired: everything of it is removed, and it is never collected again"
	for _, c := range []struct {
		metadataOnly, retire time.Duration
		says, not            []string
	}{
		{time.Hour, 0, []string{"idle for 1h0m0s " + metadataOnly}, []string{retired}},
		{time.Hour, 2 * time.Hour, []string{"idle for 1h0m0s " + metadataOnly, "idle for 2h0m0s " + retired}, nil},
		{2 * time.Hour, 2 * time.Hour, []string{"idle for 2h0m0s " + retired}, []string{metadataOnly}},
		{0, 0, nil, []string{metadataOnly, retired}},
	} {
		ref.retain(c.metadataOnly, c.retire)
		out := stdoutOf(t, func() { printPipeline(ref, cfg) })
		for _, want := range c.says {
			if !strings.Contains(out, want) {
				t.Errorf("%v and %v: the startup does not say %q:\n%s", c.metadataOnly, c.retire, want, out)
			}
		}
		for _, not := range c.not {
			if strings.Contains(out, not) {
				t.Errorf("%v and %v: the startup says %q:\n%s", c.metadataOnly, c.retire, not, out)
			}
		}
	}
}

// asz parse and asz push finish a retirement a pass left unfinished before
// they do anything else, whatever the configuration says now, and write
// nothing of the session: no round, no index, nothing sent.
func TestStandaloneCommandsFinishARetirementFirst(t *testing.T) {
	for _, command := range []string{"parse", "push"} {
		t.Run(command, func(t *testing.T) {
			root, b := scenarioRoot(t, "growth.yaml")
			if err := os.RemoveAll(filepath.Join(root, storage.ScenarioDir)); err != nil {
				t.Fatal(err)
			}
			rcv := receiver(t)
			ref := pipelineOver(t, root, bothAdapters(root, ""))
			sendingTo(t, ref, rcv)
			ref.retain(time.Nanosecond, time.Nanosecond)
			ref.retention.Stop = func(p retention.Point) error {
				if p.Phase == retention.PhaseRetireMarked {
					return errors.New("stopped as a crash would stop it")
				}
				return nil
			}
			stderrOf(t, func() {
				stdoutOf(t, func() { _ = ref.pass() })
			})
			zone := storage.NewZone(root)
			if !pathExists(zone.SessionDir(b.Session)) || !pathExists(zone.RetiredPath(b.Session)) {
				t.Fatal("the stopped retirement should leave its marker and the session's directory")
			}
			rcv.Reset()
			cfg := &config.Config{Metrics: allMetrics}
			cfg.Storage.Root = root
			o := rcv.Options(otlp.ProtocolGRPC)
			cfg.Export.OTLP.Protocol, cfg.Export.OTLP.Endpoint, cfg.Export.OTLP.ServiceName = o.Protocol, o.Endpoint, "Removal Test"
			var err error
			stderrOf(t, func() {
				stdoutOf(t, func() {
					if command == "parse" {
						err = cmdParse(cfg, config.Adapter{}, false)
					} else {
						err = cmdPush(cfg, config.Adapter{}, false)
					}
				})
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, dir := range []string{zone.SessionDir(b.Session), filepath.Join(root, "_conversations", b.Session)} {
				if pathExists(dir) {
					t.Fatalf("%s is left after asz %s", dir, command)
				}
			}
			for _, rec := range otlptest.Records(rcv.Requests()) {
				if otlptest.Attrs(rec.Attributes)["asz.session"] == b.Session {
					t.Fatalf("asz %s sent a file of the retired session", command)
				}
			}
		})
	}
}
