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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/kiro"
	"github.com/apache/skywalking-ai-sessionizer/internal/config"
	"github.com/apache/skywalking-ai-sessionizer/internal/judge"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/view"
)

// startJudgePolicy makes asz server judge after its watching passes, when
// judge.server is on. A configuration it cannot judge with on its own stops
// the server before it starts; a single pass judges nothing, and says so.
func startJudgePolicy(cfg *config.Config, ref *refresher, srv *view.Server, zone *storage.Zone, single bool) error {
	s := cfg.Judge.Server
	if !s.Enabled {
		return nil
	}
	if single {
		fmt.Println("judge    : judge.server is on, but this server makes one pass; nothing is judged on its own")
		return nil
	}
	if err := judge.CheckPolicy(cfg.Judge); err != nil {
		return fmt.Errorf("server: %w", err)
	}
	if fi, err := os.Stat(filepath.Join(zone.Root(), storage.ScenarioDir)); err == nil && fi.IsDir() {
		return errors.New("server: judge.server does not judge a scenario root")
	}
	pricing, err := judge.LoadPricing(cfg.Judge.Pricing, "")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	root := zone.Root()
	ref.policy = &judge.Policy{
		Zone: zone, Config: cfg.Judge, Facts: ref.judgeFacts,
		NewRunner: func() *judge.Runner {
			return &judge.Runner{
				Zone: zone, Config: cfg.Judge, Pricing: pricing, Version: version,
				Dialect: func(session string) string { return dialectOf(zone, session) },
				Decoder: decoderOf,
				MCP: func(a judge.MCPArgs) []string {
					return append(navigationCommand(self, root, cfg.Judge.Hide, a), windowArgs(a.Window)...)
				},
				Log: func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
			}
		},
		Origin: func(session string) (string, error) { return originOf(zone, session) },
		OriginNow: func(session string) (string, error) {
			if ref.kiroCol != nil && dialectOf(zone, session) == kiro.Dialect {
				if o, found, err := kiro.SourceOrigin(ref.kiroSource, zone, session); err != nil || found {
					return o, err
				}
			}
			return originOf(zone, session)
		},
		Log: func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	srv.SetJudgeStatus(func() any { return ref.policy.Status() })
	// An interrupt or a stop admits nothing more, lets a pass that is running
	// finish, and gives the run going its bounded cleanup. A second one stops
	// at once.
	ref.ctx, ref.stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	// A signal halts admissions at once, whichever serving path then stops
	// the rest in order, and lets the next signal stop the process at once.
	context.AfterFunc(ref.ctx, func() {
		ref.policy.Halt()
		ref.stop()
	})
	var stages []string
	for _, p := range cfg.Judge.Plan {
		stages = append(stages, p.Stage+"="+p.Evaluator)
	}
	fmt.Printf("judge    : after each pass, a conversation nothing was collected for in %s; plan %s; at most %d runs in 24 hours, %d steps and %s each\n",
		s.IdleAfter, strings.Join(stages, ","), s.RunsPerDay, cfg.Judge.MaxSteps, cfg.Judge.RunTimeout)
	return nil
}

// stopsInOrder stops a judging server in the order its runs need, once,
// whichever path asks first: no pass and no admission starts, a pass that is
// running finishes, then the run going ends its work and publishes what
// finished, within its bounded cleanup.
type stopsInOrder struct {
	once     sync.Once
	ref      *refresher
	pipeline <-chan struct{}
}

func (s *stopsInOrder) stop() {
	s.once.Do(func() {
		s.ref.stop()
		s.ref.policy.Halt()
		fmt.Fprintln(os.Stderr, "stopping : a running pass finishes, then the judge run going publishes what finished; ctrl-c again to stop at once")
		<-s.pipeline
		s.ref.policy.Stop()
	})
}

// onSignal stops in order when a server that serves only the page is
// interrupted, and exits: that server does not return. One that answers
// pulls returns at a signal, after its own shutdown, and stops in order then.
func (s *stopsInOrder) onSignal(closeClient func()) {
	<-s.ref.ctx.Done()
	s.stop()
	closeClient()
	os.Exit(130)
}

// originOf says whether a session worked inside the storage root: a Claude
// Code session never does, since claude writes no session as an evaluator;
// a Kiro session is read from its landed header.
func originOf(zone *storage.Zone, session string) (string, error) {
	switch dialectOf(zone, session) {
	case claudecode.Dialect:
		return kiro.OriginOutside, nil
	case kiro.Dialect:
		return kiro.LandedOrigin(zone, session)
	}
	return kiro.OriginUnknown, nil
}
