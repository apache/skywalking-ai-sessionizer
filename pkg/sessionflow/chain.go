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

package sessionflow

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Chain is the on-disk round chain for one conversation.
//
// Layout:
//
//	<root>/_conversations/<conversation>/
//	  conversation.state                        the mutable head pointer
//	  rounds/r000001-<digest12>.sf       immutable, 0444
//	  rounds/r000002-<digest12>.sf
//
// Rounds are immutable and carry no wall-clock time, so that the same inputs
// reproduce the same bytes. Everything mutable or temporal - which round is
// current, when it was produced, where the next one starts - lives in
// conversation.state, outside the digests.
type Chain struct {
	dir string
	id  string
}

// OpenChain binds to the chain directory for a conversation.
func OpenChain(root, conversation string) *Chain {
	return &Chain{dir: filepath.Join(root, "_conversations", conversation), id: conversation}
}

// Dir reports the chain directory.
func (c *Chain) Dir() string { return c.dir }

// RoundsDir reports the directory holding the round files.
func (c *Chain) RoundsDir() string { return filepath.Join(c.dir, "rounds") }

// StatePath reports the chain state file.
func (c *Chain) StatePath() string { return filepath.Join(c.dir, "conversation.state") }

// roundName builds a round filename.
//
// The digest prefix is in the name so a round can be located by the digest its
// successor names, without opening every file in the directory.
func roundName(round uint64, digest string) string {
	return fmt.Sprintf("r%06d-%s.sf", round, firstN(digest, 12))
}

var roundNameRe = regexp.MustCompile(`^r(\d{6,})-([0-9a-f]{12})\.sf$`)

// State is the mutable head of a chain.
//
// Head and HeadDigest are the two values a new round depends on. ThroughSeq and
// InputDigest describe how far the landed evidence has been consumed, so a
// resumed parse knows where to start without folding the whole chain.
type State struct {
	Schema       int
	Conversation string
	Head         uint64
	HeadDigest   string
	ThroughSeq   uint64
	InputDigest  string
	Parser       string
	Policy       string
	UpdatedAt    string
}

