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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestOnePassLandsParsesAndSends is the pipeline asz collect and asz server
// run. One pass has to do all three: a landed file that is never parsed
// carries no structure, and a round that is never sent never leaves the
// machine. The push runs last, so the rounds a pass writes go out in that
// same pass rather than waiting for the next one.
func TestOnePassLandsParsesAndSends(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{
		At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}

	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer rcv.Close()
	client, err := rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	zone := storage.NewZone(root)
	source := filepath.Join(root, "_source")
	ads := []config.Adapter{
		{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: source,
			Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}},
		{Name: config.AdapterClaudeCodeChanges, Enabled: true, SourceRoot: filepath.Join(source, "plugins", "data"),
			Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}},
	}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	if ref == nil {
		t.Fatal("no refresher for a root that has a source beside it")
	}
	ref.pusher = &otlp.Pusher{
		Zone: zone, Client: client, Version: "test",
		ServiceName: "Pipeline Test", InstanceID: "pipeline-test", Layer: "AI_AGENT",
		BatchBytes: 8 << 20,
	}
	if err := ref.pusher.Prepare(); err != nil {
		t.Fatal(err)
	}

	ref.pass()

	// Landed and parsed: the conversation has a chain.
	rounds, err := filepath.Glob(filepath.Join(root, "_conversations", "*", "rounds", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) == 0 {
		t.Fatal("the pass landed data but wrote no round; collect must parse as well")
	}
	// Sent: the same pass put the landed files and the rounds on the wire.
	var files, sawRound int
	for _, req := range rcv.Requests() {
		for _, g := range req.GetResourceLogs() {
			for _, sl := range g.GetScopeLogs() {
				for _, rec := range sl.GetLogRecords() {
					files++
					if otlptest.Attrs(rec.GetAttributes())["asz.file.kind"] == "round" {
						sawRound++
					}
				}
			}
		}
	}
	if files == 0 {
		t.Fatal("the pass sent nothing; the push must run at the end of a pass")
	}
	if sawRound == 0 {
		t.Fatal("the pass sent no round; the push must run after the parse, not before it")
	}
}

