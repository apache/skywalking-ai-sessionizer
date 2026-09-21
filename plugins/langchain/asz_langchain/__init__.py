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

"""Records what each of a LangChain agent's tool calls changed on disk.

The application never imports this. Installing it puts a .pth file beside it,
so the loader runs at interpreter start and attaches this to every LangChain
run in the process once ASZ_CHANGES=true is set.

It holds no logic of its own. LangChain has no way to say "a tool started" to
anything outside the process, so this says it: it runs asz-changes, which is
the same program the Claude Code plugin runs and does all the scanning and
diffing. This file exists because LangChain has no configuration-only hook and
Claude Code does.

    ASZ_CHANGES=true             attach at all
    ASZ_WATCH=/path              what to watch; the working directory otherwise
    ASZ_CHANGES_DATA=/path       where asz-changes keeps its state and output
    ASZ_CHANGES_BIN=/path/to     the program, when it is not on PATH
"""

import json
import logging
import os
import shutil
import subprocess

from langchain_core.callbacks.base import BaseCallbackHandler

from .identity import owner_values, storage_id

log = logging.getLogger("asz_langchain")

__all__ = ["AszChanges", "register"]


def _binary():
    """Where asz-changes is, or None.

    ASZ_CHANGES_BIN first, then PATH, so a machine that installed the binary
    through Homebrew, apt or the wheel ends up with one copy however it got
    there.
    """
    named = os.environ.get("ASZ_CHANGES_BIN")
    if named:
        return named if os.path.isfile(named) else None
    return shutil.which("asz-changes")


# The order the tracing client resolves a project in, read from its own
# get_tracer_project: a hosted deployment's name wins, then LANGSMITH_ or
# LANGCHAIN_PROJECT, then the legacy SESSION pair, and "default" when none is
# set. The receiver reads the project the client puts in each run, so this has
# to reach the same answer or the change records go to a session that has no
# conversation in it. Reading only LANGSMITH_PROJECT gave "" where the client
# gave "default", which is a different session.
# The hosted name is read with a plain lookup, so any value set wins, even a
# blank one. The rest go through the client's own helper, which skips a value
# that is empty or all spaces and tries the next. Treating them alike either
# way puts the change records in a session the conversation is not in.
_HOSTED_VAR = "HOSTED_LANGSERVE_PROJECT_NAME"
_PROJECT_VARS = (
    _HOSTED_VAR,
    "LANGSMITH_PROJECT", "LANGCHAIN_PROJECT",
    "LANGSMITH_SESSION", "LANGCHAIN_SESSION",
)


def _project():
    """The project the tracing client will report, resolved its way."""
    hosted = os.environ.get(_HOSTED_VAR)
    if hosted is not None:
        return hosted
    for name in _PROJECT_VARS[1:]:
        value = os.environ.get(name)
        if value is not None and value.strip() != "":
            return value
    return "default"


def _list(name):
    """A comma-separated setting, or None when it is not set.

    Empty entries are dropped, so a trailing comma is not a dimension with
    no name.
    """
    raw = os.environ.get(name, "")
    parts = [p.strip() for p in raw.split(",") if p.strip()]
    return parts or None


class AszChanges(BaseCallbackHandler):
    """Tells asz-changes when a tool call begins and ends."""

    def __init__(self):
        self.root = os.environ.get("ASZ_WATCH") or os.getcwd()
        self.project = _project()
        # The receiver's own two settings. They are read here rather than
        # assumed, because the session name is the only thing the shim and
        # the receiver share, and a root configured with a different thread
        # key joined nothing at all while looking like it worked.
        self.keys = _list("ASZ_THREAD_KEYS")
        self.scope = _list("ASZ_SCOPE")
        self.binary = _binary()
        self._said = False
        # The session a change record belongs to is worked out at tool start,
        # where the metadata is, and remembered for the end, where it is not.
        self._open = {}

    # --- the two events ---------------------------------------------------

    def on_tool_start(self, serialized, input_str, *, run_id, parent_run_id=None,
                      tags=None, metadata=None, inputs=None, **kwargs):
        values = owner_values(metadata=metadata, project=self.project,
                              keys=self.keys, scope=self.scope)
        if values is None:
            # No supplied identity, so nothing this records could be filed
            # under a conversation. Recording it anywhere else would be
            # inventing one.
            return
        session = storage_id(values)
        name = kwargs.get("name") or (serialized or {}).get("name") or "tool"
        call = kwargs.get("tool_call_id") or str(run_id)
        self._open[str(run_id)] = (session, name, call)
        self._send("tool.begin", session, name, call, inputs or {})

    def on_tool_end(self, output, *, run_id, parent_run_id=None, **kwargs):
        self._finish("tool.end", run_id, output)

    def on_tool_error(self, error, *, run_id, parent_run_id=None, **kwargs):
        self._finish("tool.failed", run_id, error)

    # --- the one thing it does -------------------------------------------

    def _finish(self, event, run_id, result):
        opened = self._open.pop(str(run_id), None)
        if opened is None:
            return
        session, name, call = opened
        self._send(event, session, name, call, {}, result)

    def _send(self, event, session, tool, call, inputs, result=None):
        if self.binary is None:
            if not self._said:
                self._said = True
                log.warning("asz_langchain: asz-changes is not on PATH and "
                            "ASZ_CHANGES_BIN is unset; no file changes are recorded")
            return
        payload = {
            "hook_event_name": event,
            "session_id": session,
            "cwd": self.root,
            "tool_name": tool,
            "tool_use_id": call,
            "tool_input": inputs if isinstance(inputs, dict) else {},
        }
        if result is not None:
            payload["tool_response"] = {"output": str(result)[:4096]}
        try:
            subprocess.run([self.binary, "hook"], input=json.dumps(payload).encode(),
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                           timeout=60, check=False)
        except Exception:
            # Never break the agent. A change record that did not happen is a
            # gap in observation; an exception here would be a gap in the
            # application.
            log.debug("asz_langchain: %s could not be recorded", event, exc_info=True)


def register():
    """Attaches this to every LangChain run in the process.

    LangChain's own mechanism: a configure hook bound to an environment
    variable. Importing this module is what arms it, which is why the loader
    imports nothing until the variable is set.
    """
    from contextvars import ContextVar

    from langchain_core.tracers.context import register_configure_hook

    var = ContextVar("asz_changes", default=None)
    register_configure_hook(var, True, AszChanges, "ASZ_CHANGES")


register()
