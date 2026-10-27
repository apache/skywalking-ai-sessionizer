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
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func TestCheckNamePart(t *testing.T) {
	type testCase struct {
		name string
		part string
		err  string
	}
	cases := []testCase{
		{name: "session UUID", part: "772204d1-ab3e-4c11-9f2a-1d2e3f4a5b6c"},
		{name: "LangSmith session", part: "ls-team-a1b2c3d4e5f6"},
		{name: "prefixed session", part: "sess_772204d1-ab3e-4c11-9f2a-1d2e3f4a5b6c"},
		{name: "landed file", part: "transcript-20261025T103200.000000000Z-000003.sd"},
		{name: "hidden name", part: ".lock"},
		{name: "near the reserved prefix", part: ".tmpx-session"},
		{name: "reserved prefix inside", part: "a.tmp-b"},
		{name: "reserved prefix", part: ".tmp-session", err: "reserved prefix"},
		{name: "reserved prefix in capitals", part: ".TMP-session", err: "reserved prefix"},
		{name: "reserved prefix in mixed case", part: ".tMp-1", err: "reserved prefix"},
		{name: "interior dots and spaces", part: "team .. one.sd"},
		{name: "leading space", part: " team"},
		{name: "UTF-8", part: "会話-é-Σ"},
		{name: "replacement character", part: "a\ufffdb"},
		{name: "JSON escapes", part: "a&b\u2028c\u2029d"},
		{name: "outside controls", part: "a ~\u00a0"},
		{name: "255 bytes", part: strings.Repeat("a", 255)},
		{name: "255 UTF-8 bytes", part: strings.Repeat("é", 127) + "a"},
		{name: "empty", err: "empty"},
		{name: "dot", part: ".", err: "dot path component"},
		{name: "parent", part: "..", err: "dot path component"},
		{name: "256 bytes", part: strings.Repeat("a", 256), err: "exceeds 255 bytes"},
		{name: "256 UTF-8 bytes", part: strings.Repeat("é", 128), err: "exceeds 255 bytes"},
		{name: "invalid UTF-8 byte", part: "a\xffb", err: "invalid UTF-8"},
		{name: "incomplete UTF-8", part: "a\xc3", err: "invalid UTF-8"},
		{name: "overlong UTF-8", part: "a\xc0\xafb", err: "invalid UTF-8"},
		{name: "surrogate UTF-8", part: "a\xed\xa0\x80b", err: "invalid UTF-8"},
		{name: "out of range UTF-8", part: "a\xf4\x90\x80\x80b", err: "invalid UTF-8"},
		{name: "trailing dot", part: "file.", err: "ends in a dot or space"},
		{name: "trailing space", part: "file ", err: "ends in a dot or space"},
	}
	for _, r := range "<>:\"|?*\\/" {
		cases = append(cases, testCase{
			name: fmt.Sprintf("forbidden %U", r), part: "a" + string(r) + "b", err: "forbidden character",
		})
	}
	for r := rune(0); r <= '\u009f'; r++ {
		if r > '\u001f' && r < '\u007f' {
			continue
		}
		cases = append(cases, testCase{
			name: fmt.Sprintf("control %U", r), part: "a" + string(r) + "b", err: "control character",
		})
	}
	devices := []string{"CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$"}
	for _, prefix := range []string{"COM", "LPT"} {
		for _, suffix := range []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "¹", "²", "³"} {
			devices = append(devices, prefix+suffix)
		}
	}
	for _, device := range devices {
		for _, spelling := range []string{device, strings.ToLower(device), device[:1] + strings.ToLower(device[1:])} {
			for _, extension := range []string{"", ".txt", " .txt", "  .tar.gz"} {
				part := spelling + extension
				cases = append(cases, testCase{name: "device " + part, part: part, err: "reserved device name"})
			}
		}
	}
	for _, part := range []string{
		"CONSOLE", "CONIN", "CONOUT", "NULled", "auxiliary.txt", "x.CON", " CON.txt", "CON\u00a0.txt",
		"COM0", "COM10", "COM¹0", "COM⁴", "LPT0", "LPT10", "LPT¹0", "LPT⁴", "conın$",
	} {
		cases = append(cases, testCase{name: "not a device " + part, part: part})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := storage.CheckNamePart(tc.part)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("CheckNamePart(%q) error = %v, want %q", tc.part, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckNamePart(%q): %v", tc.part, err)
			}
		})
	}
}