// TestAPassWithNoReceiverStillLandsAndParses. The push is skipped when no
// endpoint is named, and nothing else about the pass changes.
func TestAPassWithNoReceiverStillLandsAndParses(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{
		At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	source := filepath.Join(root, "_source")
	ads := []config.Adapter{{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: source,
		Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}}}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	ref.pass() // ref.pusher is nil: no endpoint was named.
	rounds, err := filepath.Glob(filepath.Join(root, "_conversations", "*", "rounds", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) == 0 {
		t.Fatal("a pass with no receiver wrote no round")
	}
}

// TestAThrottledReceiverHoldsTheNextSend. A receiver that asks to be left
// alone for longer than one period gets that, and the pipeline keeps
// landing and parsing in the meantime. The old push loop honoured this and
// the pipeline has to as well, or a receiver under load is hammered every
// interval.
func TestAThrottledReceiverHoldsTheNextSend(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{
		At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer rcv.Close()
	client, err := rcv.Client(otlp.ProtocolHTTP)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	zone := storage.NewZone(root)
	ref := testRefresher(t, zone, root)
	ref.pusher = &otlp.Pusher{
		Zone: zone, Client: client, Version: "test",
		ServiceName: "Throttle Test", InstanceID: "throttle-test", Layer: "AI_AGENT",
		BatchBytes: 8 << 20,
	}
	if err := ref.pusher.Prepare(); err != nil {
		t.Fatal(err)
	}

	rcv.Throttle(true, time.Hour)
	_ = ref.pass()
	if ref.pushAfter.IsZero() {
		t.Fatal("a receiver that asked for an hour did not hold the next send")
	}
	held := ref.pushAfter

	// The next pass must not send. It must still land and parse.
	rcv.Throttle(false, 0)
	rcv.Reset()
	_ = ref.pass()
	if len(rcv.Requests()) != 0 {
		t.Fatalf("the pass sent %d request(s) while the receiver's hold was still running", len(rcv.Requests()))
	}
	if !ref.pushAfter.Equal(held) {
		t.Fatal("the hold moved without the receiver asking for it")
	}
}

// TestARejectedRecordIsReported. A receiver can take a request and drop
// records inside it. Those files are marked sent and never go again, so the
// pass is the only place it can be seen: it has to say so and fail.
func TestARejectedRecordIsReported(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{
		At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	ref := testRefresher(t, zone, root)
	ref.pusher = &otlp.Pusher{
		Zone: zone, Client: rejectingClient{}, Version: "test",
		ServiceName: "Reject Test", InstanceID: "reject-test", Layer: "AI_AGENT",
		BatchBytes: 8 << 20,
	}
	if err := ref.pusher.Prepare(); err != nil {
		t.Fatal(err)
	}
	err = ref.pass()
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("a pass whose records were rejected reported %v; it must say so", err)
	}
}

// rejectingClient takes every request and says it dropped one record.
type rejectingClient struct{}

func (rejectingClient) Export(*collogspb.ExportLogsServiceRequest) (int64, error) { return 1, nil }

func (rejectingClient) ExportMetrics(*collmetricspb.ExportMetricsServiceRequest) (int64, error) {
	return 0, nil
}
func (rejectingClient) Close() error { return nil }

// testRefresher builds a refresher over a scenario root, for one pass.
func testRefresher(t *testing.T, zone *storage.Zone, root string) *refresher {
	t.Helper()
	source := filepath.Join(root, "_source")
	ads := []config.Adapter{{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: source,
		Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}}}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	if ref == nil {
		t.Fatal("no refresher for a root that has a source beside it")
	}
	// A scenario root's lock is held for the life of a process. A test ends
	// long before its process does, and the next one may put another
	// refresher on the same root.
	t.Cleanup(ref.close)
	return ref
}

// TestTheModeIsSettledBeforeAnythingIsBuilt. One pipeline serves every
// adapter, so an adapter that asks to watch makes the whole pipeline watch.
// The mode has to be settled before the metrics derivation is built,
// because a single pass derives history differently from a watching one:
// built with the wrong mode, a call split across two passes is marked
// processed before its last fragment lands and its tokens are lost.
func TestTheModeIsSettledBeforeAnythingIsBuilt(t *testing.T) {
	root := t.TempDir()
	zone := storage.NewZone(root)
	// The metrics-enabled adapter says once and comes first; the second
	// says watch. The pipeline watches, and so must the derivation.
	ads := []config.Adapter{
		{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: filepath.Join(root, "_source"),
			Metrics: true, MetricsLookback: "none",
			Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Minute, MaxDeltaBytes: 1 << 20}},
		{Name: config.AdapterClaudeCodeChanges, Enabled: true, SourceRoot: filepath.Join(root, "_source", "plugins", "data"),
			Collector: config.Collector{Mode: config.ModeWatch, Interval: 2 * time.Second, MaxDeltaBytes: 1 << 20}},
	}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, 2<<20, false)
	if err != nil {
		t.Fatal(err)
	}
	if ref.base.Mode != config.ModeWatch {
		t.Fatalf("the pipeline mode is %q; one adapter asking to watch makes it watch", ref.base.Mode)
	}
	// The shortest period wins: a source polled every two seconds is still
	// polled every two seconds when another is content with a minute.
	if ref.interval != 2*time.Second {
		t.Fatalf("the pipeline period is %s, want the shortest asked for, 2s", ref.interval)
	}
	if ref.deriver == nil {
		t.Fatal("no metrics derivation for an adapter that asked for one")
	}
	// A single pass sets Grace to -1, never wait, because there is no later
	// pass to derive what it left. A watching pipeline leaves it at the
	// default, so a call split across two passes is waited for.
	if ref.deriver.Grace < 0 {
		t.Fatalf("the derivation was built as a single pass (grace %s); a watching pipeline waits for a split call", ref.deriver.Grace)
	}
}

