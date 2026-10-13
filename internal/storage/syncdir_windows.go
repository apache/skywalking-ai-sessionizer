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

//go:build windows

package storage

// syncDir is how a write makes its directory's entries durable. A test
// replaces it to see what a write does when the disk fails.
var syncDir = syncDirEntries

// syncDirEntries does nothing on Windows, where a directory cannot be synced
// as a file is: the filesystem records a rename in its own journal.
func syncDirEntries(string) error { return nil }

// SyncFile does nothing on Windows, as syncDirEntries does: a power loss is
// not what asz promises to survive there.
func SyncFile(string) error { return nil }
