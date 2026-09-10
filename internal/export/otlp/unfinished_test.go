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

package otlp

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TestAnUnfinishedRoundIsNotSent.
//
// A round is written straight to its final name, so the file exists before
// all of its bytes do. Sending the short read would record it as sent and
// the finished bytes would never go at all. The commit frame is what says a
// round is whole.
func TestAnUnfinishedRoundIsNotSent(t *testing.T) {
	root := t.TempDir()
	conv := "0438c73b-2367-4ed5-9de3-13ef9a17ed01"
	dir := filepath.Join(root, "_conversations", conv, "rounds")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := "r000001-abcdefabcdef.sf"
	path := filepath.Join(dir, name)
	rel := "_conversations/" + conv + "/rounds/" + name
	header := `{"t":"header","schema":"1.0","conversation":"` + conv + `","session":"` + conv + `","round":1}` + "\n"

	// Mid-write: the header is there, the commit frame is not.
	if err := os.WriteFile(path, []byte(header), 0o644); err != nil {
		t.Fatal(err)
	}
	b := &batch{p: &Pusher{Zone: storage.NewZone(root)}, st: &Stats{}, state: &pushState{files: map[string]string{}}, services: map[string]string{}}
	if err := b.addRound(rel, path, conv); !errors.Is(err, errRoundUnfinished) {
		t.Fatalf("addRound gave %v for a round with no commit frame; it is still being written", err)
	}

	// A commit frame that does not carry the digest the name claims is the
	// same thing: some other round's bytes, or a partial line.
	if err := os.WriteFile(path, []byte(header+`{"t":"commit","digest":"999999999999aaaa"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.addRound(rel, path, conv); !errors.Is(err, errRoundUnfinished) {
		t.Fatalf("addRound gave %v for a commit frame whose digest is not the one in the name", err)
	}

	// That a finished round IS sent is covered by
	// TestPushSendsEveryFileOnceWithItsAttributes, which sends real rounds
	// through a receiver.
}