// TestASourceThatAppearsLaterIsCollected. The plugin may be installed after
// the pipeline starts. A source directory that is absent at the start must
// not be dropped for the life of the process.
func TestASourceThatAppearsLaterIsCollected(t *testing.T) {
	root := t.TempDir()
	zone := storage.NewZone(root)
	changes := filepath.Join(root, "_source", "plugins", "data")
	ads := []config.Adapter{{Name: config.AdapterClaudeCodeChanges, Enabled: true, SourceRoot: changes,
		Collector: config.Collector{Mode: config.ModeWatch, Interval: time.Second, MaxDeltaBytes: 1 << 20}}}
	ref, err := newRefresher(view.New(zone, nil), zone, ads, 2<<20, false)
	if err != nil {
		t.Fatal(err)
	}
	if ref == nil {
		t.Fatal("an enabled adapter whose directory does not exist yet was dropped; it must be kept and looked at again")
	}
	if ref.changes == nil {
		t.Fatal("the changes collector was not built for a directory that does not exist yet")
	}
	// A pass over the absent directory is quiet, not an error.
	if err := ref.pass(); err != nil {
		t.Fatalf("a pass over a source that does not exist yet failed: %v", err)
	}
}

// TestAnotherBuilderHoldingTheChainIsNotAFailure.
//
// The new command model puts two writers on one root on purpose: asz server
// locally, and an asz collect from a cron or a second container. When one
// holds a conversation's chain the other cannot write that round, but
// nothing is wrong and nothing is lost, so the pass must not fail.
//
// It must also not forget. A later pass looks only at what moved, and a
// session whose source has stopped growing never moves again, so a session
// dropped while another builder held it would keep its landed data unparsed
// for good. It has to be carried until a pass gets the lock.
func TestAnotherBuilderHoldingTheChainIsNotAFailure(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// Part of the session first, so the source grows later rather than
	// being rewritten, which is what a live transcript does.
	b, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{At: at, Through: "turn1"})
	if err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	ref := testRefresher(t, zone, root)
	if err := ref.pass(); err != nil {
		t.Fatalf("the first pass failed: %v", err)
	}

	// Another builder takes the chain, as a second asz process would, and
	// the rest of the session lands while it holds it.
	lock, err := sessionflow.OpenChain(root, b.Session).Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{At: at}); err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatalf("a pass that could not take a chain another builder holds reported failure: %v", err)
	}
	if !ref.retry[b.Session] {
		t.Fatal("a session another builder held was dropped; nothing would bring it back, because a later pass looks only at what moved")
	}

	// A single pass has no next pass to carry it to, so it must say so
	// rather than exit clean with data unparsed. It is a second pipeline
	// over a scenario root, so the first lets go of the root before it runs,
	// or it would wait for the root and fail for that reason instead.
	ref.close()
	final := testRefresher(t, zone, root)
	final.last = true
	if err := final.pass(); err == nil {
		t.Fatal("a single pass exited clean while another builder held a session it had landed for")
	} else if strings.Contains(err.Error(), "scenario root") {
		t.Fatalf("the single pass failed on the scenario root, not on the chain: %v", err)
	}
	final.close()

	// The other builder is done. The next pass must pick the session up
	// again, even though its source has not grown since.
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatalf("the pass after the lock was released failed: %v", err)
	}
	if ref.retry[b.Session] {
		t.Fatal("the session is still waiting for a lock nobody holds")
	}
	rounds, err := filepath.Glob(filepath.Join(root, "_conversations", b.Session, "rounds", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) < 2 {
		t.Fatalf("%d round(s) after the lock was released; what landed during the contention was never parsed", len(rounds))
	}
}

