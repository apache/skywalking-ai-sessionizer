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

package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLongLineEndsWhenTheFileIsShorterThanItWas covers a source cut short
// while its long line is read: the size taken before reading is larger than
// the file, so the window can never reach it. The window must stop growing
// when a read brings nothing new. Before, it doubled on, each read sized to
// the stale size: a source cut from gigabytes to a few bytes made it allocate
// gigabytes again and again. The size here is 64 GiB, so a window that keeps
// growing fails to allocate.
func TestLongLineEndsWhenTheFileIsShorterThanItWas(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(p, []byte(strings.Repeat("x", 300)), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf, end, err := longLine(f, 0, 64<<30, 16)
	if err != nil || end != -1 || len(buf) != 300 {
		t.Fatalf("got %d bytes, end %d, %v; want the 300 bytes there are and no line end", len(buf), end, err)
	}
}

func TestLongLineFindsTheEndOfTheFirstLine(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	long := strings.Repeat("x", 1000)
	body := long + "\n" + long + "\n" + "short\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf, end, err := longLine(f, 0, int64(len(body)), 16)
	if err != nil || end != 1000 || len(buf) < 1001 {
		t.Fatalf("got %d bytes, end %d, %v; want the first line's end at 1000", len(buf), end, err)
	}
	// The same from the second long line, which runs to the end of a file
	// still being written.
	unfinished := long + "\n" + long
	if err := os.WriteFile(p, []byte(unfinished), 0o644); err != nil {
		t.Fatal(err)
	}
	g, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if _, end, err := longLine(g, 1001, int64(len(unfinished)), 16); err != nil || end != -1 {
		t.Fatalf("end %d, %v; want no line end for a line still being written", end, err)
	}
}

// Keep trims a chunk to the lines that fit a file: reading stops before the
// first line left out, the uuid is the last landed line's, and more is to
// come. Keeping none leaves the uuid the cursor had.
func TestKeepStopsBeforeTheFirstLineLeftOut(t *testing.T) {
	mk := func() *Chunk {
		ch := &Chunk{NewOffset: 300, NewOrd: 13, LastUUID: "u3"}
		for i, u := range []string{"u1", "u2", "u3"} {
			ch.Lines = append(ch.Lines, Line{Ord: uint64(11 + i), Off: uint64(100 * i), Bytes: []byte(`{"uuid":"` + u + `"}`)})
		}
		return ch
	}
	ch := mk()
	ch.Keep(2, "u0")
	if len(ch.Lines) != 2 || ch.NewOffset != 200 || ch.NewOrd != 12 || !ch.More || ch.LastUUID != "u2" {
		t.Fatalf("kept %d lines to offset %d and line %d, more %v, uuid %q; want 2 to 200 and 12, more, u2",
			len(ch.Lines), ch.NewOffset, ch.NewOrd, ch.More, ch.LastUUID)
	}
	ch = mk()
	ch.Keep(0, "u0")
	if len(ch.Lines) != 0 || ch.NewOffset != 0 || ch.NewOrd != 10 || ch.LastUUID != "u0" {
		t.Fatalf("kept %d lines to offset %d and line %d, uuid %q; want none, the cursor's place and uuid", len(ch.Lines), ch.NewOffset, ch.NewOrd, ch.LastUUID)
	}
	ch = mk()
	ch.Keep(3, "u0")
	if ch.NewOffset != 300 || ch.More || ch.LastUUID != "u3" {
		t.Fatal("keeping every line changed the chunk")
	}
}
