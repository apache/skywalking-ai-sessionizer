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
	"path/filepath"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	collmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp/otlptest"
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
	// rather than exit clean with data unparsed.
	final := testRefresher(t, zone, root)
	final.last = true
	if err := final.pass(); err == nil {
		t.Fatal("a single pass exited clean while another builder held a session it had landed for")
	}

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