// TestOnlyOnePassSendsAtATime.
//
// Two pipelines over one root is a supported setup, and both push. A push
// is a read-then-write over push.state: read what has gone, send what has
// not, record it. Without a lock both would read the same state and send
// the same records, and a token counted twice cannot be taken back.
func TestOnlyOnePassSendsAtATime(t *testing.T) {
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "assembly.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{
		At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer rcv.Close()
	client, err := rcv.Client(otlp.ProtocolGRPC)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	zone := storage.NewZone(root)
	ref := testRefresher(t, zone, root)
	ref.pusher = &otlp.Pusher{
		Zone: zone, Client: client, Version: "test",
		ServiceName: "Lock Test", InstanceID: "lock-test", Layer: "AI_AGENT",
		BatchBytes: 8 << 20,
	}
	if err := ref.pusher.Prepare(); err != nil {
		t.Fatal(err)
	}

	// Another pipeline over the same root is sending.
	held, err := storage.LockExport(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatalf("a pass that could not take the export lock reported failure: %v", err)
	}
	if n := len(rcv.Requests()); n != 0 {
		t.Fatalf("the pass sent %d request(s) while another pass held the export state", n)
	}

	// It finishes, and the next pass sends.
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	if len(rcv.Requests()) == 0 {
		t.Fatal("nothing was sent once the export state was free")
	}
}

// scenarioRoot builds one claude-code session into a fresh root. Claude
// Code's own directory is a place of the test's, so no guard depends on the
// machine the test runs on.
func scenarioRoot(t *testing.T, name string) (string, *scenario.Built) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	sc, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", name))
	if err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(sc, scenario.FormatClaudeCode, root, scenario.Options{At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return root, b
}

// bothAdapters is what a build's configuration enables: both local
// adapters, the plugin's reading under the build's source unless another
// directory is given, and metrics derived with no look-back.
func bothAdapters(root, changes string) []config.Adapter {
	source := filepath.Join(root, "_source")
	if changes == "" {
		changes = filepath.Join(source, "plugins", "data")
	}
	return []config.Adapter{
		{Name: config.AdapterClaudeCodeLocal, Enabled: true, SourceRoot: source, Metrics: true, MetricsLookback: "none",
			Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}},
		{Name: config.AdapterClaudeCodeChanges, Enabled: true, SourceRoot: changes,
			Collector: config.Collector{Mode: config.ModeOnce, Interval: time.Second, MaxDeltaBytes: 1 << 20}},
	}
}

// pipelineOver is the pipeline asz collect -once runs over a root.
func pipelineOver(t *testing.T, root string, ads []config.Adapter) *refresher {
	t.Helper()
	zone := storage.NewZone(root)
	ref, err := newRefresher(view.New(zone, nil), zone, ads, 2<<20, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ref.close)
	return ref
}

func receiver(t *testing.T) *otlptest.Receiver {
	t.Helper()
	rcv, err := otlptest.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rcv.Close)
	return rcv
}

