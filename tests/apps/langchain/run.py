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

"""Runs the demo application against the stand-in model and the recorder.

One case per process, so a capture holds one case and nothing else, and a case
that ends a process early is a case like any other rather than a special path.

    python run.py                 every case
    python run.py subagent loop   only those
    python run.py -l              what the cases are
"""

import os
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
TESTDATA = os.path.join(HERE, "testdata")
MODEL = "http://127.0.0.1:8931/v1/chat/completions"
RECEIVER = "http://127.0.0.1:8930"


def wait(url, seconds=20):
    deadline = time.time() + seconds
    while time.time() < deadline:
        try:
            urllib.request.urlopen(url, timeout=1).read()
            return True
        except urllib.error.HTTPError:
            return True
        except Exception:
            time.sleep(0.1)
    return False


def serve(module):
    import importlib

    threading.Thread(target=importlib.import_module(module).serve, daemon=True).start()


def run_case(name):
    """Runs one case in its own process, with its own capture directory."""
    out = os.path.join(TESTDATA, name)
    subprocess.run(["rm", "-rf", out], check=False)
    os.makedirs(out, exist_ok=True)
    environment = dict(os.environ)
    environment.update({
        "ASZ_CAPTURE": out,
        "LANGSMITH_TRACING": "true",
        "LANGSMITH_ENDPOINT": RECEIVER,
        "LANGSMITH_API_KEY": "asz-harness",
        "LANGSMITH_PROJECT": "asz-harness",
        "NO_PROXY": "*",
        "no_proxy": "*",
        "ASZ_CASE": name,
    })
    for proxy in ("http_proxy", "https_proxy", "all_proxy",
                  "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        environment.pop(proxy, None)
    # The recorder runs in this process, so it takes the case's directory from
    # here. One case runs at a time, which is what makes that safe.
    import recorder

    os.environ["ASZ_CAPTURE"] = out
    recorder._count = 0
    started = time.time()
    done = subprocess.run([sys.executable, os.path.join(HERE, "case.py"), name],
                          env=environment, capture_output=True, text=True)
    took = time.time() - started
    status = "ok" if done.returncode == 0 else "exit %d" % done.returncode
    requests = len([d for d in os.listdir(out) if os.path.isdir(os.path.join(out, d))])
    print("%-22s %-8s %5.1fs  %d request(s)" % (name, status, took, requests))
    if done.returncode != 0:
        for line in (done.stderr or "").strip().splitlines()[-3:]:
            print("    %s" % line)
    return done.returncode == 0


def main(argv):
    import agents

    if "-l" in argv:
        for name, function in agents.CASES.items():
            print("%-22s %s" % (name, (function.__doc__ or "").splitlines()[0]))
        return 0
    wanted = [a for a in argv if not a.startswith("-")] or list(agents.CASES)
    unknown = [w for w in wanted if w not in agents.CASES]
    if unknown:
        print("unknown case(s): %s" % ", ".join(unknown), file=sys.stderr)
        return 2

    serve("model")
    serve("recorder")
    if not wait(MODEL) or not wait(RECEIVER + "/info"):
        print("the stand-in servers did not start", file=sys.stderr)
        return 1
    print("stand-in model on 8931, receiver on 8930; capturing into testdata/\n")
    failed = [name for name in wanted if not run_case(name)]
    print("\n%d case(s), %d failed" % (len(wanted), len(failed)))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.path.insert(0, HERE)
    sys.exit(main(sys.argv[1:]))
