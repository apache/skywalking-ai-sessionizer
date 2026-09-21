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

"""Stands in for the asz receiver and keeps what the client sent, byte for byte.

It answers what the client accepts and nothing more: `GET /info` deciding the
batch configuration, and `POST /runs/multipart` taking the runs. It deliberately
does not advertise `zstd_compression_enabled`, which is what keeps the client
from compressing, so the capture is the plain multipart body.

Every request is written whole, with its headers and its parts, so a fixture
holds what arrived rather than what a reader made of it.
"""

import gzip
import json
import os
import re
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = 8930

# What a fixture must not carry. A traceback names the file it was raised in,
# so a capture taken on someone's machine holds their home directory unless it
# is taken out on the way to disk. The replacement is fixed, so two machines
# capture the same bytes.
REDACTIONS = [
    (os.path.dirname(os.path.abspath(__file__)).encode(), b"/asz/harness"),
    (os.path.expanduser("~").encode(), b"/asz/home"),
]


def redact(data):
    """Replaces machine-specific paths with fixed ones."""
    for was, now in REDACTIONS:
        if was and was != b"/":
            data = data.replace(was, now)
    return data

_count = 0
_lock = threading.Lock()
_started = time.time()


def decode(body, encoding):
    """Returns the request body and how it was encoded."""
    if not encoding:
        return body, "none"
    encoding = encoding.lower()
    if encoding == "gzip":
        return gzip.decompress(body), "gzip"
    if encoding == "zstd":
        import zstandard

        return zstandard.ZstdDecompressor().decompressobj().decompress(body), "zstd"
    return body, "unknown:" + encoding


def parts_of(body, boundary):
    """Splits a multipart body, keeping each part's headers as they arrived."""
    out = []
    for chunk in body.split(b"--" + boundary.encode()):
        if chunk in (b"", b"--", b"--\r\n", b"\r\n"):
            continue
        chunk = chunk.lstrip(b"\r\n")
        if chunk.startswith(b"--"):
            continue
        head, _, payload = chunk.partition(b"\r\n\r\n")
        headers = {}
        for line in head.decode("utf-8", "replace").split("\r\n"):
            if ":" in line:
                key, value = line.split(":", 1)
                headers[key.strip()] = value.strip()
        name = re.search(r'name="([^"]*)"', headers.get("Content-Disposition", ""))
        out.append((name.group(1) if name else "?", headers, payload.rstrip(b"\r\n")))
    return out


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    @property
    def out(self):
        return os.environ.get("ASZ_CAPTURE", "capture")

    def do_GET(self):
        # The client asks what this server can take before every send. Leaving
        # zstd_compression_enabled out is what keeps the body uncompressed.
        body = json.dumps({
            "version": "0.0.0-asz-harness",
            "license_expiration_time": None,
            "batch_ingest_config": {
                "use_multipart_endpoint": True,
                "scale_up_qsize_trigger": 1000,
                "scale_up_nthreads_limit": 16,
                "scale_down_nempty_trigger": 4,
                "size_limit": int(os.environ.get("ASZ_SIZE_LIMIT", "100")),
                "size_limit_bytes": 20971520,
            },
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        self.keep("POST")

    def do_PATCH(self):
        self.keep("PATCH")

    def keep(self, method):
        global _count
        length = int(self.headers.get("Content-Length") or 0)
        wire = self.rfile.read(length) if length else b""
        with _lock:
            _count += 1
            index = _count
        body, encoding = decode(wire, self.headers.get("Content-Encoding"))
        directory = os.path.join(self.out, "%03d-%s" % (index, method))
        os.makedirs(directory, exist_ok=True)
        meta = {
            "n": index,
            "method": method,
            "path": self.path,
            "at": round(time.time() - _started, 2),
            "headers": dict(self.headers),
            "wire_bytes": len(wire),
            "body_bytes": len(body),
            "encoding": encoding,
            "redacted": True,
        }
        meta["body_bytes"] = len(body)
        # The body is kept as it arrived, in one file. Splitting it into a file
        # per part turned 2.7 MB of evidence into 8 MB on disk, because a part
        # is usually smaller than a block. Readers split it the same way this
        # did, from the boundary in the headers.
        body = redact(body)
        with open(os.path.join(directory, "body.bin"), "wb") as f:
            f.write(body)
        content_type = self.headers.get("Content-Type", "")
        if "multipart/" in content_type:
            boundary = re.search(r"boundary=([^;]+)", content_type).group(1).strip('"')
            meta["parts"] = [{"name": name, "headers": headers, "bytes": len(payload)}
                             for name, headers, payload in parts_of(body, boundary)]
        with open(os.path.join(directory, "meta.json"), "w") as f:
            json.dump(meta, f, indent=2, sort_keys=True)
        names = [p["name"] for p in meta.get("parts", [])]
        print("%03d %s %s at=%5.2fs encoding=%s bytes=%d parts=%d post=%d patch=%d"
              % (index, method, self.path, meta["at"], encoding, len(body), len(names),
                 sum(1 for n in names if n.startswith("post.")),
                 sum(1 for n in names if n.startswith("patch."))), flush=True)
        payload = json.dumps({"message": "accepted"}).encode()
        self.send_response(202)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def serve():
    os.makedirs(os.environ.get("ASZ_CAPTURE", "capture"), exist_ok=True)
    ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()


if __name__ == "__main__":
    serve()
