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

package verify_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// landBodies cuts bodies under a budget and lands them in one provider_body
// file, as a collector does, letting edit change a record before it lands.
func landBodies(t *testing.T, dir string, budget int64, edit func(*sessiondata.Record), bodies ...[]byte) {
	t.Helper()
	s := providerbody.NewSession()
	s.Budget(budget)
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{Seq: 1, At: "2026-01-01T00:00:00Z",
		Kind: sessiondata.KindProviderBody, Adapter: "a/1", Dialect: "d", Src: ".", Session: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range bodies {
		rec, err := s.Encode(providerbody.Body{ID: "body" + string(rune('a'+i)), Role: providerbody.RoleResponse, Src: "x.json", Bytes: b})
		if err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(rec)
		}
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider_body-20260101T000000.000000000Z-000001.sd"), buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
}

func screenshotBody() []byte {
	return []byte(`{"content":[{"type":"text","text":"` + strings.Repeat("iVBORw0KGgo", 4_000) + `"}],"id":"msg_1"}`)
}

// A body that holds a piece too large to land is counted, not reported: the
// collector left the piece out on purpose.
func TestABodyThatCannotBeRebuiltForAPieceTooLargeIsCounted(t *testing.T) {
	dir := t.TempDir()
	landBodies(t, dir, 16<<10, nil, screenshotBody())
	rep, problems, err := verify.Provider(dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 0 || rep.Unbuilt != 1 || rep.TooLarge != 1 {
		t.Fatalf("problems %v, unbuilt %d, too large %d; want none, 1 and 1", problems, rep.Unbuilt, rep.TooLarge)
	}
}

// Such a body is still held to its manifest: a record whose size is not the
// body's is a problem whether or not the body can be rebuilt.
func TestABodyThatCannotBeRebuiltIsStillHeldToItsManifest(t *testing.T) {
	dir := t.TempDir()
	landBodies(t, dir, 16<<10, func(r *sessiondata.Record) { r.Bytes++ }, screenshotBody())
	_, problems, err := verify.Provider(dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0].Why, "size or digest") {
		t.Fatalf("problems %v, want the record's size reported", problems)
	}
}

// A repeat is held to its manifest too: the same record landed again with
// another size is a problem, though the session holds the body already.
func TestARepeatIsStillHeldToItsManifest(t *testing.T) {
	dir := t.TempDir()
	landBodies(t, dir, 16<<10, nil, screenshotBody())
	first := filepath.Join(dir, "provider_body-20260101T000000.000000000Z-000001.sd")
	f, err := os.Open(first)
	if err != nil {
		t.Fatal(err)
	}
	hdr, recs, err := sessiondata.All(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	again := *recs[0]
	again.Bytes++
	hdr.Seq = 2
	var buf bytes.Buffer
	w, err := sessiondata.NewWriter(&buf, &hdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(&again); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider_body-20260101T000000.000000000Z-000002.sd"), buf.Bytes(), 0o444); err != nil {
		t.Fatal(err)
	}
	rep, problems, err := verify.Provider(dir, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Relanded != 1 || len(problems) != 1 || !strings.Contains(problems[0].Why, "size or digest") {
		t.Fatalf("relanded %d, problems %v; want the repeat counted and its size reported", rep.Relanded, problems)
	}
}
