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
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/changes"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// partsKeepSourceBytes checks that a landed part holds its source's own
// bytes wherever it carries the source's own JSON. A record's off and bytes
// name a slice of its source file, and its sha must be the digest of that
// slice. A call, result or data part's data must occur in the slice byte for
// byte, and so must the bytes an unknown part keeps. Two kinds of part are
// asz's own encoding and are not compared: a changes/1 record an adapter
// derives beside a transcript's result, and a media part, whose base64 is
// written again as a string. A document landed again from its first record
// is a new version, and its source holds only the newest, so an older version
// is not compared.
//
// sources maps an adapter's name, the start of a header's adapter field, to
// the directory its src is relative to.
func partsKeepSourceBytes(root, session string, sources map[string]string) ([]string, error) {
	files, err := storage.LandedFiles(storage.NewZone(root), session)
	if err != nil {
		return nil, err
	}
	type landed struct {
		name string
		hdr  sessiondata.Header
		recs []*sessiondata.Record
	}
	var all []landed
	type sourceKey struct{ adapter, path string }
	newest := map[sourceKey]int{}
	for _, lf := range files {
		f, err := os.Open(lf.Path)
		if err != nil {
			return nil, err
		}
		hdr, recs, err := sessiondata.All(f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", lf.Path, err)
		}
		if len(recs) > 0 && recs[0].Ord == 1 {
			newest[sourceKey{hdr.Adapter, hdr.Src}] = len(all)
		}
		all = append(all, landed{filepath.Base(lf.Path), hdr, recs})
	}
	var out []string
	fail := func(format string, a ...any) {
		out = append(out, "parts_keep_source_bytes: "+fmt.Sprintf(format, a...))
	}
	for i, l := range all {
		if i < newest[sourceKey{l.hdr.Adapter, l.hdr.Src}] {
			continue
		}
		adapter, _, _ := strings.Cut(l.hdr.Adapter, "/")
		dir, ok := sources[adapter]
		if !ok {
			fail("%s: no source directory for adapter %s", l.name, adapter)
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(l.hdr.Src)))
		if err != nil {
			return nil, err
		}
		for row, rec := range l.recs {
			end := rec.Off + uint64(rec.Bytes)
			if rec.Bytes < 0 || end < rec.Off || end > uint64(len(src)) {
				fail("%s row %d: bytes %d to %d are past the end of its source", l.name, row+1, rec.Off, end)
				continue
			}
			line := src[rec.Off:end]
			sum := sha256.Sum256(line)
			if got := hex.EncodeToString(sum[:])[:12]; got != rec.Sha {
				fail("%s row %d: sha %s, and its source's bytes give %s", l.name, row+1, rec.Sha, got)
				continue
			}
			for b, p := range rec.Parts {
				var kept []byte
				switch p.Kind {
				case sessiondata.PartCall, sessiondata.PartResult, sessiondata.PartData:
					if len(p.Data) == 0 {
						continue
					}
					if l.hdr.Kind == sessiondata.KindTranscript && p.Kind == sessiondata.PartData {
						if _, derived := changes.Decode(p.Data); derived {
							continue
						}
					}
					kept = p.Data
				case sessiondata.PartUnknown:
					raw, err := p.Raw()
					if err != nil {
						fail("%s row %d block %d: %v", l.name, row+1, b, err)
						continue
					}
					kept = raw
				default:
					continue
				}
				if !bytes.Contains(line, kept) {
					fail("%s row %d block %d: the %s part's data is not its source's bytes", l.name, row+1, b, p.Kind)
				}
			}
		}
	}
	return out, nil
}
