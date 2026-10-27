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

// renameNoReplaceAt moves a file or a directory between two bound directories
// on one volume. Without the replace flag the system refuses an existing
// destination in the same call, and the rename never copies across volumes.
func renameNoReplaceAt(from *dirHandle, name string, to *dirHandle, toName, fromPath, toPath string) error {
	return from.moveAt(name, to, toName, false, fromPath, toPath)
}
