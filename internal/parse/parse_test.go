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

package parse_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/parse"
	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

const session = "0438c73b-2367-4ed5-9de3-13ef9a17ed01"

// landOne writes one landed file of one record, the least a parse turns
// into a round.
func landOne(t *testing.T, z *storage.Zone) {
	t.Helper()
	path := filepath.Join(z.StreamDir(session, storage.StreamMain), storage.LandedName("transcript", storage.Stamp(time.Unix(0, 0)), 1))
	err := storage.WriteAtomic(path, storage.PermLanded, func(w io.Writer) error {
		sw, err := sessiondata.NewWriter(w, &sessiondata.Header{H: 1, Seq: 1, At: "2026-09-11T00:00:00Z",
			Kind: sessiondata.KindTranscript, Adapter: "test/0", Dialect: "test/1",
			Src: "-p/" + session + ".jsonl", Session: session, Stream: storage.StreamMain})
		if err != nil {
			return err
		}
		if err := sw.Write(&sessiondata.Record{Ord: 1, Sha: "a", Bytes: 1, ID: "u1", From: sessiondata.FromAgent,
			Time:  "2026-09-11T00:00:01Z",
			Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hello", State: "available", Bytes: 5}}}); err != nil {
			return err
		}
		return sw.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// A parse of a session with nothing landed and no chain creates nothing: no
// chain directory and no session directory. The same holds when the chain's
// directory exists holding only its lock.
func TestAParseOfNothingCreatesNothing(t *testing.T) {
	root := t.TempDir()
	z := storage.NewZone(root)
	chainDir := filepath.Join(root, "_conversations", session)
	opt := parse.Options{Conversation: session, Session: session}

	r, err := parse.Session(z, opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Changed() {
		t.Fatalf("a parse of nothing wrote round %d", r.Number)
	}
	if exists(t, chainDir) || exists(t, z.SessionDir(session)) {
		t.Fatal("a parse of nothing created a directory")
	}

	lock, err := sessionflow.OpenChain(root, session).Lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	if r, err = parse.Session(z, opt); err != nil {
		t.Fatal(err)
	}
	if r.Changed() {
		t.Fatalf("a parse of a chain with no round and no landed file wrote round %d", r.Number)
	}
	items, err := os.ReadDir(chainDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Name() != ".lock" {
		var names []string
		for _, it := range items {
			names = append(names, it.Name())
		}
		t.Fatalf("the chain holds %v, want only its lock", names)
	}
	if exists(t, z.SessionDir(session)) {
		t.Fatal("a parse of nothing created the session directory")
	}
}

// The chain lock comes before the index. A parse that meets a held chain
// returns at once, and does not rebuild an index it could not use. Once the
// session and its chain are removed whole, a parse creates nothing again.
func TestParseTakesTheChainBeforeTheIndex(t *testing.T) {
	root := t.TempDir()
	z := storage.NewZone(root)
	chainDir := filepath.Join(root, "_conversations", session)
	opt := parse.Options{Conversation: session, Session: session}
	landOne(t, z)
	r, err := parse.Session(z, opt)
	if err != nil {
		t.Fatal(err)
	}
	if r.Number != 1 {
		t.Fatalf("the first parse wrote round %d, want 1", r.Number)
	}
	if err := storage.DeleteTree(z.IndexDir(session), nil); err != nil {
		t.Fatal(err)
	}

	lock, err := sessionflow.OpenChain(root, session).Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parse.Session(z, opt); !errors.Is(err, storage.ErrChainBusy) {
		t.Fatalf("a parse against a held chain gave %v, want ErrChainBusy", err)
	}
	if exists(t, z.IndexDir(session)) {
		t.Fatal("the parse rebuilt the index before it held the chain")
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}

	if r, err = parse.Session(z, opt); err != nil {
		t.Fatal(err)
	}
	if r.Changed() || !exists(t, z.IndexDir(session)) {
		t.Fatalf("after the release: round %d, index rebuilt %v; want the index rebuilt and no new round",
			r.Number, exists(t, z.IndexDir(session)))
	}

	// The session and its chain removed whole, as a scenario removal leaves
	// them.
	for _, dir := range []string{z.SessionDir(session), chainDir} {
		if err := storage.DeleteTree(dir, nil); err != nil {
			t.Fatal(err)
		}
	}
	if r, err = parse.Session(z, opt); err != nil {
		t.Fatal(err)
	}
	if r.Changed() || exists(t, chainDir) || exists(t, z.SessionDir(session)) {
		t.Fatalf("a parse after the removal wrote round %d or made a directory again", r.Number)
	}
}
