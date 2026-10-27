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

package storage_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/apache/skywalking-ai-sessionizer/internal/storage"
)

func checkControlMode(t *testing.T, got, want os.FileMode) {
	t.Helper()
	if (got&0o222 != 0) != (want&0o222 != 0) {
		t.Fatalf("writable mode = %o, want %o", got, want)
	}
}

func TestControlReadProcessHelper(t *testing.T) {
	path := os.Getenv("ASZ_CONTROL_READ_PATH")
	if path == "" {
		return
	}
	var file *os.File
	var err error
	if os.Getenv("ASZ_CONTROL_READ_SHARING") == "delete" {
		file, err = storage.OpenShared(path)
	} else {
		name, conversion := windows.UTF16PtrFromString(path)
		if conversion != nil {
			t.Fatal(conversion)
		}
		handle, open := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		err = open
		if err == nil {
			file = os.NewFile(uintptr(handle), path)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fmt.Println("ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(string(data))
}

func holdControlReader(t *testing.T, path, sharing string) func() string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	command := exec.CommandContext(ctx, executable, "-test.run=^TestControlReadProcessHelper$")
	command.Env = append(os.Environ(), "ASZ_CONTROL_READ_PATH="+path, "ASZ_CONTROL_READ_SHARING="+sharing)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(output)
	ready, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != "ready" {
		t.Fatalf("reader startup: %q, %v", ready, err)
	}
	joined := false
	t.Cleanup(func() {
		input.Close()
		if !joined {
			cancel()
			command.Wait()
		}
	})
	return func() string {
		if _, err := io.WriteString(input, "read\n"); err != nil {
			t.Fatal(err)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, reader)
		if err := command.Wait(); err != nil {
			t.Fatal(err)
		}
		joined = true
		return strings.TrimSpace(line)
	}
}

func logWindowsVolume(t *testing.T, directory string) {
	t.Helper()
	version := windows.RtlGetVersion()
	volume, err := windows.UTF16PtrFromString(filepath.VolumeName(directory) + `\`)
	if err != nil {
		t.Fatal(err)
	}
	var name [256]uint16
	if err := windows.GetVolumeInformation(volume, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		t.Fatal(err)
	}
	fileSystem := windows.UTF16ToString(name[:])
	t.Logf("Windows %d.%d build %d; file system %s", version.MajorVersion, version.MinorVersion, version.BuildNumber, fileSystem)
	// The replacement proof stands for NTFS only. A runner on another file
	// system must say so rather than pass by skipping.
	if fileSystem != "NTFS" {
		t.Fatalf("control sharing is proved on NTFS; this runner's file system is %s", fileSystem)
	}
}

func TestWindowsControlSharingAcrossProcesses(t *testing.T) {
	directory := t.TempDir()
	logWindowsVolume(t, directory)
	for _, name := range []string{"_archives/archive.json", "_freezes/freeze.json", "_state", "_history", "_last_use", "_snapshot", "_leases/lease.json", "_copy", "snapshot/_copy", "tokens"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("old complete record"), nil); err != nil {
				t.Fatal(err)
			}
			finish := holdControlReader(t, path, "delete")
			if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("new complete record"), nil); err != nil {
				t.Fatal(err)
			}
			requireText(t, path, "new complete record")
			if got := finish(); got != "old complete record" {
				t.Fatalf("held reader: %q", got)
			}
		})
	}
}

func TestWindowsForeignReaderBusy(t *testing.T) {
	directory := t.TempDir()
	logWindowsVolume(t, directory)
	path := filepath.Join(directory, "record")
	if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("old"), nil); err != nil {
		t.Fatal(err)
	}
	finish := holdControlReader(t, path, "ordinary")
	temporary := filepath.Join(directory, ".tmp-probe")
	if err := os.WriteFile(temporary, []byte("probe"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	raw := root.Rename(filepath.Base(temporary), filepath.Base(path))
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	if raw == nil {
		t.Fatal("foreign reader did not obstruct replacement")
	}
	t.Logf("Root.Rename returned %T: %v", raw, raw)
	if err := os.Remove(temporary); err != nil {
		t.Fatal(err)
	}
	var last error
	count := 0
	start := time.Now()
	err = storage.RetryControl(context.Background(), func(context.Context) error {
		count++
		err := storage.ReplaceControlDurable(path, storage.PermState, controlText("new"), nil)
		var busy *storage.ControlBusyError
		if !errors.As(err, &busy) {
			t.Fatalf("expected busy, got %v", err)
		}
		last = busy.Err
		entries, read := os.ReadDir(directory)
		if read != nil || len(entries) != 1 {
			t.Fatalf("busy attempt retained replacement: %v, %v", entries, read)
		}
		return err
	})
	//nolint:errorlint // the same error value, not one that wraps it
	if err != last || count < 2 || time.Since(start) > 3*time.Second {
		t.Fatalf("busy bound: %v, attempts %d, time %v", err, count, time.Since(start))
	}
	if got := finish(); got != "old" {
		t.Fatalf("foreign reader: %q", got)
	}
	if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("new"), nil); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsControlPermissionFailure(t *testing.T) {
	directory := t.TempDir()
	logWindowsVolume(t, directory)
	path := filepath.Join(directory, "record")
	if err := storage.ReplaceControlDurable(path, storage.PermState, controlText("old"), nil); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// A rename replaces the record with DELETE on it, SD, or with
	// FILE_DELETE_CHILD on its directory, DT. Both are denied. The temporary
	// file is still made, so the refusal comes at the replacement.
	for _, item := range []struct{ path, denied string }{{directory, "DT"}, {path, "SD"}} {
		saved, err := windows.GetNamedSecurityInfo(item.path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		original, _, err := saved.DACL()
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := windows.SecurityDescriptorFromString("D:P(D;;" + item.denied + ";;;" + user.User.Sid.String() + ")(A;;FA;;;" + user.User.Sid.String() + ")")
		if err != nil {
			t.Fatal(err)
		}
		denied, _, err := descriptor.DACL()
		if err != nil {
			t.Fatal(err)
		}
		if err := windows.SetNamedSecurityInfo(item.path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, denied, nil); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := windows.SetNamedSecurityInfo(item.path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION, nil, nil, original, nil); err != nil {
				t.Error(err)
			}
		})
	}
	var last error
	start := time.Now()
	err = storage.RetryControl(context.Background(), func(context.Context) error {
		err := storage.ReplaceControlDurable(path, storage.PermState, controlText("new"), nil)
		var busy *storage.ControlBusyError
		if !errors.As(err, &busy) || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			t.Fatalf("permission failure: %v", err)
		}
		last = busy.Err
		return err
	})
	//nolint:errorlint // the same error value, not one that wraps it
	if err != last || time.Since(start) > 3*time.Second {
		t.Fatalf("permission retry: %v", err)
	}
	requireText(t, path, "old")
}
