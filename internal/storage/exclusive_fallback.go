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

//go:build linux || darwin

package storage

import (
	"errors"
	"os"
	"syscall"
)

// installLinked is for a filesystem without an exclusive rename. A hard link
// keeps both guarantees: the complete file appears at once, and an existing
// destination is refused.
func installLinked(from, to string) error {
	err := os.Link(from, to)
	if err == nil {
		_ = os.Remove(from)
		return nil
	}
	// Linux reports a filesystem without hard links as EPERM. exFAT on macOS
	// has neither an exclusive rename nor hard links, and reports ENOTSUP.
	if !errors.Is(err, syscall.EPERM) && !errors.Is(err, syscall.ENOTSUP) && !errors.Is(err, syscall.EOPNOTSUPP) {
		return err
	}
	return installReserved(from, to)
}

// installReserved is the last resort. The kernel still refuses an existing
// destination, through O_EXCL on an empty reservation. The reservation is
// then replaced by the complete file. Only between those two calls, with no
// write or sync between them, can a reader or a crash see an empty file.
// RenameExclusive takes away an empty file a crash left there.
func installReserved(from, to string) error {
	f, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(to)
		return err
	}
	if err := os.Rename(from, to); err != nil {
		_ = os.Remove(to)
		return err
	}
	return nil
}
