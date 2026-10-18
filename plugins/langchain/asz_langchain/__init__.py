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
Claude Code does. It also watches LangChain's tracer start each tool run, for
the project the run is in, which a callback is not shown, so the changes are
filed with the conversation.

    ASZ_CHANGES=true             attach at all
    ASZ_WATCH=/path              what to watch; the working directory otherwise
    ASZ_CHANGES_DATA=/path       where asz-changes keeps its state and output
    ASZ_CHANGES_BIN=/path/to     the program, when it is not on PATH
"""

import functools
import inspect
import json
import logging
import os
import shutil
import subprocess
import threading
import time

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


# Which project a tool's run is traced under is the tracer's alone, whether
# the tracer was passed in code, set by tracing_v2_enabled or made by
# LangChain, and a callback is not shown the tracer. Reading the run above or
# the context instead filed a tool under a project its tracer did not use. So
# the tracer's own start of each tool run is watched: it returns the run, and
# the run names its project.
#
# The tracer and this plugin are told of a tool's start in either order: the
# tracer first when it was there before LangChain attached the plugin, the
# plugin first when the tracer comes below it. Both are told before the tool
# runs. So the begin, whose scan has to come before the tool runs, is sent by
# whichever is told second, with the tracer's project. Sending it at once with
# a guessed project filed it in the wrong session, and asz-changes keeps the
# session the begin named for the record it writes at the end.
#
# _runs holds, by run id, what the first of the two left for the second, and
# when: the project, or a begin waiting for it. Both halves of a start come
# within one start of one tool, so an entry older than _LIFE seconds is one
# whose other half will not come: the plugin is not attached, or the tool was
# cancelled between the two and no end follows. It is dropped, with the
# handler and the input a waiting begin holds, before anything reads it. A
# tool whose start took longer records nothing. _LIMIT bounds the count too.
_runs = {}
_LIFE = 600.0
_LIMIT = 1 << 16
_now = time.monotonic
# _finished holds the runs seen from both a plugin and a tracer, whether or not
# a begin was sent for them, and the runs whose waiting half was dropped, in
# the order they finished. A later start of a finished run, from a second
# plugin or tracer of the same dispatch, does nothing, so a run has one begin
# at most and an expired one is never made again. It is kept by count
# alone, never by time or by whether a handler is still alive: a tool
# cancelled while LangChain's worker threads still run its callbacks lets go
# of its handler first. Measured with Python 3.12, each id costs about 108
# bytes, a 77-byte string and 31 bytes of the dict, so the limit holds about
# 11 MB.
#
# That bound is a limit on what this can tell. LangChain tells every handler
# of a tool's start together, in one dispatch before the tool runs, so its
# callbacks lag behind one another only while one of them is held up. Held up
# until _FINISHED_LIMIT other runs have entered this set, whether or not their
# tools have ended, the run is no longer known to be finished: a second plugin
# and tracer of the same dispatch can then send a second begin, under their own
# project, and a half that expired can be completed. Telling them apart over
# any lag would need memory without bound.
_finished = {}
_FINISHED_LIMIT = 100_000
# _OPEN_LIMIT bounds the tools one handler keeps open between their begin and
# their end. LangChain cancels an asynchronous tool without telling any
# handler it ended or failed, so a handler used for many tools would keep
# every cancelled one for ever. Past the bound the oldest open tool is
# dropped, and its end record is not written.
_OPEN_LIMIT = 1 << 16
_lock = threading.Lock()
# _watching says the tracer's start is watched. When it cannot be, the begin
# is sent at once under the environment's project, as it was before.
_watching = False


def _finish(run_id):
    """Marks a run finished, dropping the oldest past the limit."""
    _runs.pop(run_id, None)
    _finished[run_id] = None
    while len(_finished) > _FINISHED_LIMIT:
        del _finished[next(iter(_finished))]


def _expire():
    """Finishes the oldest entries of _runs past their life or the limit."""
    now = _now()
    while _runs:
        oldest = next(iter(_runs))
        if len(_runs) <= _LIMIT and now - _runs[oldest][1] <= _LIFE:
            break
        _finish(oldest)


def _tracer_started(run_id, project):
    """A tracer started a tool run in project. Returns the begin waiting for
    it when the plugin was told first, to be sent now."""
    with _lock:
        _expire()
        if run_id in _finished:
            return None
        state = _runs.get(run_id)
        if state is None:
            _runs[run_id] = (project, _now())
        elif callable(state[0]):
            _finish(run_id)
            return state[0]
    # A second tracer of the same run finds the first tracer's project kept:
    # the first project decides.
    return None


def _plugin_told(run_id, begin):
    """The plugin was told a tool run started. Returns the project when a
    tracer started the run first. Otherwise begin waits for the tracer, unless
    another plugin's begin already waits."""
    with _lock:
        _expire()
        if run_id in _finished:
            return None
        state = _runs.get(run_id)
        if state is None:
            _runs[run_id] = (begin, _now())
        elif isinstance(state[0], str):
            _finish(run_id)
            return state[0]
    return None


