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

package claudecode

import (
	"os"
	"syscall"
)

// statSource reads a source's identity and size. Windows has no inode; the
// volume serial number and the file index play the same part, stable for the
// life of a file and different for a file created in its place.
//
// The identity is optional. When it cannot be read the tail digest still
// guards the bytes behind the cursor, which is the check that matters.
func statSource(path string) (statInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return statInfo{}, ErrSourceGone
		}
		return statInfo{}, err
	}
	info := statInfo{size: fi.Size(), mtime: fi.ModTime().Unix()}
	f, err := os.Open(path)
	if err != nil {
		return info, nil
	}
	defer f.Close()
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err == nil {
		info.dev = uint64(d.VolumeSerialNumber)
		info.ino = uint64(d.FileIndexHigh)<<32 | uint64(d.FileIndexLow)
	}
	return info, nil
}
