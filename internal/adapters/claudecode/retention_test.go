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

package claudecode_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/claudecode"
)

func writeRetentionSettings(t *testing.T, root, path, body string) string {
	t.Helper()
	path = filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInspectRetention(t *testing.T) {
	for _, c := range []struct {
		name    string
		files   map[string]string
		days    int64
		path    string
		managed bool
	}{
		{name: "missing files"},
		{name: "unset", files: map[string]string{
			"user/settings.json":                `{"desktopSessionCleanupPeriodDays":1}`,
			"managed/managed-settings.json":     `{}`,
			"managed/managed-settings.d/a.json": `{}`,
		}},
		{name: "user value", files: map[string]string{
			"user/settings.json": `{"cleanupPeriodDays":30}`,
		}, days: 30, path: "user/settings.json"},
		{name: "managed wins over user", files: map[string]string{
			"user/settings.json":            `{"cleanupPeriodDays":60}`,
			"managed/managed-settings.json": `{"cleanupPeriodDays":10}`,
		}, days: 10, path: "managed/managed-settings.json", managed: true},
		{name: "later managed file wins", files: map[string]string{
			"user/settings.json":                `{"cleanupPeriodDays":60}`,
			"managed/managed-settings.json":     `{"cleanupPeriodDays":40}`,
			"managed/managed-settings.d/a.json": `{"cleanupPeriodDays":20}`,
			"managed/managed-settings.d/z.json": `{"cleanupPeriodDays":10}`,
		}, days: 10, path: "managed/managed-settings.d/z.json", managed: true},
		{name: "managed names follow UTF-16 order", files: map[string]string{
			"managed/managed-settings.d/Ａ.json": `{"cleanupPeriodDays":30}`,
			"managed/managed-settings.d/😀.json": `{"cleanupPeriodDays":2}`,
		}, days: 30, path: "managed/managed-settings.d/Ａ.json", managed: true},
		{name: "unset managed file keeps earlier value", files: map[string]string{
			"managed/managed-settings.json":     `{"cleanupPeriodDays":40}`,
			"managed/managed-settings.d/z.json": `{}`,
		}, days: 40, path: "managed/managed-settings.json", managed: true},
		{name: "empty managed file keeps earlier value", files: map[string]string{
			"managed/managed-settings.json":     `{"cleanupPeriodDays":40}`,
			"managed/managed-settings.d/z.json": "\xef\xbb\xbf \t\r\n",
		}, days: 40, path: "managed/managed-settings.json", managed: true},
		{name: "hidden and other files ignored", files: map[string]string{
			"managed/managed-settings.d/a.json":                       `{"cleanupPeriodDays":10}`,
			"managed/managed-settings.d/.hidden.json":                 `invalid`,
			"managed/managed-settings.d/z.json.bak":                   `invalid`,
			"managed/managed-settings.d/z.JSON":                       `invalid`,
			"managed/managed-settings.d/directory.json/settings.json": `invalid`,
		}, days: 10, path: "managed/managed-settings.d/a.json", managed: true},
		{name: "30.0 is a whole number", files: map[string]string{
			"user/settings.json": `{"cleanupPeriodDays":30.0}`,
		}, days: 30, path: "user/settings.json"},
		{name: "one day", files: map[string]string{
			"user/settings.json": `{"cleanupPeriodDays":1}`,
		}, days: 1, path: "user/settings.json"},
		{name: "largest comparable value", files: map[string]string{
			"user/settings.json": fmt.Sprintf(`{"cleanupPeriodDays":%d}`, math.MaxInt64/int64(24*time.Hour)),
		}, days: math.MaxInt64 / int64(24*time.Hour), path: "user/settings.json"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			for path, body := range c.files {
				writeRetentionSettings(t, root, path, body)
			}
			got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
			path := ""
			if c.path != "" {
				path = filepath.Join(root, c.path)
			}
			if got.Days != c.days || got.Path != path || got.Managed != c.managed || len(got.Problems) != 0 {
				t.Fatalf("inspection = %+v, want %d days from %q, managed %t, with no problems", got, c.days, path, c.managed)
			}
		})
	}
}