def _plugin_done(run_id):
    """The tool ended. A begin still waiting is dropped and never sent: no
    tracer traced the run, so which conversation it is in is not known. A
    finished run stays finished."""
    with _lock:
        _runs.pop(run_id, None)


def _watch(tracer):
    """Watches a tracer class's start of each tool run, keeping the coroutine
    function or plain function it is. Returns False, changing nothing, when
    the start is missing or is not a function, so it cannot be wrapped as it
    is."""
    started = getattr(tracer, "on_tool_start", None)
    if getattr(started, "_asz_watches", False):
        return True
    if not inspect.isfunction(started):
        return False

    def seen(run):
        try:
            if (getattr(run, "extra", None) or {}).get("__disabled"):
                # Started inside tracing_context(enabled=False): the tracer
                # sends none of it, so it is a run this tracer does not trace.
                return
            project = run.session_name
            if not isinstance(project, str) or not project:
                project = _project()
            waiting = _tracer_started(str(run.id), project)
            if waiting is not None:
                waiting(project)
        except Exception:
            # Never break the tracer, or the agent behind it.
            log.debug("asz_langchain: a tool's traced project could not be read", exc_info=True)

    if inspect.iscoroutinefunction(started):
        # LangChain awaits a callback only when it is a coroutine function.
        @functools.wraps(started)
        async def on_tool_start(self, *args, **kwargs):
            run = await started(self, *args, **kwargs)
            seen(run)
            return run
    else:
        @functools.wraps(started)
        def on_tool_start(self, *args, **kwargs):
            run = started(self, *args, **kwargs)
            seen(run)
            return run

    on_tool_start._asz_watches = True
    tracer.on_tool_start = on_tool_start
    return True


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
        # The callbacks of tools run at once come from several threads.
        self._open = {}
        self._open_lock = threading.Lock()

    # --- the two events ---------------------------------------------------

    def on_tool_start(self, serialized, input_str, *, run_id, parent_run_id=None,
                      tags=None, metadata=None, inputs=None, **kwargs):
        name = kwargs.get("name") or (serialized or {}).get("name") or "tool"
        call = kwargs.get("tool_call_id") or str(run_id)

        def begin(project):
            values = owner_values(metadata=metadata, project=project,
                                  keys=self.keys, scope=self.scope)
            if values is None:
                # No supplied identity, so nothing this records could be
                # filed under a conversation. Recording it anywhere else
                # would be inventing one.
                return
            session = storage_id(values)
            with self._open_lock:
                self._open[str(run_id)] = (session, name, call)
                while len(self._open) > _OPEN_LIMIT:
                    del self._open[next(iter(self._open))]
            self._send("tool.begin", session, name, call, inputs or {})

        if not _watching:
            begin(self.project)
            return
        project = _plugin_told(str(run_id), begin)
        if project is not None:
            begin(project)

    def on_tool_end(self, output, *, run_id, parent_run_id=None, **kwargs):
        self._finish("tool.end", run_id, output)

    def on_tool_error(self, error, *, run_id, parent_run_id=None, **kwargs):
        self._finish("tool.failed", run_id, error)

    # --- the one thing it does -------------------------------------------

    def _finish(self, event, run_id, result):
        _plugin_done(str(run_id))
        with self._open_lock:
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
    global _watching
    try:
        from langchain_core.tracers.langchain import LangChainTracer

        _watching = _watch(LangChainTracer)
    except Exception:
        _watching = False
    if not _watching:
        log.warning("asz_langchain: LangChain's tracer cannot be watched here; "
                    "changes are filed under the project the environment names")


register()
