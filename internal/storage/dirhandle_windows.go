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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dirHandle is a directory handle that every open is relative to, through
// NtCreateFile's RootDirectory, as os.Root does on Windows
// (src/internal/syscall/windows/at_windows.go). It is opened here because a
// lock needs an open that shares nothing, which os.Root does not offer.
type dirHandle struct{ f *os.File }

func openDirHandle(path string) (*dirHandle, fs.FileInfo, error) {
	name, err := exclusiveWindowsPath(path)
	if err != nil {
		return nil, nil, err
	}
	h, err := windows.CreateFile(name, windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(h), path)
	info, err := f.Stat()
	if err == nil && info.Mode().Type() != fs.ModeDir {
		err = fmt.Errorf("%w: %s is not a real directory", ErrProtected, path)
	}
	if err != nil {
		return nil, nil, errors.Join(err, f.Close())
	}
	return &dirHandle{f: f}, info, nil
}

func (d *dirHandle) close() error { return d.f.Close() }

func (d *dirHandle) openAt(name string, how fileOpen, perm os.FileMode) (*os.File, error) {
	path := filepath.Join(d.f.Name(), name)
	access := uint32(windows.GENERIC_READ | windows.SYNCHRONIZE)
	share := uint32(windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE)
	disposition := uint32(windows.FILE_OPEN)
	switch how {
	case appendExisting:
		access = windows.FILE_APPEND_DATA | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	case writeNew:
		access = windows.GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE
		share = windows.FILE_SHARE_READ
		disposition = windows.FILE_CREATE
	case lockExisting, lockNew:
		// A lock is an open that shares nothing: every other open fails until
		// this handle closes, and the system closes it when the process dies.
		access = windows.GENERIC_READ | windows.GENERIC_WRITE | windows.SYNCHRONIZE
		share = 0
		if how == lockNew {
			disposition = windows.FILE_CREATE
		}
	case changeMode:
		access = windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE
	}
	object, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(d.f.Fd()),
		ObjectName:    object,
	}
	fileAttributes := uint32(windows.FILE_ATTRIBUTE_NORMAL)
	if perm&0o200 == 0 {
		fileAttributes = windows.FILE_ATTRIBUTE_READONLY
	}
	var h windows.Handle
	var status windows.IO_STATUS_BLOCK
	// FILE_OPEN_REPARSE_POINT opens a link itself, never what it points at,
	// and the caller's regular-file check then refuses it.
	err = windows.NtCreateFile(&h, access, &attributes, &status, nil, fileAttributes, share, disposition,
		windows.FILE_NON_DIRECTORY_FILE|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	switch {
	case err == nil:
		return os.NewFile(uintptr(h), path), nil
	case errors.Is(err, windows.STATUS_SHARING_VIOLATION) && (how == lockExisting || how == lockNew):
		return nil, ErrBusy
	case errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION):
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrExist}
	case errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND), errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND):
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrNotExist}
	case errors.Is(err, windows.STATUS_FILE_IS_A_DIRECTORY):
		// FILE_NON_DIRECTORY_FILE refuses a directory before the caller's
		// regular-file check can, so it is refused here as that check
		// refuses it on every other system.
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrProtected, path)
	}
	return nil, &os.PathError{Op: "open", Path: path, Err: ntError(err)}
}

// renameInformation is FILE_RENAME_INFORMATION_EX, and with its first field
// read as a byte, FILE_RENAME_INFORMATION, as Go's own rename lays them out
// (src/internal/syscall/windows/types_windows.go).
type renameInformation struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [windows.MAX_PATH]uint16
}

// fileRenameInformationEx is the information class of the extended form.
const fileRenameInformationEx = 65

// renameAt moves name to toName in to, replacing it, with POSIX semantics
// where the file system has them, as os.Root's rename does
// (src/internal/syscall/windows/at_windows.go).
func (d *dirHandle) renameAt(name string, to *dirHandle, toName, from, dest string) error {
	return d.moveAt(name, to, toName, true, from, dest)
}

func (d *dirHandle) moveAt(name string, to *dirHandle, toName string, replace bool, from, dest string) error {
	object, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return err
	}
	attributes := windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: windows.Handle(d.f.Fd()),
		ObjectName:    object,
	}
	var h windows.Handle
	var status windows.IO_STATUS_BLOCK
	// FILE_OPEN_REPARSE_POINT moves a link itself, never what it points at.
	err = windows.NtCreateFile(&h, windows.DELETE|windows.SYNCHRONIZE, &attributes, &status, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		return &os.LinkError{Op: "rename", Old: from, New: dest, Err: ntError(err)}
	}
	defer func() { _ = windows.CloseHandle(h) }()
	target, err := windows.UTF16FromString(toName)
	if err != nil {
		return err
	}
	info := renameInformation{RootDirectory: windows.Handle(to.f.Fd())}
	if len(target) > len(info.FileName) {
		return &os.LinkError{Op: "rename", Old: from, New: dest, Err: windows.ERROR_FILENAME_EXCED_RANGE}
	}
	copy(info.FileName[:], target)
	info.FileNameLength = uint32((len(target) - 1) * 2)
	if replace {
		// Windows refuses to replace a read-only file unless asked to
		// ignore the attribute, and a record is first written read-only.
		info.Flags = windows.FILE_RENAME_REPLACE_IF_EXISTS | windows.FILE_RENAME_POSIX_SEMANTICS | windows.FILE_RENAME_IGNORE_READONLY_ATTRIBUTE
	}
	size := uint32(unsafe.Sizeof(info))
	err = windows.NtSetInformationFile(h, &status, (*byte)(unsafe.Pointer(&info)), size, fileRenameInformationEx)
	if err != nil && !errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) {
		// A file system without the extended form, such as FAT, takes the
		// plain one, whose first field is a byte that says replace.
		info.Flags = 0
		if replace {
			info.Flags = 1
		}
		err = windows.NtSetInformationFile(h, &status, (*byte)(unsafe.Pointer(&info)), size, windows.FileRenameInformation)
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, windows.STATUS_OBJECT_NAME_COLLISION) && !replace:
		return existsError(dest)
	}
	return &os.LinkError{Op: "rename", Old: from, New: dest, Err: ntError(err)}
}

// ntError gives a status as the Windows error Go's own calls return, so a
// caller can classify it as it classifies theirs.
func ntError(err error) error {
	var status windows.NTStatus
	if errors.As(err, &status) {
		return status.Errno()
	}
	return err
}

// linkAt is never called on Windows: snapshots copy there, so no tree shares
// a file whose read-only attribute a deletion could clear.
func (d *dirHandle) linkAt(_ string, _ *dirHandle, _, from, dest string) error {
	return &os.LinkError{Op: "link", Old: from, New: dest, Err: errors.ErrUnsupported}
}

// sync does nothing on Windows, as syncDirEntries does not.
func (d *dirHandle) sync() error { return nil }

// syncFileHandle does nothing on Windows, as SyncFile does not.
func syncFileHandle(*os.File) error { return nil }
