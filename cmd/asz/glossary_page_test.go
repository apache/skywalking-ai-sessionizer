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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// glossaryPages names the page under docs/en/adapters that copies each
// dialect's tables.
var glossaryPages = map[string]string{
	"claude-code/1": "claude-code-glossary.md",
	"kiro/1":        "kiro-glossary.md",
	"langsmith/1":   "langsmith-glossary.md",
}

// totalsLine is the line a glossary page gives its totals in.
var totalsLine = regexp.MustCompile(`\*\*(\d+) terms: (\d+) that [^,]+ names?, (\d+) this project derives\.\*\*`)

// TestGlossaryPagesCopyTheCommand. Each glossary page says its tables are
// copied from what asz glossary prints, and gives the totals the command
// prints. A term added to an adapter and to its page's table, but not to the
// page's totals, left two pages saying 64 terms where the command said 66.
// So each page's totals, and every row of its two tables, are checked
// against the declarations the command reads.
func TestGlossaryPagesCopyTheCommand(t *testing.T) {
	for _, dialect := range dialectNames() {
		t.Run(dialect, func(t *testing.T) {
			page, ok := glossaryPages[dialect]
			if !ok {
				t.Fatalf("%s has no glossary page", dialect)
			}
			data, err := os.ReadFile(filepath.Join("..", "..", "docs", "en", "adapters", page))
			if err != nil {
				t.Fatal(err)
			}
			g, _ := glossaryFor(dialect)
			var named, derived []string
			for _, term := range g.Terms() {
				if term.Derived() {
					derived = append(derived, fmt.Sprintf("%s | %s", term.Unified, term.Note))
				} else {
					named = append(named, fmt.Sprintf("%s | %s | %s | %s", term.Unified, term.Native, term.Where, term.Note))
				}
			}
			m := totalsLine.FindStringSubmatch(string(data))
			want := fmt.Sprint(len(named)+len(derived), len(named), len(derived))
			if m == nil || strings.Join(m[1:], " ") != want {
				t.Errorf("%s gives the totals %q; the command prints %s terms, named and derived", page, m, want)
			}
			gotNamed, gotDerived := glossaryRows(string(data))
			if strings.Join(gotNamed, "\n") != strings.Join(named, "\n") {
				t.Errorf("%s names these terms:\n%s\nthe command prints:\n%s", page, strings.Join(gotNamed, "\n"), strings.Join(named, "\n"))
			}
			if strings.Join(gotDerived, "\n") != strings.Join(derived, "\n") {
				t.Errorf("%s derives these terms:\n%s\nthe command prints:\n%s", page, strings.Join(gotDerived, "\n"), strings.Join(derived, "\n"))
			}
		})
	}
}

// glossaryRows reads the rows of a page's two tables, under the headings that
// start "## Named by" and "## Derived by", in the form the test builds from
// the declarations. A cell is read without its code marks, which a page adds
// so that a name like <synthetic> shows. A derived row keeps its model name
// and its note, its last cell: a page may leave out the runtime and where
// columns, which say nothing for a derived term.
func glossaryRows(page string) (named, derived []string) {
	var table *[]string
	for _, line := range strings.Split(page, "\n") {
		switch {
		case strings.HasPrefix(line, "## Named by"):
			table = &named
			continue
		case strings.HasPrefix(line, "## Derived by"):
			table = &derived
			continue
		case strings.HasPrefix(line, "## "):
			table = nil
			continue
		}
		if table == nil || !strings.HasPrefix(line, "| `") {
			continue
		}
		var cells []string
		for _, c := range strings.Split(strings.Trim(line, "|"), "|") {
			cells = append(cells, strings.ReplaceAll(strings.TrimSpace(c), "`", ""))
		}
		if table == &derived {
			cells = []string{cells[0], cells[len(cells)-1]}
		}
		*table = append(*table, strings.Join(cells, " | "))
	}
	return named, derived
}
