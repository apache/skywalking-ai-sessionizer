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

package tests_test

import (
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// checkLandedKinds reads the header of every landed file and skeleton a
// scenario's roots hold, in every format, and requires the kind its name
// says. Provider verification and the pull choose files by that name.
func checkLandedKinds(t *testing.T, out string) {
	t.Helper()
	count := 0
	err := filepath.WalkDir(out, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		name := d.Name()
		if filepath.Ext(name) != sessiondata.Ext && filepath.Ext(name) != sessiondata.SkeletonExt {
			return nil
		}
		want, ok := storage.LandedKind(name)
		if !ok {
			t.Errorf("%s: the name says no kind", path)
			return nil
		}
		f, err := sessiondata.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r, err := sessiondata.NewReader(f)
		if err != nil {
			return err
		}
		if got := r.Header().Kind; got != want {
			t.Errorf("%s: header kind %q, name says %q", path, got, want)
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatalf("no landed files under %s", out)
	}
}
