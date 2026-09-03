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
	"fmt"
	"os"
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// SessionReport aggregates every stream and run in one session.
type SessionReport struct {
	Session  string
	Streams  []*StreamReport
	Records  int
	Problems int
	// Relanded counts records an interrupted pass caused to be landed twice.
	// It is reported so it is visible, not because it is wrong.
	Relanded int
}

// OK reports whether every stream in the session is contiguous and intact.
func (r *SessionReport) OK() bool { return r.Problems == 0 }

// Session checks every landed stream and run of one session.
func Session(z *storage.Zone, session string) (*SessionReport, error) {
	rep := &SessionReport{Session: session}

	check := func(dir string, kinds ...string) error {
		for _, k := range kinds {
			sr, err := Stream(dir, k)
			if err != nil {
				return err
			}
			if sr.Files == 0 {
				continue
			}
			rep.Streams = append(rep.Streams, sr)
			rep.Records += sr.Records
			rep.Relanded += sr.Relanded
			rep.Problems += len(sr.OrdGaps) + len(sr.ByteGaps) + len(sr.ShaBad)
		}
		return nil
	}

	for _, group := range []struct {
		sub   string
		kinds []string
	}{
		{"streams", []string{"transcript", "meta"}},
		{"runs", []string{"journal", "manifest", "script"}},
	} {
		base := filepath.Join(z.SessionDir(session), group.sub)
		owners, err := os.ReadDir(base)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		for _, o := range owners {
			if !o.IsDir() {
				continue
			}
			if err := check(filepath.Join(base, o.Name()), group.kinds...); err != nil {
				return nil, err
			}
		}
	}
	return rep, nil
}

// Problems returns a short description of everything wrong in a session.
func (r *SessionReport) Details() []string {
	var out []string
	for _, s := range r.Streams {
		for _, g := range s.OrdGaps {
			out = append(out, fmt.Sprintf("ord gap  %s", g))
		}
		for _, g := range s.ByteGaps {
			out = append(out, fmt.Sprintf("byte gap %s", g))
		}
		for _, g := range s.ShaBad {
			out = append(out, fmt.Sprintf("digest   %s row %d", filepath.Base(g.File), g.Row))
		}
	}
	return out
}
