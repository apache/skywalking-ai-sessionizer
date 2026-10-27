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

package storage_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// A bounded listing takes every entry up to its bound, in name order, and
// refuses one more, in batches as large as it reads.
func TestABoundedListingStopsPastItsBound(t *testing.T) {
	dir := t.TempDir()
	const n = 2500
	for i := range n {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%05d", n-i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	all, err := storage.ReadDirAtMost(dir, n)
	if err != nil || len(all) != n || all[0].Name() != "f00001" {
		t.Fatalf("%d entries: %v", len(all), err)
	}
	if _, err := storage.ReadDirAtMost(dir, n-1); !errors.Is(err, storage.ErrTooMany) {
		t.Fatalf("one past the bound: %v", err)
	}
	if every, err := storage.ReadDirAtMost(dir, -1); err != nil || len(every) != n {
		t.Fatalf("no bound: %d %v", len(every), err)
	}
}

// A bounded listing of landed files counts landed files only: a session of
// n of them is listed under a bound of n however many temporaries lie
// beside them, and refused under n-1.
func TestABoundedLandedListingCountsLandedFilesOnly(t *testing.T) {
	z := storage.NewZone(t.TempDir())
	var stream string
	for seq := range uint64(3) {
		p, _ := landOne(t, z, seq+1)
		stream = filepath.Dir(p)
	}
	for i := range 1100 {
		if err := os.WriteFile(filepath.Join(stream, fmt.Sprintf(".tmp-%d", i)), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := storage.LandedFilesAtMost(z, "s", 3)
	if err != nil || len(files) != 3 {
		t.Fatalf("%d files under a bound of 3: %v", len(files), err)
	}
	if _, err := storage.LandedFilesAtMost(z, "s", 2); !errors.Is(err, storage.ErrTooMany) {
		t.Fatalf("3 files under a bound of 2: %v", err)
	}
}
