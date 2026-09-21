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

"""Turns the loader on for this environment, so an application imports nothing.

Python runs a .pth file only when it sits in a site directory, and a wheel
cannot put one there: measured, `pip install` places a declared data file in
the environment's root, where nothing reads it. So the file is written here
instead, by a command run once against the environment it belongs to.

    asz-langchain enable     write the loader into this environment
    asz-langchain status     say whether it is there
    asz-langchain disable    take it out again
"""

import os
import site
import sys

NAME = "asz_langchain.pth"
LINE = "import asz_langchain_boot\n"


def site_dir():
    """The site directory of the running interpreter.

    getsitepackages is absent on some interpreters, so the directory holding
    this package is the fallback: it is where the loader has to sit, and it is
    a site directory by definition, because this package was imported from it.
    """
    try:
        for d in site.getsitepackages():
            if os.path.isdir(d):
                return d
    except AttributeError:
        pass
    return os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def path():
    return os.path.join(site_dir(), NAME)


def enable():
    target = path()
    with open(target, "w") as f:
        f.write(LINE)
    print("asz-langchain: wrote %s" % target)
    print("asz-langchain: set ASZ_CHANGES=true and ASZ_WATCH to record file changes")
    return 0


def disable():
    target = path()
    if os.path.exists(target):
        os.remove(target)
        print("asz-langchain: removed %s" % target)
    else:
        print("asz-langchain: nothing to remove at %s" % target)
    return 0


def status():
    target = path()
    if not os.path.exists(target):
        print("asz-langchain: not enabled; %s is not there" % target)
        return 1
    on = os.environ.get("ASZ_CHANGES", "").strip().lower() in ("1", "true", "yes", "on")
    print("asz-langchain: enabled at %s" % target)
    print("asz-langchain: ASZ_CHANGES is %s" % ("on" if on else "off, so nothing is recorded"))
    return 0


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    command = argv[0] if argv else "status"
    if command == "enable":
        return enable()
    if command == "disable":
        return disable()
    if command == "status":
        return status()
    print(__doc__.strip(), file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main())
