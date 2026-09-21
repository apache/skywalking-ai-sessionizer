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

"""Runs at interpreter start, and must cost nothing when the plugin is off.

The .pth file beside this imports it in EVERY Python process in the
environment: pip, a shell script, an unrelated tool. Importing langchain_core
takes about 0.2 s, measured, so doing it here unconditionally would tax all of
them. Reading one environment variable does not.
"""

import os

if os.environ.get("ASZ_CHANGES", "").strip().lower() in ("1", "true", "yes", "on"):
    try:
        import asz_langchain  # noqa: F401  attaches the handler
    except Exception:  # never break the host program
        pass
