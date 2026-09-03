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

	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// cmdView serves the conversations in the storage root.
//
// It reads and never writes: the chain, the landed records and the index are
// all opened read-only, so a viewer can run beside a collector without taking
// the lock that would block it.
func cmdView(cfg *config.Config, _ config.Adapter, _ bool) error {
	zoneRoot, err := cfg.ResolvedRoot()
	if err != nil {
		return err
	}
	addr := arg(0)
	if addr == "" {
		addr = "127.0.0.1:8787"
	}
	srv := view.New(storage.NewZone(zoneRoot))
	ids, err := srv.List()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fmt.Errorf("no conversations in %s; run asz parse first", zoneRoot)
	}

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("reading  : %s\n", zoneRoot)
	fmt.Printf("serving  : %d conversation(s)\n", len(ids))
	fmt.Printf("\n   http://%s\n\n", ln.Addr())
	fmt.Fprintln(os.Stderr, "ctrl-c to stop")
	return http.Serve(ln, srv.Handler())
}
