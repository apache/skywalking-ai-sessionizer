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

package langsmith

import (
	"testing"
	"time"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
	"github.com/apache/skywalking-ai-sessionizer/pkg/sessiondata"
)

// The receiver lands runs as transcripts (collect.go) and request bodies as
// provider bodies (bodies.go); the shared map reads each kind back from its name.
func TestLandedNamesCarryTheirKinds(t *testing.T) {
	stamp := storage.Stamp(time.Now())
	for prefix, want := range map[string]sessiondata.Kind{"transcript": sessiondata.KindTranscript, string(sessiondata.KindProviderBody): sessiondata.KindProviderBody} {
		if got, ok := storage.LandedKind(storage.LandedName(prefix, stamp, 1)); !ok || got != want {
			t.Fatalf("%s read back as %q %v", prefix, got, ok)
		}
	}
}
