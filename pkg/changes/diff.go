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

package changes

import (
	"bytes"
	"unicode/utf8"
)

// Context is how many unchanged lines a hunk shows on each side of a
// change. Three, as git shows.
const Context = 3

// MaxDiffLines bounds one side of a comparison. Past it the result is
// too_large rather than a diff that takes seconds to compute.
const MaxDiffLines = 200_000

// IsText reports whether bytes can be shown line by line: valid UTF-8 with
// no NUL byte. Anything else is binary and lands as path and hash only.
func IsText(b []byte) bool {
	return bytes.IndexByte(b, 0) < 0 && utf8.Valid(b)
}

// Text is one side of a comparison, split into lines.
type Text struct {
	Lines []string
	// NoNewlineAtEnd says the last line had no newline after it.
	NoNewlineAtEnd bool
}

// SplitLines splits text into lines without their newlines. A carriage
// return stays on its line, so CRLF and LF files compare as different
// lines, which is what they are. An empty text has no lines, and a text
// that ends with a newline has no empty line after it.
func SplitLines(b []byte) Text {
	if len(b) == 0 {
		return Text{}
	}
	parts := bytes.Split(b, []byte("\n"))
	t := Text{}
	if len(parts[len(parts)-1]) == 0 {
		parts = parts[:len(parts)-1]
	} else {
		t.NoNewlineAtEnd = true
	}
	t.Lines = make([]string, len(parts))
	for i, p := range parts {
		t.Lines[i] = string(p)
	}
	return t
}

// Diff compares two texts and returns unified hunks with Context lines of
// context, and the number of added and removed lines. ok is false when a
// side exceeds MaxDiffLines.
//
// The algorithm is Myers' shortest edit script, computed in linear space by
// bisection. Within a change block removed lines are listed before added
// ones, as a reader expects from a unified diff.
func Diff(before, after Text) (hunks []Hunk, additions, deletions int, ok bool) {
	a, b := before.Lines, after.Lines
	if len(a) > MaxDiffLines || len(b) > MaxDiffLines {
		return nil, 0, 0, false
	}
	ops := diffOps(a, b)
	hunks, additions, deletions = hunksOf(a, b, ops, Context)
	return hunks, additions, deletions, true
}

// opKind is one of equal, delete, insert.
type opKind uint8

const (
	opEqual opKind = iota
	opDelete
	opInsert
)

// op is a run of lines: n lines of kind k, starting at a[ai] and b[bi].
type op struct {
	k      opKind
	ai, bi int
	n      int
}

// diffOps returns the edit script as runs.
func diffOps(a, b []string) []op {
	var out []op
	diffRange(a, b, 0, 0, &out)
	return mergeOps(out)
}

// diffRange appends the ops for a and b, whose first lines sit at aOff and
// bOff in the original texts.
func diffRange(a, b []string, aOff, bOff int, out *[]op) {
	// Common prefix and suffix are equal runs; only the middle needs work.
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	if pre > 0 {
		*out = append(*out, op{opEqual, aOff, bOff, pre})
	}
	a, b = a[pre:], b[pre:]
	aOff, bOff = aOff+pre, bOff+pre
	suf := 0
	for suf < len(a) && suf < len(b) && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	mid := func() {
		a2, b2 := a[:len(a)-suf], b[:len(b)-suf]
		switch {
		case len(a2) == 0 && len(b2) == 0:
		case len(a2) == 0:
			*out = append(*out, op{opInsert, aOff, bOff, len(b2)})
		case len(b2) == 0:
			*out = append(*out, op{opDelete, aOff, bOff, len(a2)})
		default:
			bisect(a2, b2, aOff, bOff, out)
		}
	}
	mid()
	if suf > 0 {
		*out = append(*out, op{opEqual, aOff + len(a) - suf, bOff + len(b) - suf, suf})
	}
}

