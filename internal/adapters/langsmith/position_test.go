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

package langsmith

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
	"github.com/apache/skywalking-ai-sessionizer/pkg/skeleton"
)

// A stream's next position goes on from a file retention removed as from
// the landed file: its skeleton keeps every record's ordinal, offset and
// size. While a removal is in progress both are there, and read the same.
func TestThePositionGoesOnFromASkeleton(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	var data [][]byte
	for seq, ords := range [][]uint64{{1, 2}, {3, 4, 5}} {
		var buf bytes.Buffer
		w, err := sessiondata.NewWriter(&buf, &sessiondata.Header{H: 1, Schema: sessiondata.Schema, Seq: uint64(seq + 1),
			At: "2026-10-01T00:00:00Z", Kind: sessiondata.KindTranscript, Adapter: "langsmith-ingest/1", Dialect: "langsmith/1",
			Src: "t", Session: "s", Stream: "main"})
		if err != nil {
			t.Fatal(err)
		}
		for _, ord := range ords {
			if err := w.Write(&sessiondata.Record{Ord: ord, Off: (ord - 1) * 10, Bytes: 9, ID: "r",
				Parts: []sessiondata.Part{{Kind: sessiondata.PartText, Text: "123456789", State: model.ContentAvailable, Bytes: 9}}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, storage.LandedName("transcript", storage.Stamp(time.Unix(int64(seq), 0)), uint64(seq+1)))
		if err := os.WriteFile(path, buf.Bytes(), 0o444); err != nil {
			t.Fatal(err)
		}
		paths, data = append(paths, path), append(data, buf.Bytes())
	}
	wantOrd, wantOff, err := nextPosition(dir)
	if err != nil || wantOrd != 6 {
		t.Fatalf("position %d, %d, %v before any removal", wantOrd, wantOff, err)
	}
	for i, path := range paths {
		sk := strings.TrimSuffix(path, sessiondata.Ext) + sessiondata.SkeletonExt
		if err := storage.WriteAtomic(sk, storage.PermLanded, func(w io.Writer) error {
			return skeleton.Write(w, data[i], sessiondata.Removed{Generation: 1, Receiver: sessiondata.ReceiverAccepted})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if ord, off, err := nextPosition(dir); err != nil || ord != wantOrd || off != wantOff {
		t.Fatalf("position %d, %d, %v while a removal is in progress, want %d, %d", ord, off, err, wantOrd, wantOff)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if ord, off, err := nextPosition(dir); err != nil || ord != wantOrd || off != wantOff {
		t.Fatalf("position %d, %d, %v after the removal, want %d, %d", ord, off, err, wantOrd, wantOff)
	}
}
