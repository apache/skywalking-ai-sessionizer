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

"""Runs one case, then waits for the client to send what it is holding.

The client sends from a background thread, so a process that exits the moment
the graph returns takes its unsent runs with it. Flushing here is what makes a
capture complete; the case that deliberately does not flush is the one that
proves what a crash leaves behind.
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import agents  # noqa: E402


def main(name):
    agents.CASES[name]()
    from langsmith import Client

    Client().flush()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
