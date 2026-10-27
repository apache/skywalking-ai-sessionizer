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
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/pull/server"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// cmdView serves the conversations in the storage root, and changes no
// collected data.
//
// The chain, the landed records and the index are opened read-only, and
// nothing in this process collects, parses or sends. It writes a lease
// while it reads a pulled snapshot. When its root is a pulled snapshot a
// stopped pull left half published, it deletes that unfinished snapshot and
// records its tag removed. When it answers pulls, it keeps the pull server's
// records under _pull/. It is the web host: a
// root filled by asz collect on this machine, or copied from another one,
// or written by a receiver, is served the same way.
//
// To collect and serve in one process, run asz server.
func cmdView(cfg *config.Config, _ []config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	zone := storage.NewZone(zoneRoot)
	srv := view.NewWithGlossaries(zone, glossaries())
	if err := srv.SetHide(cfg.View.Hide, cfg.View.HideOverride); err != nil {
		return err
	}
	guard, err := protectionOf(cfg)
	if err != nil {
		return err
	}
	// A read of a pulled snapshot's evidence that fails stops the page:
	// nothing more is served over evidence known damaged.
	damaged := make(chan error, 1)
	snap, err := snapshotView(func(err error) {
		select {
		case damaged <- err:
		default:
		}
	})
	if err != nil {
		return err
	}
	if snap != nil {
		srv.SetSnapshot(snap)
	}
	ids, err := srv.List()
	if err != nil {
		return err
	}
	// A snapshot's page serves the snapshot alone. Any other root serves
	// the conversations asz pull copied into it beside its own.
	var pulled *pulledPage
	if snap == nil {
		if pulled, err = newPulledPage(cfg, zoneRoot); err != nil {
			return err
		}
	}
	if len(ids) == 0 && (pulled == nil || !pulled.any()) {
		return fmt.Errorf("no conversations in %s; run asz collect first, or asz server to do both", zoneRoot)
	}
	ln, err := serveAddr()
	if err != nil {
		return err
	}
	fmt.Printf("reading  : %s\nserving  : %d conversation(s)\n", zoneRoot, len(ids))
	printWithholding(cfg)
	fmt.Printf("\n   %s://%s\n\n", guard.scheme(), ln.Addr())
	fmt.Fprintln(os.Stderr, "ctrl-c to stop")
	guard.warn(ln)
	pulls, err := guard.pullServer(cfg, zoneRoot)
	if err != nil {
		return err
	}
	return servePage(ln, guard, srv, snap, damaged, pollInterval(cfg), pulls, pulled)
}

// servePage serves the page over a root, or over a pulled snapshot when snap
// is set.
//
// Nothing here writes, but something else may: an asz collect running beside
// this page fills the same root. Watching the chains costs a directory
// listing, and without it an open list page would never show a conversation
// that arrived after it was loaded. A pulled snapshot never changes, and is
// not watched.
//
// A snapshot's page serves no pull routes: they answer 404, tokens or not.
func servePage(ln net.Listener, p *protection, srv *view.Server, snap *view.Snapshot, damaged <-chan error, every time.Duration,
	pulls *server.Server, pulled *pulledPage) error {
	if snap != nil {
		return serveSnapshot(ln, p, p.handler(srv.Handler(), nil), damaged)
	}
	go watchRoot(srv, every, pulled)
	page := srv.Handler()
	if pulled != nil {
		page = pulled.handler(page)
	}
	if pulls != nil {
		return p.serveWithPulls(ln, page, pulls)
	}
	return p.serve(p.server(p.handler(page, nil)), ln)
}

