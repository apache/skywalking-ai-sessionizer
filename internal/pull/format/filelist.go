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

package format

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"slices"
	"strconv"
)

// Digest is the SHA-256 of the canonical file list: one line per entry,
// sorted by the bytes of its name, with its fields in a fixed order and a
// dash for a field its kind does not have. The order the entries were given
// in, members the schema does not know, the generation, the server's tag and
// the lease do not change it; every stored name and byte does. Full digests,
// never a prefix, decide whether two manifests list the same evidence.
func (m *Manifest) Digest() ManifestDigest {
	h := sha256.New()
	_ = m.WriteFileList(h)
	return ManifestDigest(hex.EncodeToString(h.Sum(nil)))
}

// WriteFileList writes the canonical file list the digest is taken over.
func (m *Manifest) WriteFileList(w io.Writer) error {
	order := make([]int, len(m.Files))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		switch x, y := m.Files[a].File, m.Files[b].File; {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	})
	bw := bufio.NewWriter(w)
	number := func(n uint64, applies bool) string {
		if !applies {
			return "-"
		}
		return strconv.FormatUint(n, 10)
	}
	text := func(s string, applies bool) string {
		if !applies {
			return "-"
		}
		return s
	}
	for _, i := range order {
		e := m.Files[i]
		landed := e.Kind == KindLanded || e.Kind == KindSkeleton
		fields := []string{
			e.File, string(e.Kind),
			number(e.Seq, landed), number(e.Round, e.Kind == KindRound),
			strconv.FormatUint(e.Bytes, 10), string(e.Digest),
			text(string(e.LandedDigest), e.Kind == KindSkeleton), text(string(e.Content), e.Kind == KindStore),
		}
		for j, f := range fields {
			if j > 0 {
				_ = bw.WriteByte('\t')
			}
			_, _ = bw.WriteString(f)
		}
		_ = bw.WriteByte('\n')
	}
	return bw.Flush()
}
