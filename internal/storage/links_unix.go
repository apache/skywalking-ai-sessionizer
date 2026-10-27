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
	"crypto/rand"
	"errors"
	"path/filepath"
	"syscall"
)

// sharesLinks is true where snapshots share unchanged files through hard
// links. Unlink on these platforms leaves a surviving link's mode alone
// (src/os/file_unix.go:357), which is what makes sharing safe.
const sharesLinks = true

// linkUnavailable decides whether a failed link permits a copy instead.
// Another device (EXDEV) and the link limit (EMLINK) always do. EPERM, ENOTSUP
// and EOPNOTSUPP do only when the file system has no hard links at all, as FAT
// and exFAT do not: on Linux EPERM also means a link refused for permission,
// and that must stop the operation. A probe in dir tells the two apart.
func linkUnavailable(err error, dir string, files FileSystem) (bool, error) {
	if errors.Is(err, syscall.EXDEV) || errors.Is(err, syscall.EMLINK) {
		return true, nil
	}
	if !withoutLinks(err) {
		return false, nil
	}
	works, err := linksWork(dir, files)
	return !works, err
}

func withoutLinks(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}

// linksWork tells whether the file system holding dir has hard links. A test
// replaces it to stand for FAT or exFAT.
var linksWork = probeLinks

// probeLinks links a fresh file of its own in dir, through files so a test sees
// it. Only an answer that says the file system lacks links reports false; any
// other link failure reports true, so the original error stops the caller
// rather than turning into a copy. A probe it cannot clean up is an error.
func probeLinks(dir string, files FileSystem) (bool, error) {
	files = writeFileSystem(files)
	probe := filepath.Join(dir, reservedPrefix+rand.Text())
	file, err := files.Create(probe, 0o600)
	if err != nil {
		return true, err
	}
	err = file.Close()
	linked := probe + "-link"
	linkErr := files.Link(probe, linked)
	if linkErr == nil {
		err = errors.Join(err, files.Remove(linked))
	}
	err = errors.Join(err, files.Remove(probe))
	return linkErr == nil || !withoutLinks(linkErr), err
}