// bisect finds the middle of the shortest edit path from both ends at once,
// which needs memory proportional to the texts rather than to their
// product, and recurses on the two halves.
func bisect(a, b []string, aOff, bOff int, out *[]op) {
	n, m := len(a), len(b)
	maxD := (n + m + 1) / 2
	vOff := maxD
	// Two spare slots: the seed is written one past the offset, and with one
	// line on each side that is the vector's whole length.
	vLen := 2*maxD + 2
	v1 := make([]int, vLen)
	v2 := make([]int, vLen)
	for i := range v1 {
		v1[i], v2[i] = -1, -1
	}
	v1[vOff+1], v2[vOff+1] = 0, 0
	delta := n - m
	front := delta%2 != 0
	k1start, k1end, k2start, k2end := 0, 0, 0, 0
	for d := 0; d < maxD; d++ {
		for k1 := -d + k1start; k1 <= d-k1end; k1 += 2 {
			k1o := vOff + k1
			var x1 int
			if k1 == -d || (k1 != d && v1[k1o-1] < v1[k1o+1]) {
				x1 = v1[k1o+1]
			} else {
				x1 = v1[k1o-1] + 1
			}
			y1 := x1 - k1
			for x1 < n && y1 < m && a[x1] == b[y1] {
				x1++
				y1++
			}
			v1[k1o] = x1
			switch {
			case x1 > n:
				k1end += 2
			case y1 > m:
				k1start += 2
			case front:
				k2o := vOff + delta - k1
				if k2o >= 0 && k2o < vLen && v2[k2o] != -1 {
					if x2 := n - v2[k2o]; x1 >= x2 {
						split(a, b, aOff, bOff, x1, y1, out)
						return
					}
				}
			}
		}
		for k2 := -d + k2start; k2 <= d-k2end; k2 += 2 {
			k2o := vOff + k2
			var x2 int
			if k2 == -d || (k2 != d && v2[k2o-1] < v2[k2o+1]) {
				x2 = v2[k2o+1]
			} else {
				x2 = v2[k2o-1] + 1
			}
			y2 := x2 - k2
			for x2 < n && y2 < m && a[n-x2-1] == b[m-y2-1] {
				x2++
				y2++
			}
			v2[k2o] = x2
			switch {
			case x2 > n:
				k2end += 2
			case y2 > m:
				k2start += 2
			case !front:
				k1o := vOff + delta - k2
				if k1o >= 0 && k1o < vLen && v1[k1o] != -1 {
					x1 := v1[k1o]
					y1 := vOff + x1 - k1o
					if x1 >= n-x2 {
						split(a, b, aOff, bOff, x1, y1, out)
						return
					}
				}
			}
		}
	}
	// No line in common: everything removed, then everything added.
	*out = append(*out, op{opDelete, aOff, bOff, n}, op{opInsert, aOff, bOff, m})
}

// split recurses on the two halves either side of a middle point.
func split(a, b []string, aOff, bOff, x, y int, out *[]op) {
	diffRange(a[:x], b[:y], aOff, bOff, out)
	diffRange(a[x:], b[y:], aOff+x, bOff+y, out)
}

// mergeOps joins adjacent runs of the same kind.
func mergeOps(ops []op) []op {
	var out []op
	for _, o := range ops {
		if o.n == 0 {
			continue
		}
		if len(out) > 0 && out[len(out)-1].k == o.k {
			out[len(out)-1].n += o.n
			continue
		}
		out = append(out, o)
	}
	return out
}

// hunksOf renders runs as unified hunks: each change block with up to
// ctx lines of context on each side, blocks whose context would touch or
// overlap merged into one hunk, removed lines before added lines within a
// block.
func hunksOf(a, b []string, ops []op, ctx int) (hunks []Hunk, additions, deletions int) {
	// Expand runs into per-line entries, which keeps the hunk cutting simple.
	type line struct {
		k      opKind
		ai, bi int
	}
	var lines []line
	for _, o := range ops {
		for i := 0; i < o.n; i++ {
			switch o.k {
			case opEqual:
				lines = append(lines, line{opEqual, o.ai + i, o.bi + i})
			case opDelete:
				lines = append(lines, line{opDelete, o.ai + i, o.bi})
				deletions++
			case opInsert:
				lines = append(lines, line{opInsert, o.ai, o.bi + i})
				additions++
			}
		}
	}
	// Mark which lines a hunk shows: every changed line and ctx equal
	// lines around each.
	show := make([]bool, len(lines))
	for i, l := range lines {
		if l.k == opEqual {
			continue
		}
		for j := max(0, i-ctx); j <= min(len(lines)-1, i+ctx); j++ {
			show[j] = true
		}
	}
	for i := 0; i < len(lines); {
		if !show[i] {
			i++
			continue
		}
		j := i
		for j < len(lines) && show[j] {
			j++
		}
		h := Hunk{Lines: []string{}}
		first := true
		var pendingAdds []string
		flush := func() {
			h.Lines = append(h.Lines, pendingAdds...)
			pendingAdds = nil
		}
		for _, l := range lines[i:j] {
			if first {
				h.OldStart, h.NewStart = l.ai+1, l.bi+1
				first = false
			}
			switch l.k {
			case opEqual:
				flush()
				h.Lines = append(h.Lines, " "+a[l.ai])
				h.OldLines++
				h.NewLines++
			case opDelete:
				h.Lines = append(h.Lines, "-"+a[l.ai])
				h.OldLines++
			case opInsert:
				pendingAdds = append(pendingAdds, "+"+b[l.bi])
				h.NewLines++
			}
		}
		flush()
		// A side with no lines in the hunk starts at the line before it, as
		// git writes "-0,0" for a created file.
		if h.OldLines == 0 {
			h.OldStart--
		}
		if h.NewLines == 0 {
			h.NewStart--
		}
		hunks = append(hunks, h)
		i = j
	}
	return hunks, additions, deletions
}
