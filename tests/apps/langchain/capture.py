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

"""Reads a captured request back into its parts.

A capture holds the body as it arrived and the headers beside it, so a reader
splits it exactly as the receiver will have to. Everything that reads a fixture
goes through here, so there is one way to do it.
"""

import json
import os
import re

import recorder


def requests(case_directory):
    """Yields (meta, parts) for each request of a case, in arrival order."""
    for name in sorted(os.listdir(case_directory)):
        directory = os.path.join(case_directory, name)
        meta_path = os.path.join(directory, "meta.json")
        if not os.path.isfile(meta_path):
            continue
        meta = json.load(open(meta_path))
        body = open(os.path.join(directory, "body.bin"), "rb").read()
        content_type = meta["headers"].get("Content-Type", "")
        match = re.search(r"boundary=([^;]+)", content_type)
        parts = recorder.parts_of(body, match.group(1).strip('"')) if match else []
        yield meta, parts


def runs(case_directory):
    """Yields (operation, run id, meta, fields) for every run in a case.

    `fields` maps `_run`, `inputs`, `outputs`, `extra`, `events` and
    `serialized` to the decoded value and the bytes it took on the wire.
    """
    for meta, parts in requests(case_directory):
        collected = {}
        for name, _headers, payload in parts:
            segments = name.split(".")
            if len(segments) < 2:
                continue
            field = segments[2] if len(segments) > 2 else "_run"
            collected.setdefault((segments[0], segments[1]), {})[field] = (
                json.loads(payload), len(payload))
        for (operation, run_id), fields in collected.items():
            yield operation, run_id, meta, fields
