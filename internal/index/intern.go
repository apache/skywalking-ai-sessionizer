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

package index

import "sync"

// Interner maps identifier strings to compact ids.
//
// Identifiers dominate an index: a real session holds ~83,000 distinct ids
// across ~59,000 records, mostly 36-character uuids. Interning turns each
// reference into 4 bytes and lets an Entry stay pointer-free.
//
// Id 0 is reserved for "absent", which is the common case - many record types
// carry no uuid at all.
type Interner struct {
	mu sync.RWMutex
	m  map[string]uint32
	s  []string
}

// NewInterner returns an empty Interner.
func NewInterner() *Interner {
	return &Interner{m: make(map[string]uint32)}
}

// ID interns s and returns its id. The empty string always returns 0.
func (i *Interner) ID(s string) uint32 {
	if s == "" {
		return 0
	}
	i.mu.RLock()
	v, ok := i.m[s]
	i.mu.RUnlock()
	if ok {
		return v
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if v, ok := i.m[s]; ok {
		return v
	}
	v = uint32(len(i.s) + 1)
	i.m[s] = v
	i.s = append(i.s, s)
	return v
}

// Lookup returns the id for s without interning it, and whether it was present.
func (i *Interner) Lookup(s string) (uint32, bool) {
	if s == "" {
		return 0, true
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	v, ok := i.m[s]
	return v, ok
}

// String returns the identifier for an id, or "" for 0 or an unknown id.
func (i *Interner) String(id uint32) string {
	if id == 0 {
		return ""
	}
	i.mu.RLock()
	defer i.mu.RUnlock()
	if int(id) > len(i.s) {
		return ""
	}
	return i.s[id-1]
}

// Len returns how many distinct identifiers are interned.
func (i *Interner) Len() int {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return len(i.s)
}

// all returns the interned strings in id order, for persistence.
func (i *Interner) all() []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]string, len(i.s))
	copy(out, i.s)
	return out
}

// load restores an interner from strings in id order.
func loadInterner(ss []string) *Interner {
	in := &Interner{m: make(map[string]uint32, len(ss)), s: ss}
	for n, s := range ss {
		in.m[s] = uint32(n + 1)
	}
	return in
}
