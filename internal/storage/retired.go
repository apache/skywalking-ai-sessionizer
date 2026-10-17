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

package storage

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RetiredDir holds one empty file for each retired session, named by its id.
//
// A retired session is never written again: nothing of it is landed,
// indexed, parsed, derived or sent, and no directory of it is made. Its
// receiver may still hold its first chain, for longer than this root does,
// and a second chain under the same id would show the receiver two
// conversations as one. The marker is what keeps it out, so it is never
// removed, and it holds nothing but its name: nothing reads more.
const RetiredDir = "_retired"

// RetiredPath is where a session's retired marker is.
func (z *Zone) RetiredPath(session string) string {
	return filepath.Join(z.root, RetiredDir, session)
}

// Retired reports whether a session is retired. Every writer that can make a
// session's directory, its chain or any state of it asks before it does,
// since taking a session's lock makes its directory. An error other than the
// marker not being there is returned, and the caller writes nothing of the
// session that pass: a marker it cannot see may still be there.
func Retired(z *Zone, session string) (bool, error) {
	_, err := os.Lstat(z.RetiredPath(session))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// RetiredSessions lists the retired sessions of a root, by their markers. A
// name that cannot be a session's, such as a marker's temporary file, is not
// one.
func RetiredSessions(z *Zone) ([]string, error) {
	items, err := os.ReadDir(filepath.Join(z.root, RetiredDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, it := range items {
		name := it.Name()
		if it.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, name)
	}
	return out, nil
}
