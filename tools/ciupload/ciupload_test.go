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

package ciupload_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPrereleaseIsReadyOnlyAfterEveryUploadedByteIsVerified(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CI upload fixtures use POSIX tools")
	}
	for _, name := range []string{"bash", "python3", "tar"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("CI upload fixture needs %s", name)
		}
	}
	for _, mode := range []string{"complete", "existing", "corrupt-download", "moved-tag", "swapped-assets"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			packages := filepath.Join(dir, "packages")
			remote := filepath.Join(dir, "remote")
			for _, p := range []string{bin, packages, remote} {
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			var data bytes.Buffer
			gz := gzip.NewWriter(&data)
			tw := tar.NewWriter(gz)
			body := []byte("CI binary bytes")
			if err := tw.WriteHeader(&tar.Header{Name: "asz", Mode: 0o755, Size: int64(len(body))}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			name := "apache-skywalking-ai-sessionizer-0.3.0-bin-linux-amd64.tgz"
			if err := os.WriteFile(filepath.Join(packages, name), data.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			sum := fmt.Sprintf("%x  %s\n", sha512.Sum512(data.Bytes()), name)
			if err := os.WriteFile(filepath.Join(packages, name+".sha512"), []byte(sum), 0o644); err != nil {
				t.Fatal(err)
			}
			release := map[string]any{"id": 789, "tag_name": "v0.3.0", "name": "0.3.0", "draft": false, "prerelease": true, "body": "Developer review only. Not an Apache release.", "assets": []any{}}
			if mode == "existing" {
				release["assets"] = []any{map[string]any{"name": "old-asset"}}
			}
			state, _ := json.Marshal(release)
			if err := os.WriteFile(filepath.Join(dir, "release.json"), state, 0o644); err != nil {
				t.Fatal(err)
			}
			stub := `#!/usr/bin/env python3
import hashlib, json, os, pathlib, shutil, sys
root=pathlib.Path(os.environ["CI_UPLOAD_FIXTURE"])
state=root/"release.json"
r=json.loads(state.read_text())
a=sys.argv[1:]
mode=os.environ["CI_UPLOAD_MODE"]
def save(): state.write_text(json.dumps(r))
def log(message):
    with (root/"writes").open("a") as f: f.write(message+"\n")
if a[0]=="api":
    endpoint=a[-1]
    if "/git/ref/tags/" in endpoint:
        count_path=root/"tag-count"
        count=int(count_path.read_text()) if count_path.exists() else 0
        count_path.write_text(str(count+1))
        sha=("b" if mode=="moved-tag" and count else "a")*40
        print(json.dumps({"object":{"type":"commit","sha":sha}}))
    elif "/releases/tags/" in endpoint: print(json.dumps(r))
    else: sys.exit(9)
elif a[:2]==["release","upload"]:
    log("upload")
    for name in a[5:]:
        path=pathlib.Path(name)
        shutil.copyfile(path,root/"remote"/path.name)
        r["assets"].append({"name":path.name,"id":len(r["assets"])+1,"digest":"sha256:"+hashlib.sha256(path.read_bytes()).hexdigest(),"size":path.stat().st_size})
    save()
elif a[:2]==["release","download"]:
    log("download")
    dest=pathlib.Path(a[a.index("--dir")+1]); dest.mkdir()
    for path in (root/"remote").iterdir(): shutil.copyfile(path,dest/path.name)
    if mode=="swapped-assets":
        r["assets"][0]["id"]+=1000; save()
    if mode=="corrupt-download":
        path=next(dest.glob("*.tgz")); path.write_bytes(path.read_bytes()+b"corrupted")
elif a[:2]==["release","edit"]:
    log("ready")
    r["body"]=pathlib.Path(a[a.index("--notes-file")+1]).read_text()
    save()
else: sys.exit(8)
`
			if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "../ci-upload-binaries.sh", "0.3.0", strings.Repeat("a", 40), "123", "2", "789", packages, "linux/amd64")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "CI_UPLOAD_FIXTURE="+dir, "CI_UPLOAD_MODE="+mode)
			output, err := cmd.CombinedOutput()
			state, readErr := os.ReadFile(filepath.Join(dir, "release.json"))
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := json.Unmarshal(state, &release); err != nil {
				t.Fatal(err)
			}
			ready := strings.Contains(release["body"].(string), "<!-- asz-ci-binaries run_id=123 run_attempt=2 commit="+strings.Repeat("a", 40)+" -->")
			writes, _ := os.ReadFile(filepath.Join(dir, "writes"))
			if mode == "complete" {
				if err != nil || !ready {
					t.Fatalf("upload: %s\n%v; ready=%v", output, err, ready)
				}
				if string(writes) != "upload\ndownload\nready\n" {
					t.Fatalf("publication order: %q", writes)
				}
				got, err := os.ReadFile(filepath.Join(remote, name))
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data.Bytes()) {
					t.Fatal("CI archive changed on upload")
				}
			} else {
				if err == nil || ready {
					t.Fatalf("unsafe upload accepted: %s\n%v; ready=%v", output, err, ready)
				}
				if mode == "existing" && len(writes) > 0 {
					t.Fatalf("existing candidate was modified: %s", writes)
				}
				if strings.Contains(string(writes), "ready") {
					t.Fatal("failed candidate was marked ready")
				}
			}
		})
	}
}
