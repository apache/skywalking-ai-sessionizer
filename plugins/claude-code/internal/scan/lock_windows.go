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

package scan

import (
	"io/fs"
	"os"
	"time"
)

// Lock is an exclusive lock held as an exclusively opened file. Windows
// has no advisory lock in the standard library; an exclusive open is what
// the storage layer uses too, and a crashed holder's handle is closed by
// the system, which releases it.
type Lock struct{ f *os.File }

// lockFile waits until the file can be opened exclusively.
func lockFile(path string) (*Lock, error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_EXCL, 0o600)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Unlock releases the lock.
func (l *Lock) Unlock() error {
	name := l.f.Name()
	_ = l.f.Close()
	return os.Remove(name)
}

// inodeOf has no cheap answer on Windows; size and time decide alone.
func inodeOf(fs.FileInfo) uint64 { return 0 }
