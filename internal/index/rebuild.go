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

package index

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// Rebuild indexes every landed file with a sequence above afterSeq, appending
// to ix. Passing afterSeq = 0 rebuilds the whole index from scratch.
//
// It reads Session Data, which is already converted, so it needs no adapter:
// a storage root separated from the runtime that produced it can still be
// indexed, and the side that assembles conversations never imports the side
// that collects them.
//
// This is the recovery path, not the normal one. Indexing normally rides along
// with landing, where the bytes are already in hand. But cursors commit per
// source while the index is written once per session, so a crash between them
// advances the cursors without recording their entries - and because nothing
// re-lands, the gap would be permanent and silent. Re-reading the landed files
// is the only way to close it.
func Rebuild(z *storage.Zone, session string, ix *Index, afterSeq uint64) (int, error) {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return 0, err
	}
	var indexed int
	for _, lf := range files {
		if lf.Seq <= afterSeq {
			continue
		}
		n, err := indexLandedFile(ix, lf)
		if err != nil {
			return indexed, err
		}
		indexed += n
	}
	return indexed, nil
}

// indexLandedFile indexes one landed file.
func indexLandedFile(ix *Index, lf storage.LandedFile) (int, error) {
	f, err := os.Open(lf.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	r, err := sessiondata.NewReader(f)
	if err != nil {
		// A landed file whose header will not read describes an unknown amount
		// of the conversation. Continuing would produce an index that silently
		// omits it, and an assembly that looks complete.
		return 0, fmt.Errorf("index: landed file %s is unreadable: %w", lf.Path, err)
	}
	hdr := r.Header()
	if hdr.Stream == "" {
		hdr.Stream = lf.Stream
	}
	if hdr.Batch == "" {
		hdr.Batch = lf.RunID
	}

	var n int
	for row := uint32(1); ; row++ {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return n, fmt.Errorf("index: %s row %d: %w", lf.Path, row, err)
		}
		e, blocks := FromRecord(ix, &hdr, rec, uint32(lf.Seq), row)
		ix.Append(e, blocks...)
		n++
	}
	return n, nil
}
