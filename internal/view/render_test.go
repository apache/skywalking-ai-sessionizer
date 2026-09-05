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

package view

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// An observed time floors to the millisecond, as time.Time does, so a time
// before 1970 renders the same everywhere; an elapsed time divides toward
// zero.
func TestMillisFloorsTimesAndDurationsDivide(t *testing.T) {
	if got := Millis(-100_000); got != -1 {
		t.Fatalf("Millis(-100µs) = %d, want -1", got)
	}
	if got := Millis(1_500_000); got != 1 {
		t.Fatalf("Millis(1.5ms) = %d, want 1", got)
	}
	if got := Millis(0); got != 0 {
		t.Fatalf("Millis(0) = %d, want 0: unobserved", got)
	}
	if got := durationMillis(-100_000); got != 0 {
		t.Fatalf("durationMillis(-100µs) = %d, want 0", got)
	}
	if got := durationMillis(2_999_999); got != 2 {
		t.Fatalf("durationMillis(2.999ms) = %d, want 2", got)
	}
}

// A preview never ends in a broken character: it keeps the longest prefix
// of whole characters within the budget.
func TestClipKeepsWholeCharacters(t *testing.T) {
	text := strings.Repeat("界", 667) // 2,001 bytes
	got := clip(text)
	if !utf8.ValidString(got) {
		t.Fatal("the preview is not valid UTF-8")
	}
	if n := utf8.RuneCountInString(got); n != 666 || len(got) != 1998 {
		t.Fatalf("the preview holds %d characters in %d bytes, want 666 in 1998", n, len(got))
	}
	if short := "short"; clip(short) != short {
		t.Fatal("a short text is clipped")
	}
}
