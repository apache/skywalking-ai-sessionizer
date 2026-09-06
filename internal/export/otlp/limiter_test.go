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

package otlp

import (
	"testing"
	"time"
)

// The bucket holds a minute's budget, starts full, refills continuously,
// and a request larger than the budget waits for a full bucket and
// empties it.
func TestLimiterPacesByTheMinute(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var slept []time.Duration
	l := newLimiter(6000, func() time.Time { return now }, func(d time.Duration) { slept = append(slept, d); now = now.Add(d) })

	if w := l.take(6000); w != 0 {
		t.Fatalf("a full bucket must take a minute's worth at once, waited %s", w)
	}
	if w := l.take(3000); w != 30*time.Second {
		t.Fatalf("half a minute's worth on an empty bucket must wait 30s, waited %s", w)
	}
	if w := l.take(9000); w != time.Minute {
		t.Fatalf("more than a minute's worth must wait for a full bucket, waited %s", w)
	}
	if l.tokens != 0 {
		t.Fatalf("an oversize request must leave the bucket empty, holds %v", l.tokens)
	}
	now = now.Add(30 * time.Second)
	l.drain()
	if w := l.take(100); w != time.Second {
		t.Fatalf("after a drain 100 bytes at 100 a second must wait 1s, waited %s", w)
	}
	if len(slept) != 3 {
		t.Fatalf("waited %d times: %v", len(slept), slept)
	}

	none := newLimiter(0, func() time.Time { return now }, func(time.Duration) { t.Fatal("no limit must never wait") })
	if w := none.take(1 << 30); w != 0 {
		t.Fatalf("no limit waited %s", w)
	}
	var nilLimiter *limiter
	if w := nilLimiter.take(1); w != 0 {
		t.Fatal("a nil limiter must not wait")
	}
}