// sendingTo gives the pipeline a pusher that records the receiver it sends
// to, as newPusher builds one.
func sendingTo(t *testing.T, ref *refresher, rcv *otlptest.Receiver) {
	t.Helper()
	o := rcv.Options(otlp.ProtocolGRPC)
	client, err := otlp.NewClient(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	ref.pusher = &otlp.Pusher{
		Zone: ref.zone, Client: client, Endpoint: otlp.EndpointOf(o.Protocol, o.Endpoint, o.TLS), Version: "test",
		ServiceName: "Removal Test", InstanceID: "removal-test", Layer: "AI_AGENT", BatchBytes: 8 << 20,
		MetricsService: "Removal Test",
	}
	if err := ref.pusher.Prepare(); err != nil {
		t.Fatal(err)
	}
}

// stderrOf runs fn and returns what it wrote to standard error, where a pass
// says why it kept a session.
func stderrOf(t *testing.T, fn func()) string {
	t.Helper()
	return written(t, &os.Stderr, fn)
}

// stdoutOf runs fn and returns what it wrote to standard output, where the
// pipeline says what it will do.
func stdoutOf(t *testing.T, fn func()) string {
	t.Helper()
	return written(t, &os.Stdout, fn)
}

// written runs fn with *stream sent into a pipe, and returns what fn wrote.
func written(t *testing.T, stream **os.File, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	out := make(chan string)
	go func() {
		data, _ := io.ReadAll(r)
		out <- string(data)
	}()
	old := *stream
	*stream = w
	func() {
		defer func() { *stream = old; _ = w.Close() }()
		fn()
	}()
	return <-out
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// TestOnePassSendsAndRemovesAScenarioSession. Over a root a claude-code
// build wrote, the pass that sends a session whole also removes it, and the
// page stops listing it. The next pass has nothing to send or remove.
func TestOnePassSendsAndRemovesAScenarioSession(t *testing.T) {
	root, b := scenarioRoot(t, "workspace-changes.yaml")
	rcv := receiver(t)
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	sendingTo(t, ref, rcv)
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	if len(rcv.Requests()) == 0 {
		t.Fatal("the pass sent nothing")
	}
	for _, p := range []string{b.Marker, filepath.Join(root, b.Session), filepath.Join(root, "_conversations", b.Session)} {
		if pathExists(p) {
			t.Errorf("%s is still there after the pass that sent all of it", p)
		}
	}
	ids, err := ref.srv.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if id == b.Session {
			t.Fatal("the page still lists the removed conversation")
		}
	}
	rcv.Reset()
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	if n := len(rcv.Requests()) + len(rcv.MetricsRequests()); n != 0 {
		t.Fatalf("the next pass sent %d request(s)", n)
	}
	if pathExists(filepath.Join(root, b.Session)) {
		t.Fatal("the next pass made the removed session again")
	}
}

// TestAScenarioSessionIsKeptWithoutAnEndpoint. With nothing sent, nothing
// can be proved sent, so nothing is removed. The pass says why, once, and it
// is not a failure: the product has no removal setting that could be wrong.
func TestAScenarioSessionIsKeptWithoutAnEndpoint(t *testing.T) {
	root, b := scenarioRoot(t, "workspace-changes.yaml")
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	var err error
	said := stderrOf(t, func() { err = ref.pass() })
	if err != nil {
		t.Fatalf("a pass with no endpoint failed: %v", err)
	}
	if !strings.Contains(said, "kept: every marked session: no export endpoint") {
		t.Fatalf("the pass did not say why the session is kept:\n%s", said)
	}
	if !pathExists(b.Marker) || !pathExists(filepath.Join(root, b.Session)) {
		t.Fatal("a session was removed with no endpoint to send it to")
	}
	if said := stderrOf(t, func() { _ = ref.pass() }); strings.Contains(said, "kept:") {
		t.Fatalf("the reason was said a second time:\n%s", said)
	}
}

// TestAScenarioSessionIsKeptWhenTheChangesAdapterReadsElsewhere. The build
// wrote plugin output under its own source. A changes adapter that reads any
// other directory never lands it, so it is never sent, and the session stays.
func TestAScenarioSessionIsKeptWhenTheChangesAdapterReadsElsewhere(t *testing.T) {
	root, b := scenarioRoot(t, "workspace-changes.yaml")
	rcv := receiver(t)
	ref := pipelineOver(t, root, bothAdapters(root, t.TempDir()))
	sendingTo(t, ref, rcv)
	var err error
	said := stderrOf(t, func() { err = ref.pass() })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(said, "kept: every marked session: the changes adapter reads") {
		t.Fatalf("the pass did not say why the session is kept:\n%s", said)
	}
	if !pathExists(b.Marker) || !pathExists(filepath.Join(root, b.Session)) {
		t.Fatal("a session was removed while its plugin output could never be landed")
	}
}

// TestTheScenarioLineSaysWhyNothingIsRemoved. At the start, over a scenario
// root, the pipeline says what the removal will do. It asks the check each
// removal pass makes before it looks at any session, so it never promises a
// removal the configuration prevents. The line printed when a pass takes the
// lock says the same.
func TestTheScenarioLineSaysWhyNothingIsRemoved(t *testing.T) {
	root, _ := scenarioRoot(t, "workspace-changes.yaml")
	rcv := receiver(t)
	cfg := &config.Config{}
	cfg.Export.OTLP.Endpoint = rcv.Options(otlp.ProtocolGRPC).Endpoint
	for _, c := range []struct {
		name string
		ads  []config.Adapter
		send bool
		// metricsOff and logsOff are what export.otlp.metrics: false and
		// export.otlp.logs: false do to the pusher.
		metricsOff bool
		logsOff    bool
		want       string
	}{
		{name: "no endpoint", ads: bothAdapters(root, ""),
			want: "scenario : no session is removed: no export endpoint; nothing is sent, so nothing is removed\n"},
		{name: "metrics off", ads: bothAdapters(root, ""), send: true, metricsOff: true,
			want: "scenario : no session is removed: export.otlp.metrics is off;"},
		{name: "logs off", ads: bothAdapters(root, ""), send: true, logsOff: true,
			want: "scenario : no session is removed: export.otlp.logs is off;"},
		{name: "the changes adapter elsewhere", ads: bothAdapters(root, t.TempDir()), send: true,
			want: "scenario : no session is removed: the changes adapter reads "},
		{name: "no claude-code-local", ads: bothAdapters(root, "")[1:], send: true,
			want: "scenario : no session is removed: claude-code-local is not enabled"},
		{name: "nothing prevents it", ads: bothAdapters(root, ""), send: true,
			want: "scenario : a session a build marked is removed by its policy once all of it is sent\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ref := pipelineOver(t, root, c.ads)
			if c.send {
				sendingTo(t, ref, rcv)
				ref.pusher.NoMetrics = c.metricsOff
				ref.pusher.NoLogs = c.logsOff
			}
			said := stdoutOf(t, func() { printPipeline(ref, cfg) })
			if !strings.Contains(said, c.want) {
				t.Fatalf("the start-up lines do not say %q:\n%s", c.want, said)
			}
		})
	}

	// A root no build wrote has no such line.
	plain := t.TempDir()
	said := stdoutOf(t, func() { printPipeline(pipelineOver(t, plain, bothAdapters(plain, "")), cfg) })
	if strings.Contains(said, "scenario :") {
		t.Fatalf("a root with no %s has a scenario line:\n%s", storage.ScenarioDir, said)
	}

	// The lock's line ends with the same words as the start-up line.
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	said = stdoutOf(t, func() { _ = stderrOf(t, func() { _ = ref.pass() }) })
	want := "scenario : this pipeline holds " + filepath.Join(root, storage.ScenarioDir, ".lock") +
		"; no session is removed: no export endpoint; nothing is sent, so nothing is removed\n"
	if !strings.Contains(said, want) {
		t.Fatalf("the lock's line is not %q:\n%s", want, said)
	}
}

