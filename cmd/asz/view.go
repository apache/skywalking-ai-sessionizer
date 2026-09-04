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

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// cmdView serves the conversations in the storage root.
//
// The page only reads: the chain, the landed records and the index are opened
// read-only. When the adapter is the local Claude Code one and its source is
// on this machine, the same process also runs the collector and the parser -
// once, or on the configured interval in watch mode - so one command is a
// complete local setup, and the list page can say when its data was last
// refreshed and when it will be next.
func cmdView(cfg *config.Config, ad config.Adapter, once bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	addr := arg(0)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	zone := storage.NewZone(zoneRoot)
	srv := view.New(zone, claudecode.Glossary())

	var ref *refresher
	if ad.Name == config.AdapterClaudeCodeLocal {
		if err := os.MkdirAll(zoneRoot, 0o755); err != nil {
			return err
		}
		if ref, err = newRefresher(srv, zone, ad, cfg.Parse.MaxRoundBytes, once); err != nil {
			return err
		}
	}

	ids, err := srv.List()
	if err != nil {
		return err
	}
	if len(ids) == 0 && ref == nil {
		return fmt.Errorf("no conversations in %s; run asz parse first", zoneRoot)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("reading  : %s\n", zoneRoot)
	if ref != nil {
		switch ref.base.Mode {
		case config.ModeWatch:
			fmt.Printf("source   : %s (refreshed every %s)\n", ref.base.Source, ref.interval)
		default:
			fmt.Printf("source   : %s (refreshed once)\n", ref.base.Source)
		}
	}
	fmt.Printf("serving  : %d conversation(s)\n", len(ids))
	fmt.Printf("\n   http://%s\n\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "ctrl-c to stop")

	if ref != nil {
		// The page is up before the first pass so a large backfill does not
		// look like a hung command; the page shows the pass running.
		go func() {
			ref.pass()
			if ref.base.Mode == config.ModeWatch {
				ref.loop()
			}
		}()
	}
	return http.Serve(ln, srv.Handler())
}
