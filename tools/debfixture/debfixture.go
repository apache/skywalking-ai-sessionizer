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

// Package debfixture writes small Debian packages for the tests of the
// release scripts. tools/debpackage refuses what a test needs to hold, such
// as macOS metadata, so the tests write their own.
package debfixture

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"sort"
)

// Bytes returns a Debian package whose control file is control and whose
// data archive holds files, by path with no leading ./.
func Bytes(control string, files map[string]string) ([]byte, error) {
	var names []string
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := tarGz(names, files)
	if err != nil {
		return nil, err
	}
	meta, err := tarGz([]string{"control"}, map[string]string{"control": control})
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteString("!<arch>\n")
	for _, m := range []struct {
		name string
		body []byte
	}{{"debian-binary", []byte("2.0\n")}, {"control.tar.gz", meta}, {"data.tar.gz", data}} {
		fmt.Fprintf(&out, "%-16s%-12d%-6d%-6d%-8s%-10d`\n", m.name, 0, 0, 0, "100644", len(m.body))
		out.Write(m.body)
		if len(m.body)%2 == 1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes(), nil
}

func tarGz(names []string, files map[string]string) ([]byte, error) {
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, name := range names {
		body := files[name]
		if err := tw.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
