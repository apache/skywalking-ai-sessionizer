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
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/metrics"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// cmdView serves the conversations in the storage root, and only reads.
//
// The chain, the landed records and the index are opened read-only, and
// nothing in this process collects, parses or sends. It is the web host: a
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
	srv := view.New(zone, claudecode.Glossary())
	ids, err := srv.List()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("no conversations in %s; run asz collect first, or asz server to do both", zoneRoot)
	}
	ln, err := serveAddr()
	if err != nil {
		return err
	}
	fmt.Printf("reading  : %s\nserving  : %d conversation(s)\n", zoneRoot, len(ids))
	fmt.Printf("\n   http://%s\n\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "ctrl-c to stop")
	// Nothing here writes, but something else may: an asz collect running
	// beside this page fills the same root. Watching the chains costs a
	// directory listing, and without it an open list page would never show
	// a conversation that arrived after it was loaded.
	go watchRoot(srv, pollInterval(cfg))
	return http.Serve(ln, srv.Handler())
}

// watchRoot reports when the storage root last changed, so the page reloads
// its list. It only lists directories: the folding is still done when a
// reader asks for a conversation.
func watchRoot(srv *view.Server, every time.Duration) {
	seen := ""
	for {
		now := chainsFingerprint(srv)
		if now != seen {
			seen = now
			st := srv.Status()
			st.LastRefresh = time.Now().UnixMilli()
			srv.SetStatus(st)
		}
		time.Sleep(every)
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
	srv := view.New(zone, claudecode.Glossary())

	ref, err := newRefresher(srv, zone, ads, cfg.Parse.MaxRoundBytes, once)
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
	fmt.Printf("\n   http://%s\n\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "ctrl-c to stop")

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
	return http.Serve(ln, srv.Handler())
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
	if ref.deriver != nil {
		fmt.Printf("metrics  : %s, derived from the landed files, look-back %s on the first pass\n",
			metrics.TokenUsage, lookbackWord(ref.deriver.Lookback))
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
	// A root a claude-code scenario build wrote. Only there does a pipeline
	// remove anything, and only what a build marked.
	if fi, err := os.Stat(filepath.Join(ref.zone.Root(), storage.ScenarioDir)); err == nil && fi.IsDir() {
		fmt.Printf("scenario : %s\n", ref.removalSays())
	}
}
