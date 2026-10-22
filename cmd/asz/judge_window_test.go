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

package main

import (
	"slices"
	"testing"
)

// A window's times are read in the forms a person types, and passed to the
// navigation server so it reads the same instant back.
func TestAWindowIsReadAndPassedOn(t *testing.T) {
	for _, s := range []string{"2026-10-20T10:00:00+08:00", "2026-10-20T10:00", "2026-10-20 10:00", "2026-10-20"} {
		w, err := windowOf(s, "", "", "")
		if err != nil || w.From.IsZero() {
			t.Fatalf("%q: %v", s, err)
		}
		back, err := windowOf(windowArgs(w)[1], "", "", "")
		if err != nil || !back.From.Equal(w.From) {
			t.Fatalf("%q came back as %v: %v", s, back.From, err)
		}
	}
	if _, err := windowOf("yesterday", "", "", ""); err == nil {
		t.Fatal("a time that is no time was read")
	}
	if _, err := windowOf("2026-10-21", "2026-10-20", "", ""); err == nil {
		t.Fatal("a window that ends before it starts was read")
	}
	w, _ := windowOf("", "", "talk/main/a", "talk/main/b")
	if got := windowArgs(w); !slices.Equal(got, []string{"-from-id", "talk/main/a", "-to-id", "talk/main/b"}) {
		t.Fatalf("ids passed on as %v", got)
	}
}