func TestCheckRelativePath(t *testing.T) {
	// Every part stays below its own limit, so only the whole-path guard can
	// refuse the extra byte.
	limit := strings.Repeat(strings.Repeat("a", 255)+"/", 3) + strings.Repeat("b", 254) + "/c"
	for _, tc := range []struct {
		name string
		path string
		err  string
	}{
		{name: "session", path: "772204d1-ab3e-4c11-9f2a-1d2e3f4a5b6c"},
		{name: "landed file", path: "streams/main/transcript-20261025T103200.000000000Z-000003.sd"},
		{name: "Unicode owners", path: "streams/会話/résumé.sd"},
		{name: "control directory", path: "_conversations/sess_772204d1/rounds/r000001.sf"},
		{name: "255 byte part", path: "streams/" + strings.Repeat("a", 255) + "/file.sd"},
		{name: "1024 bytes", path: limit},
		{name: "empty", err: "empty"},
		{name: "leading slash", path: "/streams/main/file.sd", err: "leading or trailing slash"},
		{name: "trailing slash", path: "streams/main/", err: "leading or trailing slash"},
		{name: "root", path: "/", err: "leading or trailing slash"},
		{name: "empty part", path: "streams//file.sd", err: "empty"},
		{name: "dot part", path: "streams/./file.sd", err: "dot path component"},
		{name: "parent part", path: "streams/../file.sd", err: "dot path component"},
		{name: "backslash", path: "streams\\main\\file.sd", err: "forbidden character"},
		{name: "drive", path: "C:/streams/file.sd", err: "forbidden character"},
		{name: "256 byte part", path: "streams/" + strings.Repeat("a", 256) + "/file.sd", err: "exceeds 255 bytes"},
		{name: "1025 bytes", path: limit + "d", err: "exceeds 1024 bytes"},
		{name: "invalid UTF-8", path: "streams/ma\xffin/file.sd", err: "invalid UTF-8"},
		{name: "control", path: "streams/ma\u0085in/file.sd", err: "control character"},
		{name: "NUL", path: "streams/main/file\x00.sd", err: "control character"},
		{name: "trailing dot in part", path: "streams/main./file.sd", err: "ends in a dot or space"},
		{name: "trailing space in part", path: "streams/main /file.sd", err: "ends in a dot or space"},
		{name: "device directory", path: "streams/NuL .txt/file.sd", err: "reserved device name"},
		{name: "device file", path: "streams/main/LPT³.sd", err: "reserved device name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := storage.CheckRelativePath(tc.path)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("CheckRelativePath(%q) error = %v, want %q", tc.path, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckRelativePath(%q): %v", tc.path, err)
			}
		})
	}
}

func TestCheckRemoteName(t *testing.T) {
	for _, tc := range []struct {
		name   string
		remote string
		err    string
	}{
		{name: "all allowed characters", remote: "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.a"},
		{name: "remote", remote: "prod-east_1.example"},
		{name: "hidden", remote: ".prod"},
		{name: "UUID", remote: "772204d1-ab3e-4c11-9f2a-1d2e3f4a5b6c"},
		{name: "255 bytes", remote: strings.Repeat("a", 255)},
		{name: "device prefix", remote: "COM10"},
		{name: "empty", err: "empty"},
		{name: "dot", remote: ".", err: "dot path component"},
		{name: "parent", remote: "..", err: "dot path component"},
		{name: "256 bytes", remote: strings.Repeat("a", 256), err: "exceeds 255 bytes"},
		{name: "slash", remote: "prod/east", err: "forbidden character"},
		{name: "backslash", remote: "prod\\east", err: "forbidden character"},
		{name: "trailing dot", remote: "prod.", err: "ends in a dot or space"},
		{name: "trailing space", remote: "prod ", err: "ends in a dot or space"},
		{name: "device", remote: "cOn", err: "reserved device name"},
		{name: "device extension", remote: "LpT9.txt", err: "reserved device name"},
		{name: "device spaces", remote: "CON .txt", err: "reserved device name"},
		{name: "invalid UTF-8", remote: "prod\xff", err: "invalid UTF-8"},
		{name: "control", remote: "pr\x00od", err: "control character"},
		{name: "Unicode", remote: "pröd", err: "outside A-Z"},
		{name: "Unicode ASCII alias", remote: "K", err: "outside A-Z"},
		{name: "space", remote: "prod east", err: "outside A-Z"},
		{name: "plus", remote: "prod+east", err: "outside A-Z"},
		{name: "at sign", remote: "prod@east", err: "outside A-Z"},
		{name: "dollar", remote: "prod$", err: "outside A-Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := storage.CheckRemoteName(tc.remote)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("CheckRemoteName(%q) error = %v, want %q", tc.remote, err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckRemoteName(%q): %v", tc.remote, err)
			}
		})
	}
}

