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
	"strings"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestParseTag(t *testing.T) {
	for _, tc := range []struct {
		name string
		tag  string
		want time.Time
		err  string
	}{
		{
			name: "nanoseconds",
			tag:  "20261025T103200.123456789Z",
			want: time.Date(2026, 10, 25, 10, 32, 0, 123456789, time.UTC),
		},
		{
			name: "whole second",
			tag:  "20261025T103200.000000000Z",
			want: time.Date(2026, 10, 25, 10, 32, 0, 0, time.UTC),
		},
		{
			name: "leap day",
			tag:  "20240229T235959.999999999Z",
			want: time.Date(2024, 2, 29, 23, 59, 59, 999999999, time.UTC),
		},
		{
			name: "year zero",
			tag:  "00000101T000000.000000000Z",
			want: time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "last supported instant",
			tag:  "99991231T235959.999999999Z",
			want: time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC),
		},
		{name: "empty", err: "parse publication tag"},
		{name: "missing fraction", tag: "20261025T103200Z", err: "parse publication tag"},
		{name: "short fraction", tag: "20261025T103200.123Z", err: "parse publication tag"},
		{name: "long fraction", tag: "20261025T103200.1234567890Z", err: "parse publication tag"},
		{name: "comma fraction", tag: "20261025T103200,123456789Z", err: "not canonical"},
		{name: "zero offset", tag: "20261025T103200.123456789+00:00", err: "parse publication tag"},
		{name: "other offset", tag: "20261025T183200.123456789+08:00", err: "parse publication tag"},
		{name: "zone name", tag: "20261025T103200.123456789UTC", err: "parse publication tag"},
		{name: "missing zone", tag: "20261025T103200.123456789", err: "parse publication tag"},
		{name: "lowercase zone", tag: "20261025T103200.123456789z", err: "parse publication tag"},
		{name: "lowercase separator", tag: "20261025t103200.123456789Z", err: "parse publication tag"},
		{name: "RFC 3339", tag: "2026-10-25T10:32:00.123456789Z", err: "parse publication tag"},
		{name: "invalid date", tag: "20260229T103200.123456789Z", err: "parse publication tag"},
		{name: "invalid month", tag: "20261325T103200.123456789Z", err: "parse publication tag"},
		{name: "invalid hour", tag: "20261025T243200.123456789Z", err: "parse publication tag"},
		{name: "leap second", tag: "20261025T103260.123456789Z", err: "parse publication tag"},
		{name: "leading space", tag: " 20261025T103200.123456789Z", err: "parse publication tag"},
		{name: "trailing space", tag: "20261025T103200.123456789Z ", err: "parse publication tag"},
		{name: "year overflow", tag: "100000101T000000.000000000Z", err: "parse publication tag"},
		{name: "negative year", tag: "-00010101T000000.000000000Z", err: "parse publication tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := storage.ParseTag(tc.tag)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("ParseTag(%q) error = %v, want %q", tc.tag, err, tc.err)
				}
				if !got.IsZero() {
					t.Fatalf("ParseTag(%q) returned %v after refusing the tag", tc.tag, got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("ParseTag(%q) = %v, want %v", tc.tag, got, tc.want)
			}
			if roundTrip := storage.Stamp(got); roundTrip != tc.tag {
				t.Fatalf("Stamp(ParseTag(%q)) = %q", tc.tag, roundTrip)
			}
		})
	}
}

func TestNextTag(t *testing.T) {
	now := time.Date(2026, 10, 25, 10, 32, 0, 123456789, time.UTC)
	for _, tc := range []struct {
		name     string
		now      time.Time
		greatest string
		want     string
		err      string
	}{
		{name: "empty history", now: now, want: "20261025T103200.123456789Z"},
		{name: "zero clock", want: "00010101T000000.000000000Z"},
		{
			name: "clock in another zone",
			now:  time.Date(2026, 10, 25, 18, 32, 0, 123456789, time.FixedZone("east", 8*60*60)),
			want: "20261025T103200.123456789Z",
		},
		{
			name: "clock moves forward", now: now,
			greatest: "20261025T103159.123456789Z", want: "20261025T103200.123456789Z",
		},
		{
			name: "equal readings", now: now,
			greatest: "20261025T103200.123456789Z", want: "20261025T103200.123456790Z",
		},
		{
			name: "clock steps back", now: now.Add(-time.Hour),
			greatest: "20261025T103200.123456789Z", want: "20261025T103200.123456790Z",
		},
		{
			name: "clock advances by two nanoseconds", now: now.Add(2 * time.Nanosecond),
			greatest: "20261025T103200.123456789Z", want: "20261025T103200.123456791Z",
		},
		{
			name: "clock reaches next tag", now: now.Add(time.Nanosecond),
			greatest: "20261025T103200.123456789Z", want: "20261025T103200.123456790Z",
		},
		{
			name: "new year", now: now,
			greatest: "20261231T235959.999999999Z", want: "20270101T000000.000000000Z",
		},
		{
			name: "last supported tag", now: now,
			greatest: "99991231T235959.999999998Z", want: "99991231T235959.999999999Z",
		},
		{
			name: "malformed history with a later clock", now: now,
			greatest: "20251025T103200Z", err: "parse publication tag",
		},
		{
			name: "noncanonical history", now: now,
			greatest: "20251025T103200,123456789Z", err: "not canonical",
		},
		{
			name: "history overflows the year", now: now,
			greatest: "99991231T235959.999999999Z", err: "next publication tag",
		},
		{
			name: "clock overflows the year",
			now:  time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), err: "next publication tag",
		},
		{
			name: "clock precedes year zero",
			now:  time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC), err: "next publication tag",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := storage.NextTag(tc.now, tc.greatest)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("NextTag(%v, %q) error = %v, want %q", tc.now, tc.greatest, err, tc.err)
				}
				if got != "" {
					t.Fatalf("NextTag returned %q after refusing publication", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("NextTag(%v, %q) = %q, want %q", tc.now, tc.greatest, got, tc.want)
			}
		})
	}
}
