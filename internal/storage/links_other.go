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

//go:build !linux && !darwin

package storage

// sharesLinks is false here, so every snapshot gets its own copy. Windows
// deletion can clear a read-only attribute on the file a link shares
// (src/os/file_windows.go:247), and that change would reach another tree.
const sharesLinks = false

func linkUnavailable(error, string, FileSystem) (bool, error) { return true, nil }
