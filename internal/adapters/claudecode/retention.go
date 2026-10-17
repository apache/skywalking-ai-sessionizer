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

package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
)

// RetentionInspection cannot establish Claude Code's effective retention.
// Other settings can change it, and some sessions are exempt from cleanup.
// Days is zero when no inspected file supplies a valid value.
// Problems must be checked before using the default.
type RetentionInspection struct {
	Days     int64
	Path     string
	Managed  bool
	Problems []error
}

// ManagedSettingsDir follows Claude Code's documented machine settings path.
func ManagedSettingsDir() string {
	switch runtime.GOOS {
	case "darwin":
		return "/Library/Application Support/ClaudeCode"
	case "windows":
		return `C:\Program Files\ClaudeCode`
	default:
		return "/etc/claude-code"
	}
}

// InspectRetention reads only cleanupPeriodDays. Managed values take priority
// over the user's value. Problems remain separate from valid inspected values.
// A problem must never be taken to mean that Claude Code uses its default.
func InspectRetention(configDir, managedDir string) RetentionInspection {
	var result RetentionInspection
	result.read(filepath.Join(configDir, "settings.json"), false)
	result.read(filepath.Join(managedDir, "managed-settings.json"), true)
	dir := filepath.Join(managedDir, "managed-settings.d")
	// A file in its place is there and cannot be read as a directory. Listing
	// it says so on Unix, and says "not found" on Windows, so it is asked
	// first.
	var entries []os.DirEntry
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		result.Problems = append(result.Problems, fmt.Errorf("%s: cannot read managed settings directory: it is not a directory", dir))
	} else if entries, err = os.ReadDir(dir); err != nil && !os.IsNotExist(err) {
		result.Problems = append(result.Problems, fmt.Errorf("%s: cannot read managed settings directory: %w", dir, err))
	}
	// Claude Code sorts names by UTF-16 code units. ReadDir uses UTF-8 bytes.
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return slices.Compare(utf16.Encode([]rune(a.Name())), utf16.Encode([]rune(b.Name())))
	})
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		result.read(filepath.Join(dir, entry.Name()), true)
	}
	return result
}

func (r *RetentionInspection) read(path string, managed bool) {
	// Claude Code reads only regular files or symbolic links to them.
	// Check the target before opening so a named pipe cannot block startup.
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		r.Problems = append(r.Problems, fmt.Errorf("%s: cannot inspect settings path: %w", path, err))
		return
	}
	if !info.Mode().IsRegular() {
		r.Problems = append(r.Problems, fmt.Errorf("%s: skipped settings path because it is not a regular file", path))
		return
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		r.Problems = append(r.Problems, fmt.Errorf("%s: cannot read settings file: %w", path, err))
		return
	}
	// Match Claude Code's reader, which selects UTF-16LE only for FF FE.
	if bytes.HasPrefix(data, []byte{0xff, 0xfe}) {
		// Buffer.toString ignores an incomplete final code unit.
		units := make([]uint16, len(data)/2)
		for i := range units {
			units[i] = uint16(data[2*i]) | uint16(data[2*i+1])<<8
		}
		data = []byte(string(utf16.Decode(units)))
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	// Claude Code uses JavaScript trim only to check for empty settings.
	if len(bytes.TrimFunc(data, func(c rune) bool {
		switch c {
		case '\t', '\n', '\v', '\f', '\r', '\ufeff', '\u2028', '\u2029':
			return true
		}
		return unicode.Is(unicode.Zs, c)
	})) == 0 {
		return
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		var wrongType *json.UnmarshalTypeError
		if errors.As(err, &wrongType) {
			r.Problems = append(r.Problems, fmt.Errorf("%s: the top level must be an object", path))
		} else {
			r.Problems = append(r.Problems, fmt.Errorf("%s: cannot parse settings file: %w", path, err))
		}
		return
	}
	if fields == nil {
		r.Problems = append(r.Problems, fmt.Errorf("%s: the top level must be an object", path))
		return
	}
	value, ok := fields["cleanupPeriodDays"]
	if !ok {
		return
	}
	// JavaScript reads 30.0 as 30. Check the range before converting to a
	// duration, since an overflow could reverse the retention comparison.
	var days float64
	const maxDays = math.MaxInt64 / int64(24*time.Hour)
	if err := json.Unmarshal(value, &days); err != nil || days < 1 || math.Trunc(days) != days || days > float64(maxDays) {
		r.Problems = append(r.Problems, fmt.Errorf("%s: cleanupPeriodDays %q is invalid. It must be a whole number of days from 1 to %d", path, value, maxDays))
		return
	}
	r.Days, r.Path, r.Managed = int64(days), path, managed
}
