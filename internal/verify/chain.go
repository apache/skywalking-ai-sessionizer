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
	"path/filepath"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// ChainReport is the result of checking one conversation's chain against
// the landed files its rounds consumed.
type ChainReport struct {
	Conversation string
	Session      string
	Rounds       []*RoundReport
	Problems     int
}

// RoundReport is one round as the check found it. A round that could not
// be opened has OpenError and nothing else.
type RoundReport struct {
	Round  uint64
	Path   string
	Header sessionflow.Header
	Digest string

	OpenError string
	// MissingRound is set when a round number is skipped before this one:
	// the round that should have come first is not on disk.
	MissingRound uint64
	// BrokenLink says the round names a previous digest that is not the
	// digest of the round before it.
	BrokenLink bool
	// SeqGap says the round starts past where the round before ended.
	SeqGap bool
	// Missing lists the landed sequences the round consumed that are no
	// longer on disk.
	Missing []uint64
	// DigestMismatch says the landed files on disk do not digest to what
	// the round consumed, so at least one was changed.
	DigestMismatch bool

	Problems []string
}

// OK reports a round that verified against the chain and the landed files.
func (r *RoundReport) OK() bool { return len(r.Problems) == 0 }

// Damaged reports a round whose evidence is changed rather than missing: a
// broken link, a digest that does not match, or a file that will not open.
func (r *RoundReport) Damaged() bool {
	return r.OpenError != "" || r.BrokenLink || r.DigestMismatch
}

// OK reports whether every round verified.
func (r *ChainReport) OK() bool { return r.Problems == 0 }

// Details returns every problem, in round order.
func (r *ChainReport) Details() []string {
	var out []string
	for _, rr := range r.Rounds {
		out = append(out, rr.Problems...)
	}
	return out
}

// LandedDigests reads the digest of every landed file of a session, by
// sequence: what a round's input digest was computed over.
func LandedDigests(z *storage.Zone, session string) (map[uint64]string, error) {
	files, err := storage.LandedFiles(z, session)
	if err != nil {
		return nil, err
	}
	out := map[uint64]string{}
	for _, lf := range files {
		d, err := storage.FileDigest(lf.Path)
		if err != nil {
			return nil, err
		}
		out[lf.Seq] = d
	}
	return out, nil
}

// Chain checks a conversation's rounds against each other and against the
// landed files: numbered from one, each naming the digest of the round
// before, each starting where the round before ended, and each bound to
// landed files that are still on disk and still digest to what the round
// consumed. A landed file that a round consumed and that is gone is
// reported here and nowhere else: the stream checks see only what exists,
// and a stream whose one file is gone has nothing left to check.
//
// digests may be passed by a caller that has read the landed files already;
// nil means read them, for the session the first round names.
func Chain(z *storage.Zone, conversation string, digests map[uint64]string) (*ChainReport, error) {
	chain := sessionflow.OpenChain(z.Root(), conversation)
	list, err := chain.List()
	if err != nil {
		return nil, err
	}
	rep := &ChainReport{Conversation: conversation, Session: conversation}
	var prevDigest, prevInput string
	var prevThrough, prevRound uint64
	for _, rf := range list {
		rr := &RoundReport{Round: rf.Round, Path: rf.Path}
		rep.Rounds = append(rep.Rounds, rr)
		r, err := chain.Open(rf.Path)
		if err != nil {
			rr.OpenError = err.Error()
			rr.Problems = append(rr.Problems, fmt.Sprintf("round %d: %v", rf.Round, err))
			rep.Problems += len(rr.Problems)
			continue
		}
		h := r.Header
		rr.Header, rr.Digest = h, r.Commit.Digest
		if h.Session != "" {
			rep.Session = h.Session
		}
		if digests == nil {
			digests, err = LandedDigests(z, rep.Session)
			if err != nil {
				return nil, err
			}
		}
		if h.Round != prevRound+1 {
			// A missing round: this one cannot link to what is not there,
			// and nothing after it folds.
			rr.MissingRound = prevRound + 1
			rr.Problems = append(rr.Problems, fmt.Sprintf("round %d is missing before round %d", prevRound+1, h.Round))
		} else if h.Previous != prevDigest {
			rr.BrokenLink = true
			rr.Problems = append(rr.Problems, fmt.Sprintf("round %d names previous %s, the round before is %s", h.Round, firstN(h.Previous, 12), firstN(prevDigest, 12)))
		}
		if h.FromSeq != prevThrough+1 {
			rr.SeqGap = true
			rr.Problems = append(rr.Problems, fmt.Sprintf("round %d starts at seq %d, the round before ended at %d", h.Round, h.FromSeq, prevThrough))
		}
		var added []string
		for seq := h.FromSeq; seq <= h.ThroughSeq; seq++ {
			d, have := digests[seq]
			if !have {
				rr.Missing = append(rr.Missing, seq)
				rr.Problems = append(rr.Problems, fmt.Sprintf("round %d: landed file seq %d is missing", h.Round, seq))
				continue
			}
			added = append(added, d)
		}
		if len(rr.Problems) == 0 && sessionflow.ChainInputDigest(prevInput, added) != h.InputDigest {
			rr.DigestMismatch = true
			rr.Problems = append(rr.Problems, fmt.Sprintf("round %d: the input digest does not match the landed files", h.Round))
		}
		rep.Problems += len(rr.Problems)
		prevDigest, prevInput, prevThrough, prevRound = r.Commit.Digest, h.InputDigest, h.ThroughSeq, h.Round
	}
	return rep, nil
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Base is the file name of a path, for a report line.
func Base(path string) string { return filepath.Base(path) }
