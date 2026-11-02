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

package kiro

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// realDir is a fresh directory with its links resolved, as the storage root
// is before a pass compares with it.
func realDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// One workspace inside the storage root keeps a session out, whatever else
// it worked in; whole path components are compared; a session with no
// workspace path is outside.
func TestASessionThatWorkedInsideTheRootIsAnEvaluators(t *testing.T) {
	base := realDir(t)
	root := filepath.Join(base, "data")
	outsideDir := filepath.Join(base, "data-project")
	for _, d := range []string{root, outsideDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	stage := filepath.Join(root, "s1", "judgments", "j000001", "work", "2-targets", "c001", "a1")
	for _, tc := range []struct {
		name       string
		workspaces []string
		want       origin
	}{
		{"the root itself", []string{root}, originInside},
		{"a stage directory that is gone", []string{stage}, originInside},
		{"one inside, one outside", []string{outsideDir, stage}, originInside},
		{"a sibling whose name starts like the root", []string{outsideDir}, originOutside},
		{"no workspace", nil, originOutside},
		{"a path not absolute here", []string{"data/s1"}, originOutside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := originOf(Session{ID: "s", Workspaces: tc.workspaces}, root); got != tc.want {
				t.Fatalf("origin %d, want %d", got, tc.want)
			}
		})
	}
}

// Links are followed on both sides: a root reached through a link, and a
// workspace named through one, compare as the directories they are, even
// when the workspace is gone; a link that does not resolve is no answer.
func TestTheGuardFollowsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a symbolic link needs a privilege on Windows")
	}
	base := realDir(t)
	target := filepath.Join(base, "real-root")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatal(err)
	}
	root, err := ownRoot(alias)
	if err != nil {
		t.Fatal(err)
	}
	if root != target {
		t.Fatalf("the root resolves to %s, not %s", root, target)
	}
	// Kiro recorded the stage directory under the alias, and the
	// directory went with its session.
	gone := filepath.Join(alias, "s1", "judgments", "j000001", "work", "2-targets")
	if got := originOf(Session{Workspaces: []string{gone}}, root); got != originInside {
		t.Fatalf("a removed stage directory under a linked root: %d", got)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if got := originOf(Session{Workspaces: []string{filepath.Join(dangling, "x")}}, root); got != originUnknown {
		t.Fatalf("a link that does not resolve: %d", got)
	}
}
