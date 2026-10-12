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

package run

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// A source line that is not JSON lands as an unknown part, whose text is
// the reason, not content. Taken out as too large, it keeps the reason and
// names the digest of the line, and the check accepts it; with another
// digest it does not.
func TestAnUnknownPartTakenOutKeepsItsReason(t *testing.T) {
	line := []byte("{not json " + strings.Repeat("z", 10_000))
	sum := sha256.Sum256(line)
	for _, tc := range []struct {
		digest string
		ok     bool
	}{{hex.EncodeToString(sum[:]), true}, {strings.Repeat("0", 64), false}} {
		root, src := t.TempDir(), t.TempDir()
		if err := os.MkdirAll(filepath.Join(src, "proj"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(src, "proj", "s.jsonl"), append(line, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		z := storage.NewZone(root)
		hdr := &sessiondata.Header{Seq: 1, At: "2026-01-01T00:00:00Z", Kind: sessiondata.KindTranscript,
			Adapter: "claude-code-local/0.1.0", Dialect: "claude-code/1", Src: "proj/s.jsonl", Session: "s1", Stream: "main"}
		var buf bytes.Buffer
		w, err := sessiondata.NewWriter(&buf, hdr)
		if err != nil {
			t.Fatal(err)
		}
		rec := &sessiondata.Record{Ord: 1, Off: 0, Sha: hex.EncodeToString(sum[:])[:12], Bytes: len(line),
			Parts: []sessiondata.Part{{Kind: sessiondata.PartUnknown, Text: "the record is not valid JSON",
				State: model.ContentTooLarge, Bytes: len(line), SHA256: tc.digest}}}
		if err := w.Write(rec); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		dir := z.StreamDir("s1", "main")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, storage.LandedName("transcript", "20260101T000000.000000000Z", 1)), buf.Bytes(), 0o444); err != nil {
			t.Fatal(err)
		}
		out, err := partsKeepSourceBytes(root, "s1", map[string]string{"claude-code-local": src})
		if err != nil {
			t.Fatal(err)
		}
		if (len(out) == 0) != tc.ok {
			t.Fatalf("digest %s…: the check says %v, want accepted %v", tc.digest[:8], out, tc.ok)
		}
	}
}
