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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/internal/verify"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// A removed file is reported with what the receiver confirmed, and never
// checked as verified: a stream's line and byte checks go on through its
// skeleton, a removed provider body is not rebuilt, and a part too large to
// land is counted in either.
func TestRemovedFilesAreReportedNotVerified(t *testing.T) {
	for _, c := range []struct {
		kind sessiondata.Kind
		dir  func(z *storage.Zone) string
		run  func(dir string) (*verify.StreamReport, error)
	}{
		{sessiondata.KindTranscript, func(z *storage.Zone) string { return z.StreamDir("s", "main") },
			func(dir string) (*verify.StreamReport, error) { return verify.Stream(dir, "transcript") }},
		{sessiondata.KindProviderBody, func(z *storage.Zone) string { return z.ProviderDir("s") },
			func(dir string) (*verify.StreamReport, error) {
				rep, _, err := verify.Provider(dir, nil, 0)
				return rep, err
			}},
	} {
		t.Run(string(c.kind), func(t *testing.T) {
			z := storage.NewZone(t.TempDir())
			dir := c.dir(z)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			for seq := uint64(1); seq <= 2; seq++ {
				var buf bytes.Buffer
				w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: seq, At: "2026-10-01T00:00:00Z",
					Kind: c.kind, Adapter: "test/0", Dialect: "test/1", Src: "s", Session: "s", Stream: "main"})
				if err != nil {
					t.Fatal(err)
				}
				if err := w.Write(&sessiondata.Record{Ord: seq, Off: (seq - 1) * 3, Bytes: 2, ID: "r",
					Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "hi", State: model.ContentAvailable, Bytes: 2},
						{Kind: sessiondata.PartData, State: model.ContentTooLarge, Bytes: 5 << 20, SHA256: "feed"}}}); err != nil {
					t.Fatal(err)
				}
				if err := w.Close(); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, storage.LandedName(string(c.kind), storage.Stamp(time.Unix(int64(seq), 0)), seq))
				if seq == 1 {
					path = strings.TrimSuffix(path, sessiondata.Ext) + sessiondata.SkeletonExt
					if err := storage.WriteAtomic(path, storage.PermLanded, func(out io.Writer) error {
						return skeleton.Write(out, buf.Bytes(), sessiondata.Removed{Generation: 1, Receiver: sessiondata.ReceiverRejected})
					}); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(path, buf.Bytes(), 0o444); err != nil {
					t.Fatal(err)
				}
			}
			rep, err := c.run(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Removed) != 1 || rep.Removed[0].Receiver != sessiondata.ReceiverRejected || rep.Removed[0].Digest == "" {
				t.Fatalf("removed %+v, want the one skeleton, rejected", rep.Removed)
			}
			if rep.Records != 2 || len(rep.OrdGaps) != 0 || len(rep.ByteGaps) != 0 {
				t.Fatalf("%d records, gaps %v %v, want 2 and none", rep.Records, rep.OrdGaps, rep.ByteGaps)
			}
			// A part too large to land is counted in the skeleton too, which
			// keeps its state, so a body built from it is not left unexplained.
			if rep.TooLarge != 2 {
				t.Fatalf("%d parts too large to land counted, want 2, the skeleton's included", rep.TooLarge)
			}
		})
	}
}
