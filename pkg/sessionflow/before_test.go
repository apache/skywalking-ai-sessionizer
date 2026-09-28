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

package sessionflow_test

import (
	"strings"
	"testing"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// TestNodesOnOneRecordHaveOneOrder. Three nodes stand on one record: one on
// the whole record and two on its parts. Deciding the whole record against a
// part by id made a cycle - block 1 before the whole record, the whole record
// before block 0, block 0 before block 1 - so a sort could return any order.
// The whole record comes first, then the parts by block, whatever order the
// nodes arrive in.
func TestNodesOnOneRecordHaveOneOrder(t *testing.T) {
	node := func(id string, block *int) *sessionflow.Node {
		return &sessionflow.Node{Entity: sessionflow.Entity{ID: id}, Ref: &sessionflow.Ref{Seq: 1, Row: 1, Block: block}}
	}
	zero, one := 0, 1
	partOne, whole, partZero := node("tool/a", &one), node("tool/b", nil), node("tool/c", &zero)
	for _, in := range [][]*sessionflow.Node{
		{partOne, whole, partZero}, {partOne, partZero, whole}, {whole, partOne, partZero},
		{whole, partZero, partOne}, {partZero, partOne, whole}, {partZero, whole, partOne},
	} {
		var ids []string
		for _, n := range sessionflow.InOrder(in) {
			ids = append(ids, n.ID)
		}
		if got := strings.Join(ids, " "); got != "tool/b tool/c tool/a" {
			t.Errorf("%s, want tool/b tool/c tool/a: the whole record, then its parts by block", got)
		}
	}
}
