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

//go:build linux || darwin

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// CI has no filesystem without both an exclusive rename and hard links, so
// the last resort is called directly. exFAT on macOS is such a filesystem.
func TestReservedInstallPublishesAndRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	to := filepath.Join(dir, "round.sf")
	first := filepath.Join(dir, ".tmp-first")
	if err := os.WriteFile(first, []byte("first"), PermLanded); err != nil {
		t.Fatal(err)
	}
	if err := installReserved(first, to); err != nil {
		t.Fatal(err)
	}
	second := filepath.Join(dir, ".tmp-second")
	if err := os.WriteFile(second, []byte("second"), PermLanded); err != nil {
		t.Fatal(err)
	}
	if err := installReserved(second, to); !os.IsExist(err) {
		t.Fatalf("an existing file was not refused: %v", err)
	}
	body, err := os.ReadFile(to)
	if err != nil || string(body) != "first" {
		t.Fatalf("published file = %q, %v", body, err)
	}
	info, err := os.Stat(to)
	if err != nil || info.Mode().Perm() != PermLanded {
		t.Fatalf("published mode = %v, %v; want %v", info.Mode().Perm(), err, PermLanded)
	}
	if _, err := os.Stat(first); !os.IsNotExist(err) {
		t.Fatalf("the installed temporary file is still there: %v", err)
	}
}
