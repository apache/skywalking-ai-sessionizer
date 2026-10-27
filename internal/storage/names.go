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

package storage

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
)

// reservedPrefix starts the name of every temporary file an atomic write makes
// (internal/storage/zone.go, internal/storage/control.go).
const reservedPrefix = ".tmp-"

// CheckNamePart applies the same portability rule on every platform, so a
// source cannot publish a name that another machine cannot hold.
func CheckNamePart(part string) error {
	if part == "" {
		return fmt.Errorf("storage: name part is empty")
	}
	if part == "." || part == ".." {
		return fmt.Errorf("storage: name part %q is a dot path component", part)
	}
	if len(part) > 255 {
		return fmt.Errorf("storage: name part %q exceeds 255 bytes", part)
	}
	// Cleanup recognizes the files an interrupted atomic write leaves by this
	// prefix alone. A name holding it, in any letter case, could be taken for one.
	if len(part) >= len(reservedPrefix) && strings.EqualFold(part[:len(reservedPrefix)], reservedPrefix) {
		return fmt.Errorf("storage: name part %q starts with the reserved prefix %s", part, reservedPrefix)
	}

	// JSON replaces invalid UTF-8. This also checks that a request will name
	// exactly the same bytes after the receiver decodes it.
	encoded, err := json.Marshal(part)
	if err != nil {
		return fmt.Errorf("storage: encode name part %q as JSON: %w", part, err)
	}
	var decoded string
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return fmt.Errorf("storage: decode name part %q from JSON: %w", part, err)
	}
	if decoded != part {
		return fmt.Errorf("storage: name part %q has invalid UTF-8: JSON encoding and decoding changes its bytes", part)
	}
	for _, r := range part {
		if r <= '\u001f' || r >= '\u007f' && r <= '\u009f' {
			return fmt.Errorf("storage: name part %q contains control character %U", part, r)
		}
		if strings.ContainsRune("<>:\"|?*\\/", r) {
			return fmt.Errorf("storage: name part %q contains forbidden character %q", part, r)
		}
	}
	if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
		return fmt.Errorf("storage: name part %q ends in a dot or space", part)
	}

	base, _, _ := strings.Cut(part, ".")
	base = strings.TrimRight(base, " ")
	if strings.EqualFold(base, "CON") || strings.EqualFold(base, "PRN") ||
		strings.EqualFold(base, "AUX") || strings.EqualFold(base, "NUL") ||
		strings.EqualFold(base, "CONIN$") || strings.EqualFold(base, "CONOUT$") {
		return fmt.Errorf("storage: name part %q is a reserved device name", part)
	}
	if len(base) >= 4 && (strings.EqualFold(base[:3], "COM") || strings.EqualFold(base[:3], "LPT")) {
		switch base[3:] {
		case "1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³":
			return fmt.Errorf("storage: name part %q is a reserved device name", part)
		}
	}
	return nil
}

// CheckRelativePath keeps manifest names independent of the host's path rules.
func CheckRelativePath(p string) error {
	if len(p) > 1024 {
		return fmt.Errorf("storage: relative path %q exceeds 1024 bytes", p)
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return fmt.Errorf("storage: relative path %q has a leading or trailing slash", p)
	}
	for _, part := range strings.Split(p, "/") {
		if err := CheckNamePart(part); err != nil {
			return fmt.Errorf("storage: relative path %q: %w", p, err)
		}
	}
	return nil
}

// CheckRemoteName restricts configuration names to portable ASCII spellings.
func CheckRemoteName(name string) error {
	if err := CheckNamePart(name); err != nil {
		return fmt.Errorf("storage: remote name %q: %w", name, err)
	}
	for _, r := range name {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("storage: remote name %q contains character %q outside A-Z, a-z, 0-9, hyphen, underscore and dot", name, r)
	}
	return nil
}

// CheckCaseFolding prevents a destination from merging files or directories
// whose names differ only by letter case.
func CheckCaseFolding(paths []string) error {
	type node struct {
		part     string
		path     string
		complete bool
		children map[string]*node
	}
	var root node
	for _, p := range paths {
		parent := &root
		for _, part := range strings.Split(p, "/") {
			key := foldNamePart(part)
			child, ok := parent.children[key]
			if ok {
				if child.part != part {
					return fmt.Errorf("storage: paths %q and %q have conflicting spellings when letter case is ignored", child.path, p)
				}
			} else {
				child = &node{part: part, path: p}
				if parent.children == nil {
					parent.children = make(map[string]*node)
				}
				parent.children[key] = child
			}
			parent = child
		}
		if parent.complete {
			return fmt.Errorf("storage: paths %q and %q are equal when letter case is ignored", parent.path, p)
		}
		parent.complete = true
		parent.path = p
	}
	return nil
}

func foldNamePart(part string) string {
	// Lowercasing misses aliases such as the Greek final sigma. The smallest
	// rune in each simple-fold cycle gives EqualFold's classes one map key.
	return strings.Map(func(r rune) rune {
		key := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < key {
				key = folded
			}
		}
		return key
	}, part)
}
