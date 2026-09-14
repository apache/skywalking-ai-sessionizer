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

package storage

import (
	"fmt"
	"os"
	"time"
)

// abandonedReservation is how old an empty file at a destination must be
// before it counts as the reservation of a writer that crashed. A live
// reservation lasts only from one system call to the next.
const abandonedReservation = time.Minute

// RemoveAbandonedReservation removes path when it is an empty regular file
// older than abandonedReservation, and reports whether it did. Nothing this
// package publishes is empty, so such a file can only be a reservation left
// by a crash on a filesystem without an exclusive rename. Two writers that
// find the same abandoned reservation at the same moment are not told apart.
func RemoveAbandonedReservation(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != 0 || time.Since(info.ModTime()) < abandonedReservation {
		return false
	}
	return os.Remove(path) == nil
}

// RenameExclusive installs a completed temporary file without replacing an
// existing destination. The files must be on the same filesystem; callers
// sync the file before publication and the directory afterwards. An empty
// file is refused: an empty destination is how a crashed reservation is
// recognized.
func RenameExclusive(from, to string) error {
	if info, err := os.Lstat(from); err != nil {
		return fmt.Errorf("storage: publish %s: %w", to, err)
	} else if info.Size() == 0 {
		return fmt.Errorf("storage: publish %s: the file is empty", to)
	}
	err := installExclusive(from, to)
	if os.IsExist(err) && RemoveAbandonedReservation(to) {
		err = installExclusive(from, to)
	}
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("%w: %s", ErrExists, to)
		}
		return fmt.Errorf("storage: publish %s: %w", to, err)
	}
	return nil
}
