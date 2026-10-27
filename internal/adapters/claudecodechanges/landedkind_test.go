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

package claudecodechanges

import (
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// This adapter names a landed file by its kind (collect.go), so the shared map
// must read each kind it lands back from that name.
func TestLandedNamesCarryTheirKinds(t *testing.T) {
	stamp := storage.Stamp(time.Now())
	for _, kind := range []sessiondata.Kind{sessiondata.KindChanges, sessiondata.KindExecution} {
		got, ok := storage.LandedKind(storage.LandedName(string(kind), stamp, 1))
		if !ok || got != kind {
			t.Fatalf("%q read back as %q %v", kind, got, ok)
		}
	}
}
