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
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// SessionReport aggregates every stream and run in one session.
type SessionReport struct {
	Session  string
	Streams  []*StreamReport
	Records  int
	Problems int
	// EndGaps counts the streams whose landed records stop before where
	// their cursor says the collector read to. They are counted in Problems
	// too. A cursor stays in the storage root and never travels with the
	// landed files, so a root rebuilt from those files alone can show every
	// other problem and never this one.
	EndGaps int
	// Relanded counts records an interrupted pass caused to be landed twice.
	// It is reported so it is visible, not because it is wrong.
	Relanded int
	// TooLarge counts the parts whose payload was too large to land, and
	// Unbuilt the provider bodies that cannot be rebuilt for it. They are
	// reported so they are visible, not because they are wrong.
	TooLarge, Unbuilt int
	// Removing counts the provider bodies of a removal in progress that no
	// longer rebuild, because it deleted one they are built from first. The
	// removal deletes them too, so they are not problems.
	Removing int
	// Bodies lists the provider bodies that do not rebuild. They are counted
	// in Problems too.
	Bodies []BodyProblem
	// Removed lists the files retention removed. They are reported so it is
	// visible what the root no longer holds, not because they are wrong.
	Removed []RemovedFile
}

// OK reports whether every stream in the session is contiguous and intact.
func (r *SessionReport) OK() bool { return r.Problems == 0 }

// Session checks every landed stream and run of one session.
func Session(z *storage.Zone, session string) (*SessionReport, error) {
	rep := &SessionReport{Session: session}

	check := func(dir string, kinds ...string) error {
		for _, k := range kinds {
			var reports []*StreamReport
			// The plugin's kinds are checked one source at a time: a stream
			// can hold more than one, each numbering its lines from 1.
			if k == string(sessiondata.KindChanges) || k == string(sessiondata.KindExecution) {
				srs, err := Sources(dir, k)
				if err != nil {
					return err
				}
				reports = srs
			} else {
				sr, err := Stream(dir, k)
				if err != nil {
					return err
				}
				reports = []*StreamReport{sr}
			}
			for _, sr := range reports {
				// A stream with no landed file of this kind is still checked
				// when its cursor says something was read: every file of it
				// is lost.
				if sr.Files == 0 && sr.End == nil {
					continue
				}
				rep.Streams = append(rep.Streams, sr)
				rep.Records += sr.Records
				rep.Relanded += sr.Relanded
				rep.TooLarge += sr.TooLarge
				rep.Removed = append(rep.Removed, sr.Removed...)
				rep.Problems += len(sr.OrdGaps) + len(sr.ByteGaps)
				if sr.End != nil {
					rep.Problems++
					rep.EndGaps++
				}
			}
		}
		return nil
	}

	for _, group := range []struct {
		sub   string
		kinds []string
	}{
		{"streams", []string{"transcript", "meta", "changes", "execution"}},
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
	// The bodies are checked against one view of the session's removals. A
	// removal that runs during the check deletes files the check reads, and a
	// body built from one it found deleted is no damage. So the check runs
	// again while the view it started from changed under it, and says it
	// could not tell when the view kept changing.
	var pr *StreamReport
	var bodies []BodyProblem
	for attempt := 1; ; attempt++ {
		before, err := removalOf(z, session)
		if err != nil {
			return nil, err
		}
		if beforeScan != nil {
			beforeScan()
		}
		pr, bodies, err = Provider(z.ProviderDir(session), z.BodyStore(session), before.through)
		if err != nil {
			return nil, err
		}
		after, err := removalOf(z, session)
		if err != nil {
			return nil, err
		}
		if after == before {
			break
		}
		if attempt == scanAttempts {
			bodies = []BodyProblem{{File: z.ProviderDir(session),
				Why: fmt.Sprintf("removals of the session changed while its bodies were checked, %d times; the check is inconclusive, run it again", scanAttempts)}}
			break
		}
	}
	if pr.Files > 0 {
		rep.Streams = append(rep.Streams, pr)
		rep.Records += pr.Records
		rep.Relanded += pr.Relanded
		rep.TooLarge += pr.TooLarge
		rep.Unbuilt += pr.Unbuilt
		rep.Removing += pr.Removing
		rep.Removed = append(rep.Removed, pr.Removed...)
		rep.Bodies = bodies
		rep.Problems += len(bodies)
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
		if s.End != nil {
			out = append(out, fmt.Sprintf("end gap  %s", *s.End))
		}
	}
	for _, b := range r.Bodies {
		out = append(out, fmt.Sprintf("body     %s", b))
	}
	return out
}

// scanAttempts bounds how many times the bodies are checked while removals
// keep changing the session. A removal changes what a reader sees three
// times: its intent is written, its generation is saved, its intent goes. The
// next removal of the session waits a whole retention. So the fourth check at
// the latest runs with nothing changing.
const scanAttempts = 4

// beforeScan runs between reading the removals and checking the bodies.
// Tests set it to run a removal there.
var beforeScan func()

// removals is what a reader sees of a session's removals at one moment: the
// removal in progress, by its generation and the newest sequence it removes,
// or otherwise the generation of the last one that deleted files.
type removals struct {
	generation int
	inProgress bool
	through    uint64
}

// removalOf reads the generation before the intent, as RemovalGeneration
// does. A removal writes its intent first, its generation before it deletes,
// and deletes its intent last, and each removal has its own generation. So
// when a deletion falls between two reads, the later one sees a removal, or a
// generation, the earlier did not.
//
// A read that finds an intent and then cannot load it saw a removal end
// between the two, and reads again: that view names no removal at all.
func removalOf(z *storage.Zone, session string) (removals, error) {
	for {
		generation, inProgress, err := storage.RemovalGeneration(z, session)
		if err != nil {
			return removals{}, err
		}
		if betweenReads != nil {
			betweenReads()
		}
		intent, err := storage.LoadRemoving(z.RemovingPath(session))
		if err != nil {
			return removals{}, err
		}
		if intent != nil {
			return removals{generation: intent.Generation, inProgress: true, through: intent.Through}, nil
		}
		if !inProgress {
			return removals{generation: generation}, nil
		}
	}
}

// betweenReads runs between reading a session's generation and loading its
// intent. Tests set it to end a removal there.
var betweenReads func()
