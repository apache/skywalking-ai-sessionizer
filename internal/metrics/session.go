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

package metrics

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// SpoolName is the name of the spool file the deriver writes for one landed
// file: the session, then the landed file's sequence, then the source.
// SpoolOwner reads it back.
func SpoolName(session string, seq uint64) string {
	return fmt.Sprintf("metrics-%s-%06d-%s.pb", session, seq, SourceLocal)
}

// SpoolOwner returns the session a spool file the deriver wrote belongs to.
//
// The name is read from the right. The sequence is the last dash-separated
// part before the source, and it has at least six digits. Everything before
// it is the session. A pattern such as metrics-<session>-* is never used,
// because one session id can be another followed by a dash and digits, and
// the other's files would then be taken for its own. A request the receiver
// adapter put in the spool names no session, so no session owns it.
func SpoolOwner(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, "metrics-")
	if !ok {
		return "", false
	}
	rest, ok = strings.CutSuffix(rest, "-"+SourceLocal+".pb")
	if !ok {
		return "", false
	}
	i := strings.LastIndexByte(rest, '-')
	if i <= 0 {
		return "", false
	}
	seq := rest[i+1:]
	if len(seq) < 6 {
		return "", false
	}
	for _, c := range seq {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return rest[:i], true
}

// Derived is what metrics.state records as derived: every landed file the
// deriver has read, by its path under the root, with the SHA-256 it had.
type Derived struct{ files map[string]string }

// LoadDerived reads metrics.state of a root once. A root with none has
// derived nothing.
func LoadDerived(z *storage.Zone) (*Derived, error) {
	s, _, err := loadState(filepath.Join(storage.NewSpool(z).Dir(), StateFile))
	if err != nil {
		return nil, err
	}
	return &Derived{files: s.Derived}, nil
}

// Has reports whether the landed file at rel, its path under the root with
// forward slashes, was derived with the digest given.
func (d *Derived) Has(rel, digest string) bool {
	got, ok := d.files[rel]
	return ok && got == digest
}

// DerivedAll reports whether every landed file of a session was derived with
// the digest it has now. files may be nil, and then the session's landed
// files are listed. It reads metrics.state on every call, so a caller that
// asks about many sessions calls LoadDerived once and uses Has.
func DerivedAll(z *storage.Zone, session string, files []storage.LandedFile) (bool, error) {
	if files == nil {
		var err error
		if files, err = storage.LandedFiles(z, session); err != nil {
			return false, err
		}
	}
	d, err := LoadDerived(z)
	if err != nil {
		return false, err
	}
	for _, lf := range files {
		rel, err := filepath.Rel(z.Root(), lf.Path)
		if err != nil {
			return false, err
		}
		digest, err := storage.FileDigest(lf.Path)
		if err != nil {
			return false, err
		}
		if !d.Has(filepath.ToSlash(rel), digest) {
			return false, nil
		}
	}
	return true, nil
}

// Forget drops what metrics.state says about sessions that were removed:
// every derived entry under <session>/ whose landed file is gone, and the
// session's counted calls and series once no derived entry of it is left.
//
// An entry is dropped only when its file is gone, so no file is derived twice
// for it. A session with a file left keeps its counted calls, so a call cut
// across two files is never counted twice. The deriver makes a session's
// state only for a session it lists, and it never lists a session whose
// directory is gone, so the state is not made again. The caller makes sure
// no deriver runs over the root at the same time. A root with no
// metrics.state is left without one.
func Forget(z *storage.Zone, sessions []string, now time.Time) error {
	path := filepath.Join(storage.NewSpool(z).Dir(), StateFile)
	s, first, err := loadState(path)
	if err != nil {
		return err
	}
	if first {
		return nil
	}
	changed := false
	for _, id := range sessions {
		if id == "" {
			continue
		}
		prefix := id + "/"
		left := false
		for rel := range s.Derived {
			if !strings.HasPrefix(rel, prefix) {
				continue
			}
			_, err := os.Lstat(filepath.Join(z.Root(), filepath.FromSlash(rel)))
			if errors.Is(err, fs.ErrNotExist) {
				delete(s.Derived, rel)
				changed = true
				continue
			}
			left = true
		}
		if _, ok := s.Sessions[id]; ok && !left {
			delete(s.Sessions, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.save(path, now)
}