func TestCheckCaseFolding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		paths    []string
		conflict []string
	}{
		{name: "empty"},
		{name: "single", paths: []string{"streams/main/transcript-20261025T103200.000000000Z-000003.sd"}},
		{name: "shared spelling", paths: []string{"streams/main/a.sd", "streams/main/b.sd"}},
		{name: "same leaf different owners", paths: []string{"streams/main/a.sd", "streams/child/A.sd"}},
		{name: "same directory name different parents", paths: []string{"streams/Main/a.sd", "runs/main/b.sd"}},
		{name: "simple folding only", paths: []string{"Straße/a.sd", "STRASSE/b.sd", "ﬀ/a.sd", "ff/b.sd"}},
		{name: "no normalization", paths: []string{"é/a.sd", "e\u0301/b.sd"}},
		{name: "distinct dotted and dotless I", paths: []string{"I/a.sd", "İ/b.sd", "ı/c.sd"}},
		{
			name: "duplicate", paths: []string{"streams/main/a.sd", "streams/main/a.sd"},
			conflict: []string{"streams/main/a.sd", "streams/main/a.sd"},
		},
		{
			name: "leaf case", paths: []string{"streams/main/a.sd", "streams/main/A.SD"},
			conflict: []string{"streams/main/a.sd", "streams/main/A.SD"},
		},
		{
			name: "directory case", paths: []string{"streams/Main/a.sd", "streams/main/b.sd"},
			conflict: []string{"streams/Main/a.sd", "streams/main/b.sd"},
		},
		{
			name: "directory case reversed", paths: []string{"streams/main/b.sd", "streams/Main/a.sd"},
			conflict: []string{"streams/main/b.sd", "streams/Main/a.sd"},
		},
		{
			name: "first part case", paths: []string{"Streams/main/a.sd", "streams/child/b.sd"},
			conflict: []string{"Streams/main/a.sd", "streams/child/b.sd"},
		},
		{
			name: "deep directory case", paths: []string{"s/provider/tips/One/a.gz", "s/provider/tips/one/b.gz"},
			conflict: []string{"s/provider/tips/One/a.gz", "s/provider/tips/one/b.gz"},
		},
		{
			name: "nonadjacent collision", paths: []string{"streams/main/a.sd", "runs/other/b.sd", "streams/main/A.sd"},
			conflict: []string{"streams/main/a.sd", "streams/main/A.sd"},
		},
		{
			name: "Unicode leaf", paths: []string{"streams/main/é.sd", "streams/main/É.sd"},
			conflict: []string{"streams/main/é.sd", "streams/main/É.sd"},
		},
		{
			name: "Kelvin sign", paths: []string{"streams/K/a.sd", "streams/K/b.sd"},
			conflict: []string{"streams/K/a.sd", "streams/K/b.sd"},
		},
		{
			name: "long s", paths: []string{"streams/s/a.sd", "streams/ſ/b.sd"},
			conflict: []string{"streams/s/a.sd", "streams/ſ/b.sd"},
		},
		{
			name: "final sigma", paths: []string{"streams/σ/a.sd", "streams/ς/b.sd"},
			conflict: []string{"streams/σ/a.sd", "streams/ς/b.sd"},
		},
		{
			name: "capital sigma", paths: []string{"streams/Σ/a.sd", "streams/ς/b.sd"},
			conflict: []string{"streams/Σ/a.sd", "streams/ς/b.sd"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := storage.CheckCaseFolding(tc.paths)
			if len(tc.conflict) != 0 {
				if err == nil {
					t.Fatalf("CheckCaseFolding(%q) accepted conflicting names", tc.paths)
				}
				for _, path := range tc.conflict {
					if !strings.Contains(err.Error(), strconv.Quote(path)) {
						t.Errorf("conflict does not name %q: %v", path, err)
					}
				}
				if tc.conflict[0] == tc.conflict[1] && strings.Count(err.Error(), strconv.Quote(tc.conflict[0])) < 2 {
					t.Errorf("duplicate path is not named twice: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("CheckCaseFolding(%q): %v", tc.paths, err)
			}
		})
	}
}
