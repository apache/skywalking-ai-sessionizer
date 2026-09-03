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

package parse

import (
	"sort"

	"github.com/apache/skywalking-ai-sessionizer/pkg/sessionflow"
)

// sortDelta puts a round's frames in a fixed order.
//
// Without it the frames would follow Go's map iteration, which is deliberately
// randomised, and two runs over identical input would produce different bytes
// and different digests. The whole chain rests on that not happening.
func sortDelta(d *delta) {
	sort.Slice(d.nodes, func(i, j int) bool { return d.nodes[i].ID < d.nodes[j].ID })
	sort.Slice(d.relations, func(i, j int) bool { return d.relations[i].ID < d.relations[j].ID })
	sort.Slice(d.unresolved, func(i, j int) bool { return d.unresolved[i].ID < d.unresolved[j].ID })
}

// View folds a conversation's chain into its current structure.
//
// The view is derived and disposable: the rounds are what is authoritative.
// Deleting a view and folding again must reproduce it exactly.
func View(root, conversation string) (*sessionflow.View, error) {
	return sessionflow.OpenChain(root, conversation).Fold()
}
