#!/bin/sh
# Licensed to the Apache Software Foundation (ASF) under one
# or more contributor license agreements.  See the NOTICE file
# distributed with this work for additional information
# regarding copyright ownership.  The ASF licenses this file
# to you under the Apache License, Version 2.0 (the
# "License"); you may not use this file except in compliance
# with the License.  You may obtain a copy of the License at
#
#   http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing,
# software distributed under the License is distributed on an
# "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
# KIND, either express or implied.  See the License for the
# specific language governing permissions and limitations
# under the License.

# Lists what a Debian package installs, one path per line with no leading
# ./ or /, and a directory with a trailing slash. With --control, it prints
# the package's control file instead, which names the package, its version
# and its architecture.
#
#   tools/release/deb-list.sh [--control] PACKAGE.deb
#
# A .deb is an ar archive that holds a tar archive of the files. macOS has no
# dpkg-deb, and GNU tar cannot read ar, so Python reads both, as it is
# already needed by the release scripts.
set -eu
part=data
if [ "${1:-}" = --control ]; then part=control; shift; fi
[ "$#" -eq 1 ] || { echo "usage: tools/release/deb-list.sh [--control] PACKAGE.deb" >&2; exit 2; }
command -v python3 >/dev/null 2>&1 || { echo "deb-list: python3 is required" >&2; exit 1; }
exec python3 - "$part" "$1" <<'PY'
import io, sys, tarfile

part, path = sys.argv[1:]

def fail(message):
    print("deb-list: %s: %s" % (path, message), file=sys.stderr)
    sys.exit(1)

with open(path, "rb") as source:
    data = source.read()
if not data.startswith(b"!<arch>\n"):
    fail("not a Debian package")
offset, members = 8, {}
while offset + 60 <= len(data):
    header = data[offset:offset + 60]
    name = header[:16].decode("ascii", "replace").strip().rstrip("/")
    try:
        size = int(header[48:58].decode("ascii").strip())
    except ValueError:
        fail("a damaged ar member header")
    members[name] = data[offset + 60:offset + 60 + size]
    offset += 60 + size + size % 2
if members.get("debian-binary") != b"2.0\n":
    fail("its debian-binary member is not 2.0")
names = [n for n in members if n.startswith(part + ".tar")]
if len(names) != 1:
    fail("it has no single %s.tar member" % part)
with tarfile.open(fileobj=io.BytesIO(members[names[0]])) as archive:
    if part == "control":
        for entry in archive.getmembers():
            if entry.name in ("./control", "control"):
                sys.stdout.write(archive.extractfile(entry).read().decode("utf-8"))
                sys.exit(0)
        fail("it has no control file")
    for entry in archive.getmembers():
        path = entry.name[2:] if entry.name.startswith("./") else entry.name.lstrip("/")
        if path in ("", "."):
            continue
        print(path + "/" if entry.isdir() else path)
PY
