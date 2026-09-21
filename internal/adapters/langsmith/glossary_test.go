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

package langsmith_test

import (
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/internal/adapters/langsmith"
	"github.com/apache/skywalking-ai-sessionizer/pkg/model"
)

// TestGlossaryCoversTheWholeVocabulary: every name the model can emit has an
// entry, including the ones saying this runtime has no word for it. A reader
// asking for the runtime's terms must never meet a name with nothing behind
// it.
func TestGlossaryCoversTheWholeVocabulary(t *testing.T) {
	g := langsmith.Glossary()
	var missing []string
	for _, name := range model.Vocabulary() {
		if _, ok := g.Lookup(name); !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d name(s) have no term:\n  %v", len(missing), missing)
	}
}
