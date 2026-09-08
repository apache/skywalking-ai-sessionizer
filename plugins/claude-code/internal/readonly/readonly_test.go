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

package readonly_test

import (
	"bufio"
	"os"
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/plugins/claude-code/internal/readonly"
)

// TestFixture runs every command in testdata/commands.txt: real commands
// picked from a landed corpus, each with the outcome it must get.
func TestFixture(t *testing.T) {
	f, err := os.Open("testdata/commands.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	n := 0
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		expect, cmd, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("fixture line has no tab: %q", line)
		}
		n++
		got := "scan"
		if readonly.IsReadOnly(cmd) {
			got = "read"
		}
		if got != expect {
			t.Errorf("expect %s, got %s: %s", expect, got, cmd)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 50 {
		t.Fatalf("only %d cases read from the fixture", n)
	}
}