// TestASecondPipelineWaitsForTheScenarioRoot. Two pipelines over one
// scenario root would race a removal: one could land again what the other
// just removed. So the second does nothing while the first holds the root.
func TestASecondPipelineWaitsForTheScenarioRoot(t *testing.T) {
	root, b := scenarioRoot(t, "assembly.yaml")
	held, err := storage.LockScenario(root)
	if err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	watching := testRefresher(t, zone, root)
	if err := watching.pass(); err != nil {
		t.Fatalf("a watching pass over a root another pipeline holds failed: %v", err)
	}
	if pathExists(filepath.Join(root, b.Session)) || pathExists(filepath.Join(root, "_conversations")) {
		t.Fatal("a pass over a root another pipeline holds landed or parsed")
	}
	single := testRefresher(t, zone, root)
	single.last, single.scenarioWait = true, 200*time.Millisecond
	if err := single.pass(); err == nil || !strings.Contains(err.Error(), "another pipeline holds this scenario root") {
		t.Fatalf("a single pass over a held root gave %v", err)
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	if err := watching.pass(); err != nil {
		t.Fatal(err)
	}
	rounds, err := filepath.Glob(filepath.Join(root, "_conversations", b.Session, "rounds", "*"))
	if err != nil || len(rounds) == 0 {
		t.Fatalf("once the root was released the pass wrote %d round(s): %v", len(rounds), err)
	}
}

// TestTheDeriverSeesTheChangesAdaptersSessions. After the first pass, the
// derivation takes only the sessions a pass moved. A session that only the
// changes adapter moved has to be among them. Otherwise its new files wait
// for a pass that moves nothing at all, which never comes while a feed runs.
func TestTheDeriverSeesTheChangesAdaptersSessions(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	root := t.TempDir()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	growth, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "growth.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	withChanges, err := scenario.Load(filepath.Join("..", "..", "tests", "scenarios", "workspace-changes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(growth, scenario.FormatClaudeCode, root, scenario.Options{At: at, Through: "turn1"}); err != nil {
		t.Fatal(err)
	}
	b, err := scenario.Build(withChanges, scenario.FormatClaudeCode, root, scenario.Options{At: at.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// The plugin's output is not there on the first pass.
	plugin := filepath.Join(root, "_source", filepath.FromSlash(scenario.PluginOutputDir), "..")
	hidden := filepath.Join(root, "_source", "plugins", "hidden")
	if err := os.Rename(plugin, hidden); err != nil {
		t.Fatal(err)
	}
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	// Now it is, and the local adapter moves only the other session.
	if err := os.Rename(hidden, plugin); err != nil {
		t.Fatal(err)
	}
	if _, err := scenario.Build(growth, scenario.FormatClaudeCode, root, scenario.Options{At: at}); err != nil {
		t.Fatal(err)
	}
	if err := ref.pass(); err != nil {
		t.Fatal(err)
	}
	zone := storage.NewZone(root)
	files, err := storage.LandedFiles(zone, b.Session)
	if err != nil {
		t.Fatal(err)
	}
	derived, err := metrics.LoadDerived(zone)
	if err != nil {
		t.Fatal(err)
	}
	saw := 0
	for _, lf := range files {
		if !strings.HasPrefix(filepath.Base(lf.Path), "changes-") {
			continue
		}
		saw++
		rel, _ := filepath.Rel(root, lf.Path)
		digest, err := storage.FileDigest(lf.Path)
		if err != nil {
			t.Fatal(err)
		}
		if !derived.Has(filepath.ToSlash(rel), digest) {
			t.Errorf("%s, which only the changes adapter landed, was not derived", rel)
		}
	}
	if saw == 0 {
		t.Fatal("the second pass landed no plugin output")
	}
}

// TestNoListingSeesRemoved. A removal renames a directory under _removed
// before it deletes it. Until it is deleted, nothing that lists sessions or
// chains may see it, or it would be parsed, sent or shown again.
func TestNoListingSeesRemoved(t *testing.T) {
	first, b := scenarioRoot(t, "assembly.yaml")
	if err := pipelineOver(t, first, bothAdapters(first, "")).pass(); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "_source", "plugins", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	removed := filepath.Join(root, storage.RemovedDir)
	if err := os.MkdirAll(removed, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(first, b.Session), filepath.Join(removed, b.Session)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(first, "_conversations", b.Session), filepath.Join(removed, b.Session+".chain")); err != nil {
		t.Fatal(err)
	}
	rcv := receiver(t)
	ref := pipelineOver(t, root, bothAdapters(root, ""))
	sendingTo(t, ref, rcv)
	if err := ref.pass(); err != nil {
		t.Fatalf("a pass over a root with a removal in progress failed: %v", err)
	}
	if rounds, _ := filepath.Glob(filepath.Join(root, "_conversations", "*", "rounds", "*")); len(rounds) != 0 {
		t.Fatalf("the pass wrote %d round(s) from what was removed", len(rounds))
	}
	if n := len(rcv.Requests()) + len(rcv.MetricsRequests()); n != 0 {
		t.Fatalf("the pass sent %d request(s) from what was removed", n)
	}
	if chains, _, _, err := verifyChains(root, ""); err != nil || chains != 0 {
		t.Fatalf("asz verify found %d chain(s): %v", chains, err)
	}
	if ids, err := view.New(storage.NewZone(root), nil).List(); err != nil || len(ids) != 0 {
		t.Fatalf("the page lists %v: %v", ids, err)
	}
}
