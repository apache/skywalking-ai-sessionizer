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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// TestLockScenarioReportsBusy. One pipeline works on a scenario root at a
// time. A second one must be told the root is held rather than take it too,
// and a single pass that waits for it must give up at its deadline.
func TestLockScenarioReportsBusy(t *testing.T) {
	root := t.TempDir()
	held, err := storage.LockScenario(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, storage.ScenarioDir, ".lock")); err != nil {
		t.Fatalf("the lock is not under %s: %v", storage.ScenarioDir, err)
	}
	if _, err := storage.LockScenario(root); !errors.Is(err, storage.ErrScenarioBusy) {
		t.Fatalf("a second LockScenario gave %v, want ErrScenarioBusy", err)
	}
	start := time.Now()
	if _, err := storage.LockScenarioWait(root, 200*time.Millisecond); !errors.Is(err, storage.ErrScenarioBusy) {
		t.Fatalf("LockScenarioWait gave %v, want ErrScenarioBusy", err)
	}
	if waited := time.Since(start); waited < 200*time.Millisecond {
		t.Fatalf("LockScenarioWait gave up after %s, before its deadline", waited)
	}
	if err := held.Unlock(); err != nil {
		t.Fatal(err)
	}
	again, err := storage.LockScenarioWait(root, time.Second)
	if err != nil {
		t.Fatalf("the lock was not free once released: %v", err)
	}
	if err := again.Unlock(); err != nil {
		t.Fatal(err)
	}
}
