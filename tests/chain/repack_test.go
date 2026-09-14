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
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/repack"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

func TestRepeatedRepackPreservesTheDestination(t *testing.T) {
	s := newStage(t, growing)
	s.through("two")
	dst := storage.NewZone(t.TempDir())
	if err := repack.CheckDestination(dst.Root()); err != nil {
		t.Fatal(err)
	}
	if _, err := repack.Session(s.zone, dst, s.session, 2<<20, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := parse.Session(dst, parse.Options{Conversation: s.session, Session: s.session}); err != nil {
		t.Fatal(err)
	}
	before, err := verify.LandedDigests(dst, s.session)
	if err != nil {
		t.Fatal(err)
	}
	if err := repack.CheckDestination(dst.Root()); err == nil {
		t.Fatal("populated destination accepted")
	}
	if _, err := repack.Session(s.zone, dst, s.session, 1024, time.Now()); err == nil {
		t.Fatal("repeated repack accepted")
	}
	after, err := verify.LandedDigests(dst, s.session)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("refused repack changed landed files")
	}
	report, err := verify.Chain(dst, s.session, nil)
	if err != nil || !report.OK() {
		t.Fatalf("destination chain changed: %+v, %v", report, err)
	}
}

func TestRepackRefusesAnOrphanDestinationChain(t *testing.T) {
	s := newStage(t, growing)
	s.through("one")
	dst := storage.NewZone(t.TempDir())
	chain := sessionflow.OpenChain(dst.Root(), s.session)
	if err := os.MkdirAll(chain.Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(chain.Dir(), "keep")
	if err := os.WriteFile(sentinel, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := repack.Session(s.zone, dst, s.session, 1024, time.Now()); err == nil {
		t.Fatal("destination chain without session data accepted")
	}
	if _, err := os.Stat(dst.SessionDir(s.session)); !os.IsNotExist(err) {
		t.Fatalf("session directory created: %v", err)
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "original" {
		t.Fatalf("chain changed: %q, %v", data, err)
	}
}
