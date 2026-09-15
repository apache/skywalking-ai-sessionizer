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

package verify

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/pkg/providerbody"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// BodyProblem is a provider body that does not come back whole.
type BodyProblem struct {
	File string
	Row  uint32
	Why  string
}

func (p BodyProblem) String() string {
	return fmt.Sprintf("%s row %d: %s", filepath.Base(p.File), p.Row, p.Why)
}

// Provider checks a session's provider bodies.
//
// Each body is a whole document, one record at ord 1 and offset 0, so the
// line and byte checks of Stream say nothing about them. What can go wrong is
// a body that no longer rebuilds: a piece or an earlier body it names is not
// there, or the bytes it rebuilds to do not match its digest. So every body is
// rebuilt, in landing order, and compared. A body landed twice with the same
// digest is a repeat, as in a stream.
func Provider(dir string) (*StreamReport, []BodyProblem, error) {
	files, err := landedFiles(dir, string(sessiondata.KindProviderBody))
	if err != nil {
		return nil, nil, err
	}
	rep := &StreamReport{Dir: dir, Files: len(files)}
	var problems []BodyProblem
	s := providerbody.NewSession()
	for _, path := range files {
		f, err := os.Open(path)
		if err != nil {
			return nil, nil, err
		}
		r, err := sessiondata.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("verify: %s: %w", path, err)
		}
		for row := uint32(1); ; row++ {
			rec, rerr := r.Next()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				f.Close()
				return nil, nil, fmt.Errorf("verify: %s row %d: %w", path, row, rerr)
			}
			rep.Records++
			rep.FirstOrd, rep.LastOrd = 1, 1
			if err := s.Add(rec); err != nil {
				if errors.Is(err, providerbody.ErrRepeat) {
					rep.Relanded++
					continue
				}
				problems = append(problems, BodyProblem{File: path, Row: row, Why: err.Error()})
				continue
			}
			m, _ := providerbody.ManifestOf(rec)
			body, err := s.Body(rec.ID)
			switch {
			case err != nil:
				problems = append(problems, BodyProblem{File: path, Row: row, Why: err.Error()})
			case rec.Bytes != len(body) || len(m.SHA256) < 12 || rec.Sha != m.SHA256[:12]:
				problems = append(problems, BodyProblem{File: path, Row: row,
					Why: "the record's size or digest is not the body's"})
			}
		}
		f.Close()
	}
	return rep, problems, nil
}
