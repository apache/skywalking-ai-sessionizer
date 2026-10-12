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
	"io"
	"os"
	"path/filepath"
	"testing"
)

// failSync makes every directory sync fail, as a disk that reports an error
// does, and collects what is reported, until the test ends.
func failSync(t *testing.T) *[]string {
	t.Helper()
	was, warn := syncDir, syncWarning
	var said []string
	syncDir = func(string) error { return errors.New("input/output error") }
	syncWarning = func(dir string, _ error) { said = append(said, dir) }
	t.Cleanup(func() { syncDir, syncWarning = was, warn })
	return &said
}

func write(text string) func(io.Writer) error {
	return func(w io.Writer) error { _, err := io.WriteString(w, text); return err }
}

// A write whose directory cannot be synced succeeds: the file is in place,
// which is all a process crash needs, and another process may have read it
// already. The failure is reported, once for the directory, because a power
// loss may lose the newest names there.
func TestAWriteSucceedsAndSaysSoWhenItsDirectoryCannotBeSynced(t *testing.T) {
	said := failSync(t)
	dir := t.TempDir()
	for i, name := range []string{"transcript-x-000001.sd", "transcript-x-000002.sd"} {
		path := filepath.Join(dir, name)
		if err := WriteAtomic(path, PermLanded, write("landed")); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
		if b, err := os.ReadFile(path); err != nil || string(b) != "landed" {
			t.Fatalf("write %d left %q, %v", i+1, b, err)
		}
	}
	if err := WriteExclusive(filepath.Join(dir, "r000001-abc.sf"), PermLanded, write("round")); err != nil {
		t.Fatalf("the exclusive write: %v", err)
	}
	if len(*said) != 1 || (*said)[0] != dir {
		t.Fatalf("reported %v, want the directory once", *said)
	}
}
