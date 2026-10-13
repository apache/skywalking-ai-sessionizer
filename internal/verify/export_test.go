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

package verify

import "github.com/apache/skywalking-ai-sessionizer/internal/storage"

// BeforeScan sets what runs between reading a session's removals and
// checking its bodies, and returns what puts it back.
func BeforeScan(fn func()) func() {
	beforeScan = fn
	return func() { beforeScan = nil }
}

// BetweenReads sets what runs between reading a session's generation and
// loading its intent, and returns what puts it back.
func BetweenReads(fn func()) func() {
	betweenReads = fn
	return func() { betweenReads = nil }
}

// RemovalView is what verify sees of a session's removals.
func RemovalView(z *storage.Zone, session string) (generation int, inProgress bool, through uint64, err error) {
	r, err := removalOf(z, session)
	return r.generation, r.inProgress, r.through, err
}