func TestInspectRetentionNormalizesSettings(t *testing.T) {
	encodeUTF16 := func(body string, order binary.ByteOrder) string {
		units := utf16.Encode([]rune(body))
		data := make([]byte, len(units)*2)
		for i, unit := range units {
			order.PutUint16(data[i*2:], unit)
		}
		return string(data)
	}
	const value = `{"cleanupPeriodDays":30}`
	littleEndian := encodeUTF16("\ufeff"+value, binary.LittleEndian)
	const whitespace = "\t\n\v\f\r \u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff"
	for _, path := range []string{"user/settings.json", "managed/managed-settings.json", "managed/managed-settings.d/a.json"} {
		for _, c := range []struct {
			name, body string
			days       int64
			problem    string
		}{
			{name: "empty"},
			{name: "whitespace", body: " \t\r\n"},
			{name: "space then byte-order mark", body: " \ufeff"},
			{name: "JavaScript whitespace", body: whitespace},
			{name: "byte-order mark", body: "\xef\xbb\xbf"},
			{name: "byte-order mark and whitespace", body: "\xef\xbb\xbf \t\r\n"},
			{name: "byte-order mark and value", body: "\xef\xbb\xbf" + `{"cleanupPeriodDays":30}`, days: 30},
			{name: "byte-order mark and spaced value", body: "\xef\xbb\xbf \t\r\n" + `{"cleanupPeriodDays":30}` + "\n", days: 30},
			{name: "UTF-16LE value", body: littleEndian, days: 30},
			{name: "UTF-16LE empty", body: "\xff\xfe"},
			{name: "UTF-16LE whitespace", body: encodeUTF16("\ufeff"+whitespace, binary.LittleEndian)},
			{name: "UTF-16LE spaced value", body: encodeUTF16("\ufeff \r\n"+value+"\r\n", binary.LittleEndian), days: 30},
			{name: "UTF-16LE incomplete trailing code unit", body: littleEndian + "\x00", days: 30},
			{name: "UTF-16LE incomplete closing brace", body: littleEndian[:len(littleEndian)-1], problem: "cannot parse"},
			{name: "UTF-16LE without byte-order mark", body: encodeUTF16(value, binary.LittleEndian), problem: "cannot parse"},
			{name: "UTF-16BE value", body: encodeUTF16("\ufeff"+value, binary.BigEndian), problem: "cannot parse"},
			{name: "UTF-16LE repeated byte-order mark", body: encodeUTF16("\ufeff\ufeff"+value, binary.LittleEndian), problem: "cannot parse"},
			{name: "U+0085 is not whitespace", body: "\u0085", problem: "cannot parse"},
			{name: "space then byte-order mark before value", body: " \ufeff" + value, problem: "cannot parse"},
			{name: "byte-order mark after value", body: value + "\ufeff", problem: "cannot parse"},
			{name: "space separator before value", body: "\u00a0" + value, problem: "cannot parse"},
			{name: "repeated byte-order mark before value", body: "\ufeff\ufeff" + value, problem: "cannot parse"},
		} {
			t.Run(path+"/"+c.name, func(t *testing.T) {
				root := t.TempDir()
				full := writeRetentionSettings(t, root, path, c.body)
				got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
				if c.problem != "" {
					checkRetentionProblem(t, got, full, c.problem)
					return
				}
				wantPath := ""
				managed := false
				if c.days > 0 {
					wantPath = full
					managed = strings.HasPrefix(path, "managed/")
				}
				if got.Days != c.days || got.Path != wantPath || got.Managed != managed || len(got.Problems) != 0 {
					t.Fatalf("inspection = %+v, want %d days from %q, managed %t, with no problems", got, c.days, wantPath, managed)
				}
			})
		}
	}
}

func TestInspectRetentionFollowsSymbolicLinks(t *testing.T) {
	for _, path := range []string{"user/settings.json", "managed/managed-settings.json", "managed/managed-settings.d/a.json"} {
		for _, directory := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/directory=%t", path, directory), func(t *testing.T) {
				root := t.TempDir()
				target := t.TempDir()
				if !directory {
					target = writeRetentionSettings(t, root, "target.json", `{"cleanupPeriodDays":30}`)
				}
				full := filepath.Join(root, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, full); err != nil {
					t.Skipf("Cannot create a symbolic link: %v", err)
				}
				got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
				if directory {
					checkRetentionProblem(t, got, full, "skipped settings path because it is not a regular file")
					return
				}
				managed := strings.HasPrefix(path, "managed/")
				if got.Days != 30 || got.Path != full || got.Managed != managed || len(got.Problems) != 0 {
					t.Fatalf("inspection = %+v, want 30 days from %q, managed %t, with no problems", got, full, managed)
				}
			})
		}
	}
}