// LoadState reads chain state, returning a fresh value if absent.
func (c *Chain) LoadState() (*State, error) {
	s := &State{Schema: 1, Conversation: c.id}
	f, err := os.Open(c.StatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return nil, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		switch key {
		case "schema":
			s.Schema, _ = strconv.Atoi(val)
		case "conversation":
			s.Conversation = val
		case "head":
			s.Head, _ = strconv.ParseUint(val, 10, 64)
		case "head_digest":
			s.HeadDigest = val
		case "through_seq":
			s.ThroughSeq, _ = strconv.ParseUint(val, 10, 64)
		case "input_digest":
			s.InputDigest = val
		case "parser":
			s.Parser = val
		case "policy":
			s.Policy = val
		case "updated_at":
			s.UpdatedAt = val
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("sessionflow: read chain state %s: %w", c.StatePath(), err)
	}
	return s, nil
}

// SaveState writes chain state atomically.
func (c *Chain) SaveState(s *State, now time.Time) error {
	s.UpdatedAt = now.UTC().Format(time.RFC3339Nano)
	return storage.WriteAtomic(c.StatePath(), storage.PermState, func(w io.Writer) error {
		bw := bufio.NewWriter(w)
		fmt.Fprintf(bw, "schema       %d\n", s.Schema)
		fmt.Fprintf(bw, "conversation %s\n", s.Conversation)
		fmt.Fprintf(bw, "head         %d\n", s.Head)
		fmt.Fprintf(bw, "head_digest  %s\n", s.HeadDigest)
		fmt.Fprintf(bw, "through_seq  %d\n", s.ThroughSeq)
		fmt.Fprintf(bw, "input_digest %s\n", s.InputDigest)
		fmt.Fprintf(bw, "parser       %s\n", s.Parser)
		fmt.Fprintf(bw, "policy       %s\n", s.Policy)
		fmt.Fprintf(bw, "updated_at   %s\n", s.UpdatedAt)
		return bw.Flush()
	})
}

// RoundFile names one round file on disk.
type RoundFile struct {
	Round  uint64
	Digest string // the 12-hex prefix carried in the name
	Path   string
}

// List returns the chain's round files in round order.
//
// The filesystem is the authority, not the state file: a crash between
// publishing a round and saving state leaves a round on disk that state does
// not mention, and recovery has to see it.
func (c *Chain) List() ([]RoundFile, error) {
	ents, err := os.ReadDir(c.RoundsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []RoundFile
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		m := roundNameRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, err := strconv.ParseUint(m[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, RoundFile{Round: n, Digest: m[2], Path: filepath.Join(c.RoundsDir(), e.Name())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Round < out[j].Round })
	return out, nil
}

// Publish writes a completed round.
//
// It refuses to replace an existing round: a round is the input to every later
// round's digest, so overwriting one would silently invalidate the rest of the
// chain. A collision means two producers derived the same round number, which
// is a condition to surface, not to resolve by picking a winner.
//
// Refusing a same-named file is not enough on its own, because a round's name
// carries its digest: two builders that both read round N would produce round
// N+1 files with DIFFERENT names and neither would collide. Guarding that is
// Lock's job, and Publish rechecks the head under it.
func (c *Chain) Publish(round uint64, digest string, data []byte) (string, error) {
	if have, err := c.Head(); err != nil {
		return "", err
	} else if have+1 != round {
		return "", fmt.Errorf("sessionflow: cannot publish round %d: the chain head is round %d", round, have)
	}
	path := filepath.Join(c.RoundsDir(), roundName(round, digest))
	err := storage.WriteExclusive(path, storage.PermLanded, func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// Lock takes exclusive ownership of the chain for the duration of a publish.
//
// Deciding the next round number and writing it is a read followed by a write.
// Without this, two builders both read round N and both write an N+1 - and
// because the digest is part of the filename, their files do not even collide.
// The chain would fork with no error anywhere.
func (c *Chain) Lock() (*storage.SessionLock, error) {
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return nil, err
	}
	return storage.LockChain(c.dir)
}

// Head returns the highest round number on disk, or 0 for an empty chain.
//
// The filesystem is the authority, not the state file: a crash between
// publishing a round and saving state leaves a round that state does not
// mention.
func (c *Chain) Head() (uint64, error) {
	files, err := c.List()
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, nil
	}
	return files[len(files)-1].Round, nil
}

// Open reads and verifies one round file.
func (c *Chain) Open(path string) (*Round, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r, err := Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return r, nil
}

// Verify walks the chain and checks that it is a chain.
//
// Three things must hold, and each catches a different failure:
//
//   - round numbers are 1..N with no gap - a missing round cannot be folded
//     over, and its successors' digests are unverifiable without it;
//   - each round names its predecessor's digest - this is what makes the chain
//     tamper-evident rather than merely ordered;
//   - each round's own digest matches its bytes - checked by Read.
//
// It also checks that the consumed landed range is contiguous across rounds:
// a gap there means evidence was skipped, which no digest would reveal.
func (c *Chain) Verify() ([]RoundFile, error) {
	files, err := c.List()
	if err != nil {
		return nil, err
	}
	var (
		prevDigest string
		prevSeq    uint64
	)
	for i, rf := range files {
		want := uint64(i + 1)
		if rf.Round != want {
			return files, fmt.Errorf("sessionflow: chain %s: expected round %d, found round %d", c.id, want, rf.Round)
		}
		r, err := c.Open(rf.Path)
		if err != nil {
			return files, err
		}
		if r.Header.Round != rf.Round {
			return files, fmt.Errorf("sessionflow: %s: header says round %d", filepath.Base(rf.Path), r.Header.Round)
		}
		if r.Header.Previous != prevDigest {
			return files, fmt.Errorf("sessionflow: round %d names previous %q, but round %d digests to %q",
				rf.Round, firstN(r.Header.Previous, 12), rf.Round-1, firstN(prevDigest, 12))
		}
		if i > 0 && r.Header.FromSeq != prevSeq+1 {
			return files, fmt.Errorf("sessionflow: round %d starts at seq %d, but round %d ended at %d: landed evidence was skipped",
				rf.Round, r.Header.FromSeq, rf.Round-1, prevSeq)
		}
		prevDigest = r.Commit.Digest
		prevSeq = r.Header.ThroughSeq
	}
	return files, nil
}
