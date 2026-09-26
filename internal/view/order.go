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
	"cmp"
	"sort"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// A point is where an item of the document happened: the lane its record
// landed in, a stream or a workflow run, the record's position there, and the
// time the record carries.
//
// A position orders records inside one lane only. The sequence is the order
// files landed, and a child's file can land before its parent's, so across
// lanes it is time that orders them.
type point struct {
	lane  string
	seq   uint64
	row   uint64
	block int   // -1 when the item stands on the whole record
	at    int64 // 0 when the record carries no time
	ok    bool  // false when the item has no landed position at all
}

// pointOf is where a reference sits.
func (c *Conversation) pointOf(r *sessionflow.Ref) point {
	if r == nil || r.Seq == 0 {
		return point{}
	}
	block := -1
	if r.Block != nil {
		block = *r.Block
	}
	return point{lane: c.lanes[r.Seq], seq: r.Seq, row: r.Row, block: block, at: c.at[[2]uint64{r.Seq, r.Row}], ok: true}
}

// earliestOf is where an item that several records support, such as a
// relation, happened: at the earliest of them by the same rule that orders
// items, the first position inside each lane and the earliest time across
// lanes.
func (c *Conversation) earliestOf(refs []sessionflow.Ref) point {
	first := map[string]point{}
	for i := range refs {
		if p := c.pointOf(&refs[i]); p.ok {
			if q, seen := first[p.lane]; !seen || comparePositions(p, q) < 0 {
				first[p.lane] = p
			}
		}
	}
	var out point
	for _, p := range first {
		if !out.ok || earlier(p, out) {
			out = p
		}
	}
	return out
}

// relationPoint is where a relation happened: at the earliest record that
// supports it.
func (c *Conversation) relationPoint(r *sessionflow.Relation) point {
	if r == nil {
		return point{}
	}
	return c.earliestOf(r.Evidence)
}

// relationTie decides between relations that one record supports.
func relationTie(a, b *sessionflow.Relation) bool { return a.ID < b.ID }

// comparePositions orders two points by position alone, which is meaningful
// inside one lane.
func comparePositions(a, b point) int {
	if c := cmp.Compare(a.seq, b.seq); c != 0 {
		return c
	}
	if c := cmp.Compare(a.row, b.row); c != 0 {
		return c
	}
	return cmp.Compare(a.block, b.block)
}

// inOrder sorts items the way they happened: by position inside each lane,
// and across lanes by time.
//
// It is a merge, not one comparison. Each lane's items are put in position
// order; then the lanes are merged by taking, each time, the lane whose next
// item is earliest, a timed item before an untimed one. A single comparator
// that decides some pairs by position and others by time is not transitive,
// and a sort over it is undefined, as the talk order found. Two lanes whose
// next items share a time are decided by position, which differs between
// lanes, so the result is the same on every read.
//
// tie decides between items at the very same position, such as two relations
// one record supports; nothing else tells them apart. Items with no position
// at all come last, in the order tie gives.
func inOrder[T any](items []T, pointOf func(T) point, tie func(a, b T) bool) []T {
	byLane := map[string][]T{}
	var lanes []string
	var nowhere []T
	for _, it := range items {
		p := pointOf(it)
		if !p.ok {
			nowhere = append(nowhere, it)
			continue
		}
		if _, ok := byLane[p.lane]; !ok {
			lanes = append(lanes, p.lane)
		}
		byLane[p.lane] = append(byLane[p.lane], it)
	}
	for _, lane := range lanes {
		list := byLane[lane]
		sort.SliceStable(list, func(i, j int) bool {
			if c := comparePositions(pointOf(list[i]), pointOf(list[j])); c != 0 {
				return c < 0
			}
			return tie(list[i], list[j])
		})
	}
	out := make([]T, 0, len(items))
	next := make(map[string]int, len(lanes))
	for len(out) < len(items)-len(nowhere) {
		best, bestAt := -1, point{}
		for i, lane := range lanes {
			if next[lane] >= len(byLane[lane]) {
				continue
			}
			if p := pointOf(byLane[lane][next[lane]]); best < 0 || earlier(p, bestAt) {
				best, bestAt = i, p
			}
		}
		lane := lanes[best]
		out = append(out, byLane[lane][next[lane]])
		next[lane]++
	}
	sort.SliceStable(nowhere, func(i, j int) bool { return tie(nowhere[i], nowhere[j]) })
	return append(out, nowhere...)
}

// earlier decides between the next items of two lanes: a timed one before an
// untimed one, then the earlier time, then the earlier position.
func earlier(a, b point) bool {
	switch {
	case (a.at != 0) != (b.at != 0):
		return a.at != 0
	case a.at != b.at:
		return a.at < b.at
	}
	return comparePositions(a, b) < 0
}
