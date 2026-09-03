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

//go:build !windows

package claudecode

import (
	"os"
	"syscall"
)

// statSource reads a source's identity and size. The identity is the device
// and inode, which a file keeps for its life and which a file created in its
// place does not share.
func statSource(path string) (statInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return statInfo{}, ErrSourceGone
		}
		return statInfo{}, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return statInfo{size: fi.Size(), mtime: fi.ModTime().Unix()}, nil
	}
	return statInfo{
		dev: uint64(st.Dev), ino: uint64(st.Ino),
		size: fi.Size(), mtime: fi.ModTime().Unix(),
	}, nil
}