func TestInspectRetentionNamesInvalidValues(t *testing.T) {
	for _, path := range []string{"user/settings.json", "managed/managed-settings.json", "managed/managed-settings.d/a.json"} {
		for _, value := range []string{"1.5", "0", "-1", `"30"`, "1e300", "null", "true", "[]", "{}", fmt.Sprint(math.MaxInt64/int64(24*time.Hour) + 1)} {
			t.Run(path+"/"+value, func(t *testing.T) {
				root := t.TempDir()
				path := writeRetentionSettings(t, root, path, `{"cleanupPeriodDays":`+value+`}`)
				got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
				checkRetentionProblem(t, got, path, "cleanupPeriodDays")
				if !strings.Contains(got.Problems[0].Error(), fmt.Sprintf("%q is invalid", value)) {
					t.Errorf("problem does not name the invalid value: %v", got.Problems[0])
				}
			})
		}
	}
}

func TestInspectRetentionNamesInvalidFiles(t *testing.T) {
	for _, c := range []struct{ name, body, problem string }{
		{"cannot parse", `{"cleanupPeriodDays":30`, "cannot parse"},
		{"second object", `{} {}`, "cannot parse"},
		{"array", `[]`, "top level must be an object"},
		{"null", `null`, "top level must be an object"},
		{"string", `"settings"`, "top level must be an object"},
		{"number", `30`, "top level must be an object"},
		{"boolean", `true`, "top level must be an object"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeRetentionSettings(t, root, "user/settings.json", c.body)
			got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
			checkRetentionProblem(t, got, path, c.problem)
		})
	}
}

func TestInspectRetentionNamesReadFailures(t *testing.T) {
	for _, path := range []string{"user/settings.json", "managed/managed-settings.json", "managed/managed-settings.d"} {
		t.Run(path, func(t *testing.T) {
			root := t.TempDir()
			full := filepath.Join(root, path)
			problem := "skipped settings path because it is not a regular file"
			if strings.HasSuffix(path, ".d") {
				writeRetentionSettings(t, root, path, "not a directory")
				problem = "cannot read managed settings directory"
			} else if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
			checkRetentionProblem(t, got, full, problem)
		})
	}
}

func TestInspectRetentionNamesUnreadablePaths(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprintf("directory=%t", directory), func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "managed", "managed-settings.d")
			problem := "cannot read managed settings directory"
			if directory {
				if err := os.MkdirAll(path, 0o700); err != nil {
					t.Fatal(err)
				}
			} else {
				path = writeRetentionSettings(t, root, "user/settings.json", `{"cleanupPeriodDays":30}`)
				problem = "cannot read settings file"
			}
			if err := os.Chmod(path, 0); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o700) })
			var err error
			if directory {
				_, err = os.ReadDir(path)
			} else {
				_, err = os.ReadFile(path)
			}
			if err == nil {
				t.Skip("This process can read paths with all permissions removed.")
			}
			got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
			checkRetentionProblem(t, got, path, problem)
		})
	}
}

func TestInspectRetentionKeepsValidValuesAndNamesProblems(t *testing.T) {
	root := t.TempDir()
	user := writeRetentionSettings(t, root, "user/settings.json", `{"cleanupPeriodDays":30}`)
	bad := writeRetentionSettings(t, root, "managed/managed-settings.json", `{"cleanupPeriodDays":0}`)
	got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
	if got.Days != 30 || got.Path != user || got.Managed || len(got.Problems) != 1 || !strings.Contains(got.Problems[0].Error(), bad) {
		t.Fatalf("inspection lost the valid user value or the invalid managed value: %+v", got)
	}
}

func TestInspectRetentionNamesEveryProblem(t *testing.T) {
	root := t.TempDir()
	paths := []string{
		writeRetentionSettings(t, root, "user/settings.json", `{"cleanupPeriodDays":0}`),
		writeRetentionSettings(t, root, "managed/managed-settings.json", `[]`),
		writeRetentionSettings(t, root, "managed/managed-settings.d/a.json", `{`),
	}
	got := claudecode.InspectRetention(filepath.Join(root, "user"), filepath.Join(root, "managed"))
	if got.Days != 0 || got.Path != "" || len(got.Problems) != len(paths) {
		t.Fatalf("inspection = %+v, want every problem and no assumed value", got)
	}
	for i, path := range paths {
		if !strings.Contains(got.Problems[i].Error(), path) {
			t.Errorf("problem %q does not name %q", got.Problems[i], path)
		}
	}
}

func checkRetentionProblem(t *testing.T, got claudecode.RetentionInspection, path, problem string) {
	t.Helper()
	if got.Days != 0 || got.Path != "" || got.Managed || len(got.Problems) != 1 {
		t.Fatalf("inspection = %+v, want one problem and no assumed value", got)
	}
	if text := got.Problems[0].Error(); !strings.Contains(text, path) || !strings.Contains(text, problem) {
		t.Fatalf("problem %q does not name %q and %q", text, path, problem)
	}
}
