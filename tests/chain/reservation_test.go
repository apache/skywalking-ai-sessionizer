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

package chain_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/export/otlp"
	"github.com/apache/skywalking-ai-sessionizer/internal/retention"
	"github.com/apache/skywalking-ai-sessionizer/internal/scenario"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// abandon leaves an empty file under a round's name beside a conversation's
// head round, older than a minute. On a filesystem with no exclusive rename,
// a writer reserves a round's name with an empty file and then renames the
// round over it. One that stopped between the two, and was run again within
// the minute over other evidence, published the round under another digest
// and left the reservation. Nothing removes it until the chain's next
// publication, which an idle conversation never has. It sorts before the
// round.
func abandon(t *testing.T, root, session string) {
	t.Helper()
	chain := sessionflow.OpenChain(root, session)
	files, err := chain.List()
	if err != nil || len(files) == 0 {
		t.Fatalf("the chain has no round: %v", err)
	}
	path := filepath.Join(chain.RoundsDir(), fmt.Sprintf("r%06d-%s.sf", files[len(files)-1].Round, strings.Repeat("0", 12)))
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
}

// TestAnAbandonedReservationIsNoRound. Every reader passes over an
// abandoned reservation, as the chain does: a push sends every round and
// leaves none for later, retention removes the conversation's files or
// retires it, and a scenario removal takes a sent session. None waits for
// the reservation, or fails reading it.
func TestAnAbandonedReservationIsNoRound(t *testing.T) {
	base := newSentRoot(t)
	b := growthAt(t, base, 2, func(k int) { base.pipelineAt(landedAt(k)) })
	abandon(t, base.dir, b.Session)
	t.Run("push", func(t *testing.T) {
		// The copy forgets what was sent, so the pass must send every landed
		// file and every round past the reservation, not only report no error.
		r := base.copyOf(t)
		if err := os.Remove(filepath.Join(r.dir, otlp.StateFile)); err != nil {
			t.Fatal(err)
		}
		files, err := storage.LandedFiles(r.zone, b.Session)
		if err != nil {
			t.Fatal(err)
		}
		rounds, err := sessionflow.OpenChain(r.dir, b.Session).List()
		if err != nil || len(rounds) == 0 {
			t.Fatalf("the chain has no round: %v", err)
		}
		client, err := r.rcv.Client(otlp.ProtocolGRPC)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		p := &otlp.Pusher{Zone: r.zone, Client: client, Endpoint: r.endpoint, Version: "test",
			ServiceName: "Removal Test", InstanceID: "removal-test", Layer: "AI_AGENT"}
		st, err := p.Pass()
		if err != nil || len(st.Errors) > 0 || st.Deferred != 0 {
			t.Fatalf("the push: %v %+v", err, st)
		}
		if want := len(files) + len(rounds); st.Files != want {
			t.Fatalf("the push sent %d files, want the %d landed files and %d rounds", st.Files, len(files), len(rounds))
		}
	})
	for _, retire := range []bool{false, true} {
		name := "metadata only"
		if retire {
			name = "retired"
		}
		t.Run(name, func(t *testing.T) {
			r := base.copyOf(t)
			if retire {
				res := retirementOf(r).Pass(retention.Input{})
				if len(res.Errors) > 0 || len(res.Retired) != 1 || res.Retired[0] != b.Session {
					t.Fatalf("the retirement: %+v", res)
				}
				retiredWhole(t, r.zone, b.Session)
				return
			}
			if res := retentionOf(r).Pass(retention.Input{}); len(res.Errors) > 0 || res.Files == 0 {
				t.Fatalf("the removal: %+v", res)
			}
		})
	}
	t.Run("scenario removal", func(t *testing.T) {
		r := newSentRoot(t)
		built := r.build(removalScenario(t), scenario.Options{})
		m := readMarker(t, built.Marker)
		r.pipeline()
		abandon(t, r.dir, built.Session)
		res := r.remover().Pass(r.input())
		noRemovalErrors(t, res)
		if !slices.Contains(res.Removed, built.Session) {
			t.Fatalf("removed %v, kept %v, waiting %v", res.Removed, res.Kept, res.Waiting)
		}
		r.gone(built, m)
	})
}
