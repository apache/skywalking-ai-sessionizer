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

import (
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsControlBusyErrors(t *testing.T) {
	for _, code := range []error{windows.ERROR_SHARING_VIOLATION, windows.ERROR_LOCK_VIOLATION, windows.ERROR_ACCESS_DENIED} {
		raw := &os.LinkError{Op: "rename", Old: "old", New: "new", Err: code}
		err := controlRenameError(raw)
		var busy *ControlBusyError
		//nolint:errorlint // the same error value, not one that wraps it
		if !errors.As(err, &busy) || busy.Err != raw || !errors.Is(err, code) {
			t.Fatalf("mapping %v: %v", code, err)
		}
	}
	raw := &os.LinkError{Op: "rename", Old: "old", New: "new", Err: windows.ERROR_DISK_FULL}
	//nolint:errorlint // the same error value, not one that wraps it
	if got := controlRenameError(raw); got != raw {
		t.Fatalf("classified another failure as busy: %v", got)
	}
	if err := controlRenameError(nil); err != nil {
		t.Fatal(err)
	}
}
