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
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

// Every listing of a root's sessions passes over a name that starts with an
// underscore (cmd/asz/parse.go, internal/export/otlp/push.go,
// internal/metrics/metrics.go, internal/retention/retention.go,
// internal/adapters/claudecodeprovider/seen.go, internal/scenario/run/remove.go).
// That is what keeps a pull's copies and a pull server's snapshots out of
// every production pass, so each top-level name pull owns must keep it.
func TestPullLayoutNamesPassUnderSessionListings(t *testing.T) {
	for _, name := range []string{storage.RemoteDir, storage.PullDir, storage.RootLockDir} {
		if !strings.HasPrefix(name, "_") {
			t.Fatalf("%s would be listed as a session", name)
		}
	}
}