// serveSnapshot serves a pulled snapshot's page until a read of its evidence
// fails or the command is interrupted. Either way it stops in one order: it
// stops taking requests, cancels those running through their contexts, and
// waits for every one to end. Only then does the command return, and its
// lease is released, so no request reads the snapshot without one.
func serveSnapshot(ln net.Listener, p *protection, h http.Handler, damaged <-chan error) error {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	gate := &requestGate{h: h}
	hs := p.server(gate)
	hs.BaseContext = func(net.Listener) context.Context { return base }
	served := make(chan error, 1)
	go func() { served <- p.serve(hs, ln) }()
	interrupted := make(chan os.Signal, 1)
	signal.Notify(interrupted, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(interrupted)
	var err error
	select {
	case err = <-damaged:
	case <-interrupted:
	case err = <-served:
	}
	gate.stop()
	cancel()
	err = errors.Join(err, hs.Shutdown(context.Background()))
	gate.wait()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// requestGate admits requests until it is stopped, and counts those it
// admitted, so a stop can wait for each of them.
type requestGate struct {
	h       http.Handler
	mu      sync.Mutex
	stopped bool
	running sync.WaitGroup
}

func (g *requestGate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	if g.stopped {
		g.mu.Unlock()
		http.Error(w, "the snapshot is no longer served", http.StatusServiceUnavailable)
		return
	}
	g.running.Add(1)
	g.mu.Unlock()
	defer g.running.Done()
	g.h.ServeHTTP(w, r)
}

func (g *requestGate) stop() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
}

func (g *requestGate) wait() { g.running.Wait() }

// watchRoot reports when the storage root last changed, so the page reloads
// its list. It only lists directories: the folding is still done when a
// reader asks for a conversation.
func watchRoot(srv *view.Server, every time.Duration, pulled *pulledPage) {
	seen := ""
	for {
		now := chainsFingerprint(srv) + pulled.fingerprint()
		if now != seen {
			seen = now
			srv.Touch(time.Now())
		}
		time.Sleep(every)
	}
}

// watchPulled marks the list changed when the pulled conversations do: a
// pull that adds a conversation or a snapshot, and a sweep that removes one.
// asz server's own passes mark it for what they collect.
func watchPulled(srv *view.Server, every time.Duration, pulled *pulledPage) {
	seen := pulled.fingerprint()
	for {
		time.Sleep(every)
		if now := pulled.fingerprint(); now != seen {
			seen = now
			srv.Touch(time.Now())
		}
	}
}

// chainsFingerprint is the conversations in the root and, for each, the head
// round and the size of its file. That is what changes when another process
// lands or parses.
//
// The size is in it because a round file is listed before its bytes are all
// there. The head alone would then stop changing the moment the name
// appeared, and the page would never be told when the round was finished.
func chainsFingerprint(srv *view.Server) string {
	ids, err := srv.List()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, id := range ids {
		files, err := sessionflow.OpenChain(srv.Root(), id).List()
		if err != nil || len(files) == 0 {
			continue
		}
		last := files[len(files)-1]
		size := int64(-1)
		if fi, serr := os.Stat(last.Path); serr == nil {
			size = fi.Size()
		}
		fmt.Fprintf(&b, "%s:%d:%d;", id, last.Round, size)
	}
	return b.String()
}

// pollInterval is the shortest period any configured collector uses, which
// is how often the root can change, and the default when none is set.
func pollInterval(cfg *config.Config) time.Duration {
	var every time.Duration
	for _, ad := range cfg.Adapters {
		if ad.Enabled && ad.Collector.Interval > 0 && (every == 0 || ad.Collector.Interval < every) {
			every = ad.Collector.Interval
		}
	}
	if every == 0 {
		every = config.Default().Adapters[0].Collector.Interval
	}
	return every
}

// cmdServer is the pipeline and the page in one process: it lands what is
// new, parses what moved, sends what the export block asks for, and serves
// the result, on the collector's interval. It is what a person runs to
// watch their own conversations locally.
func cmdServer(cfg *config.Config, ads []config.Adapter, once bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(zoneRoot, 0o755); err != nil {
		return err
	}
	zone := storage.NewZone(zoneRoot)
	srv := view.NewWithGlossaries(zone, glossaries())
	if err := srv.SetHide(cfg.View.Hide, cfg.View.HideOverride); err != nil {
		return err
	}
	guard, err := protectionOf(cfg)
	if err != nil {
		return err
	}

	ref, err := newRefresher(srv, zone, ads, pipelineMetrics(cfg), cfg.Parse.MaxRoundBytes, once)
	if err != nil {
		return err
	}
	if ref == nil {
		return fmt.Errorf("server: no local source to collect from; run asz view to serve %s as it is", zoneRoot)
	}
	pusher, closeClient, err := newPusher(cfg, zoneRoot)
	if err != nil {
		return err
	}
	defer closeClient()
	ref.pusher = pusher
	metadataOnly, retire, err := cfg.Storage.Retention.Durations()
	if err != nil {
		return err
	}
	ref.retain(metadataOnly, retire)
	if once || ref.base.Mode == config.ModeOnce {
		// One pass and no loop after it, exactly as asz collect -once has:
		// what another process was holding is reported, not deferred.
		ref.last = true
		if pusher != nil {
			pusher.WaitForExport = exportWait
		}
	}

	ids, err := srv.List()
	if err != nil {
		return err
	}
	ln, err := serveAddr()
	if err != nil {
		return err
	}
	fmt.Printf("reading  : %s\n", zoneRoot)
	printPipeline(ref, cfg)
	fmt.Printf("serving  : %d conversation(s)\n", len(ids))
	printWithholding(cfg)
	fmt.Printf("\n   %s://%s\n\n", guard.scheme(), ln.Addr())
	fmt.Fprintln(os.Stderr, "ctrl-c to stop")
	guard.warn(ln)

	// The page is up before the first pass so a large backfill does not
	// look like a hung command; the page shows the pass running.
	go func() {
		// The page reports what a pass did, errors included, so a failure
		// here is shown rather than ending the process that serves it.
		_ = ref.pass()
		if ref.base.Mode == config.ModeWatch {
			ref.loop()
		}
	}()
	pulls, err := guard.pullServer(cfg, zoneRoot)
	if err != nil {
		return err
	}
	pulled, err := newPulledPage(cfg, zoneRoot)
	if err != nil {
		return err
	}
	go watchPulled(srv, pollInterval(cfg), pulled)
	page := pulled.handler(srv.Handler())
	if pulls != nil {
		return guard.serveWithPulls(ln, page, pulls)
	}
	return guard.serve(guard.server(guard.handler(page, nil)), ln)
}

// printWithholding says what the page withholds, and whether a request may
// say otherwise.
//
// A record is named for what it carries when it lands, and a root landed
// before asz named these carries no such name. Withholding there finds
// nothing, and the only sign is a zero in each document's summary, so the
// operator is told here, once, before anyone reads.
func printWithholding(cfg *config.Config) {
	const landedBefore = " A record landed before asz named these carries no name and is shown. Collect such a root again into a new one to withhold there."
	names := strings.Join(cfg.View.Hide, " and ")
	switch {
	case cfg.View.HideOverride && names == "":
		fmt.Println("withhold : nothing, unless a request's hide parameter names what to withhold (view.hide_override is on)." + landedBefore)
	case cfg.View.HideOverride:
		fmt.Printf("withhold : %s, unless a request's hide parameter names another set (view.hide_override is on).%s\n", names, landedBefore)
	case names != "":
		fmt.Printf("withhold : %s, from every reader.%s\n", names, landedBefore)
	}
}

// serveAddr listens on the address the command was given, or the default.
func serveAddr() (net.Listener, error) {
	addr := arg(0)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	return net.Listen("tcp", addr)
}

// printPipeline says what every pass will do, before the first one runs.
func printPipeline(ref *refresher, cfg *config.Config) {
	switch ref.base.Mode {
	case config.ModeWatch:
		fmt.Printf("source   : %s (every %s)\n", ref.base.Source, ref.interval)
	default:
		fmt.Printf("source   : %s (once)\n", ref.base.Source)
	}
	switch {
	case ref.deriver != nil:
		fmt.Printf("metrics  : %s, %s and %s, derived from the landed files, look-back %s on the first pass\n",
			metrics.TokenUsage, metrics.MCPCalls, metrics.MCPDuration, lookbackWord(ref.deriver.Lookback))
	case cfg.Metrics.On():
		fmt.Println("metrics  : not derived, since nothing would send them; set export.otlp.endpoint, with export.otlp.metrics on")
	}
	if ref.pusher == nil {
		fmt.Println("export   : none; set export.otlp.endpoint to send what is landed")
	} else {
		o := cfg.Export.OTLP
		endpoint := o.Endpoint + " over gRPC"
		if o.Protocol == otlp.ProtocolHTTP {
			endpoint = o.Endpoint + " over HTTP"
		}
		fmt.Printf("export   : %s, at the end of every pass\n", endpoint)
	}
	if rt := ref.retention; rt != nil {
		// Equal levels go straight to retirement, so level 1 does nothing.
		if rt.MetadataOnlyAfter > 0 && (rt.RetireAfter == 0 || rt.MetadataOnlyAfter < rt.RetireAfter) {
			fmt.Printf("retention: a conversation idle for %s keeps only its metadata; its content is removed, and only a receiver that accepted it still has it\n", rt.MetadataOnlyAfter)
		}
		if rt.RetireAfter > 0 {
			fmt.Printf("retention: a conversation idle for %s is retired: everything of it is removed, and it is never collected again, even when its runtime resumes it\n", rt.RetireAfter)
		}
	}
	for _, line := range ref.retentionWarnings {
		fmt.Printf("warning  : %s\n", line)
	}
	// A scenario root keeps the removal policy its build chose.
	if fi, err := os.Stat(filepath.Join(ref.zone.Root(), storage.ScenarioDir)); err == nil && fi.IsDir() {
		fmt.Printf("scenario : %s\n", ref.removalSays())
	}
}
