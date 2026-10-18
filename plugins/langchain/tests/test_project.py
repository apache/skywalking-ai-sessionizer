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

"""The project a tool's changes are filed under, against LangChain's own tracer.

A tracer can be given a project in code, and the run goes there whatever the
environment says. The changes have to go to the same project, or they land in
a session the conversation is not in. These tests run a tool through
LangChain's real tracer, with a client that sends nothing and keeps what it was
given, and compare the project the tool's run went to with the session its
changes were filed under. The plugin is attached the way it attaches itself,
by LangChain's configure hook once ASZ_CHANGES is set.

They need langchain_core, and are skipped where it is not installed. Measured
with langchain-core 1.6.3 and langsmith 0.13.0, the versions in
tests/apps/langchain.
"""

import importlib.util
import os
import pathlib
import sys
import unittest

PLUGIN = pathlib.Path(__file__).resolve().parent.parent

_PROJECT_VARS = ("HOSTED_LANGSERVE_PROJECT_NAME", "LANGSMITH_PROJECT", "LANGCHAIN_PROJECT",
                 "LANGSMITH_SESSION", "LANGCHAIN_SESSION")
_TRACING_VARS = ("LANGSMITH_TRACING", "LANGCHAIN_TRACING_V2", "LANGSMITH_TRACING_V2",
                 "LANGCHAIN_TRACING")


class _Quiet:
    """A client that sends nothing and keeps the runs it was asked to send."""

    # langsmith's traceable asks the client whether it exports to OpenTelemetry.
    otel_exporter = None

    def __init__(self):
        self.created = []

    def create_run(self, **run):
        self.created.append(run)

    def update_run(self, *args, **run):
        pass


@unittest.skipUnless(importlib.util.find_spec("langchain_core"), "langchain_core is not installed")
class TestTheTracedProject(unittest.TestCase):
    def setUp(self):
        names = _PROJECT_VARS + _TRACING_VARS + ("ASZ_CHANGES",)
        self.saved = {name: os.environ.pop(name, None) for name in names}
        os.environ["LANGSMITH_PROJECT"] = "env-project"
        os.environ["ASZ_CHANGES"] = "true"
        sys.path.insert(0, str(PLUGIN))
        import asz_langchain
        from langsmith import utils
        # The client reads its project from the environment once and keeps it.
        utils.get_tracer_project.cache_clear()
        utils.get_env_var.cache_clear()
        self.sent = []
        self.handler = asz_langchain.AszChanges
        self.send = self.handler._send
        self.handler._send = lambda handler, event, session, *rest: self.sent.append(session)

    def tearDown(self):
        self.handler._send = self.send
        sys.path.remove(str(PLUGIN))
        for name, value in self.saved.items():
            os.environ.pop(name, None)
            if value is not None:
                os.environ[name] = value
        from langsmith import utils
        utils.get_tracer_project.cache_clear()
        utils.get_env_var.cache_clear()

    def _run(self, invoke):
        """Runs a tool, and returns the project its run went to and the sessions its changes went to."""
        from langchain_core.tools import tool

        @tool
        def look(path: str) -> str:
            """Look at a path."""
            return "looked"

        quiet = _Quiet()
        invoke(look, quiet, {"metadata": {"thread_id": "t"}})
        traced = [r.get("session_name") for r in quiet.created if r.get("run_type") == "tool"]
        self.assertEqual(len(traced), 1, "the tool's run was not traced once")
        self.assertEqual(len(self.sent), 2, "the tool's start and end were not both recorded")
        return traced[0], set(self.sent)

    def _session(self, project):
        from asz_langchain.identity import owner_values, storage_id
        return storage_id(owner_values(project, {"thread_id": "t"}))

    def assertFiledUnder(self, invoke, project):
        traced, sent = self._run(invoke)
        self.assertEqual(traced, project)
        self.assertEqual(sent, {self._session(project)})

    def test_a_tool_inside_a_run_traced_under_a_project_named_in_code(self):
        from langchain_core.runnables import RunnableLambda
        from langchain_core.tracers.langchain import LangChainTracer

        def invoke(look, quiet, config):
            tracer = LangChainTracer(project_name="code-project", client=quiet)
            node = RunnableLambda(lambda x, config: look.invoke({"path": x}, config))
            node.invoke("a", config={**config, "callbacks": [tracer]})

        self.assertFiledUnder(invoke, "code-project")

    def test_a_tool_traced_by_a_tracer_of_its_own(self):
        from langchain_core.tracers.langchain import LangChainTracer

        def invoke(look, quiet, config):
            tracer = LangChainTracer(project_name="code-project", client=quiet)
            look.invoke({"path": "a"}, config={**config, "callbacks": [tracer]})

        self.assertFiledUnder(invoke, "code-project")

    def test_a_tool_under_tracing_v2_enabled(self):
        from langchain_core.tracers.context import tracing_v2_enabled

        def invoke(look, quiet, config):
            with tracing_v2_enabled(project_name="code-project", client=quiet):
                look.invoke({"path": "a"}, config=config)

        self.assertFiledUnder(invoke, "code-project")

    def test_a_tool_under_a_tracing_context(self):
        from langsmith.run_helpers import tracing_context

        def invoke(look, quiet, config):
            with tracing_context(enabled=True, project_name="code-project", client=quiet):
                look.invoke({"path": "a"}, config=config)

        self.assertFiledUnder(invoke, "code-project")

    def test_a_tool_with_no_project_named_in_code(self):
        from langsmith.run_helpers import tracing_context

        def invoke(look, quiet, config):
            with tracing_context(enabled=True, client=quiet):
                look.invoke({"path": "a"}, config=config)

        self.assertFiledUnder(invoke, "env-project")

    def test_a_tool_traced_under_another_project_than_the_run_above_it(self):
        """The tool's own tracer decides, not the run it runs inside.

        A function traced in one project calls the tool inside
        tracing_v2_enabled with another, and the tool's run goes to the
        other. So does a tool a runnable traced in one project calls with a
        tracer of its own.
        """
        from langchain_core.runnables import RunnableLambda
        from langchain_core.tracers.context import tracing_v2_enabled
        from langchain_core.tracers.langchain import LangChainTracer
        from langsmith import traceable
        from langsmith.run_helpers import tracing_context

        def nested(look, quiet, config):
            @traceable(project_name="outer", client=quiet)
            def outer():
                with tracing_v2_enabled(project_name="code-project", client=quiet):
                    look.invoke({"path": "a"}, config=config)

            with tracing_context(enabled=True, client=quiet):
                outer()

        def own_tracer(look, quiet, config):
            inner = LangChainTracer(project_name="code-project", client=quiet)
            node = RunnableLambda(lambda x, config: look.invoke({"path": x}, {**config, "callbacks": [inner]}))
            node.invoke("a", config={**config, "callbacks": [LangChainTracer(project_name="outer", client=quiet)]})

        for invoke in (nested, own_tracer):
            with self.subTest(invoke.__name__):
                self.sent.clear()
                self.assertFiledUnder(invoke, "code-project")

    def test_a_tool_whose_tracer_starts_its_run_after_the_plugin_is_told(self):
        """A runnable with no tracer attaches the plugin, and the tool's tracer
        comes below it, so the tracer starts the tool's run after the plugin
        is told of it. The begin waits for the tracer, which starts the run
        before the tool runs, and is filed under the tracer's project. Read
        from the context instead, a tracer LangChain does not use was taken:
        a run bound to a tracer in one project, entering tracing_v2_enabled
        with another, keeps its own tracer for the tool."""
        from langchain_core.runnables import RunnableLambda
        from langchain_core.tracers.context import tracing_v2_enabled
        from langchain_core.tracers.langchain import LangChainTracer
        from langsmith import traceable
        from langsmith.run_helpers import tracing_context

        def below(start):
            def invoke(look, quiet, config):
                RunnableLambda(lambda x, config: start(look, quiet, x, config)).invoke("a", config=config)
            return invoke

        def under_tracing_v2_enabled(look, quiet, x, config):
            with tracing_v2_enabled(project_name="code-project", client=quiet):
                return look.invoke({"path": x}, config)

        def under_a_traced_function(look, quiet, x, config):
            @traceable(project_name="code-project", client=quiet)
            def traced():
                return look.invoke({"path": x}, config)

            with tracing_context(enabled=True, client=quiet):
                return traced()

        def under_a_tracing_context(look, quiet, x, config):
            with tracing_context(enabled=True, project_name="code-project", client=quiet):
                return look.invoke({"path": x}, config)

        def under_a_run_bound_to_a_tracer(look, quiet, x, config):
            step = RunnableLambda(lambda y, config: look.invoke({"path": y}, config)).with_config(
                callbacks=[LangChainTracer(project_name="code-project", client=quiet)])
            return step.invoke(x, config)

        def bound_to_a_tracer_and_entering_tracing_v2_enabled(look, quiet, x, config):
            def inside(y, config):
                with tracing_v2_enabled(project_name="another-project", client=quiet):
                    return look.invoke({"path": y}, config)

            step = RunnableLambda(inside).with_config(
                callbacks=[LangChainTracer(project_name="code-project", client=quiet)])
            return step.invoke(x, config)

        def with_a_tracer_bound_to_the_tool(look, quiet, x, config):
            bound = look.with_config(callbacks=[LangChainTracer(project_name="code-project", client=quiet)])
            return bound.invoke({"path": x}, config)

        for start in (under_tracing_v2_enabled, under_a_traced_function, under_a_tracing_context,
                      under_a_run_bound_to_a_tracer, bound_to_a_tracer_and_entering_tracing_v2_enabled,
                      with_a_tracer_bound_to_the_tool):
            with self.subTest(start.__name__):
                self.sent.clear()
                self.assertFiledUnder(below(start), "code-project")

    def test_a_tool_no_tracer_traces_records_nothing(self):
        """No tracer says which project the tool's run is in, and nothing is
        guessed: the begin waits for a tracer that never comes, and the end
        has nothing to close."""
        from langchain_core.tools import tool

        @tool
        def look(path: str) -> str:
            """Look at a path."""
            return "looked"

        look.invoke({"path": "a"}, config={"metadata": {"thread_id": "t"}})
        self.assertEqual(self.sent, [])

    def test_many_tools_at_once_are_each_filed_under_their_project(self):
        """An asynchronous tool's tracer starts its run in the event loop and
        the plugin is told in a worker thread, so with tools run at once many
        tracers can start their runs before the plugin is told of any. Here
        every worker is held until all the tracers have. Each tool's project
        is kept until the plugin takes it: dropping the oldest at 4,096 filed
        four of 4,100 tools under the environment's project."""
        import asyncio
        import threading
        from concurrent.futures import ThreadPoolExecutor

        from langchain_core.tools import tool
        from langchain_core.tracers.langchain import LangChainTracer

        @tool
        async def look(path: str) -> str:
            """Look at a path."""
            return "looked"

        quiet = _Quiet()
        tracer = LangChainTracer(project_name="code-project", client=quiet)
        tools, workers = 4100, 4

        def started():
            return sum(1 for r in quiet.created if r.get("run_type") == "tool")

        async def all_at_once():
            loop = asyncio.get_running_loop()
            loop.set_default_executor(ThreadPoolExecutor(max_workers=workers))
            gate = threading.Event()
            held = [loop.run_in_executor(None, gate.wait) for _ in range(workers)]
            calls = [asyncio.ensure_future(look.ainvoke({"path": str(i)}, config={
                "callbacks": [tracer], "metadata": {"thread_id": "t"}})) for i in range(tools)]
            for _ in range(3000):
                if started() >= tools:
                    break
                await asyncio.sleep(0.01)
            gate.set()
            await asyncio.gather(*held, *calls)

        asyncio.run(all_at_once())
        self.assertEqual(started(), tools)
        self.assertEqual(len(self.sent), 2 * tools)
        self.assertEqual(set(self.sent), {self._session("code-project")})

    def test_a_start_another_wrapper_made_asynchronous_stays_asynchronous(self):
        """LangChain awaits a callback only when it is a coroutine function.
        Wrapped as a plain function, a start another wrapper had made
        asynchronous was called and never awaited, and no tool was traced."""
        import functools

        import asz_langchain
        from langchain_core.tools import tool
        from langchain_core.tracers.langchain import LangChainTracer

        watched = LangChainTracer.on_tool_start
        original = watched.__wrapped__

        @functools.wraps(original)
        async def theirs(self, *args, **kwargs):
            return original(self, *args, **kwargs)

        LangChainTracer.on_tool_start = theirs
        try:
            self.assertTrue(asz_langchain._watch(LangChainTracer))

            @tool
            def look(path: str) -> str:
                """Look at a path."""
                return "looked"

            def invoke(look, quiet, config):
                tracer = LangChainTracer(project_name="code-project", client=quiet)
                look.invoke({"path": "a"}, config={**config, "callbacks": [tracer]})

            self.assertFiledUnder(invoke, "code-project")
        finally:
            LangChainTracer.on_tool_start = watched

    def test_a_start_that_cannot_be_wrapped_is_left_alone(self):
        """A LangChain whose tracer has no on_tool_start, or one that is not
        a function, is not changed and does not stop the plugin loading.
        The plugin then files changes under the environment's project."""
        import asz_langchain
        from langchain_core.tracers.langchain import LangChainTracer

        class Called:
            def __call__(self, *args, **kwargs):
                return None

        odd = Called()
        self.assertFalse(asz_langchain._watch(type("WithNone", (), {})))
        holder = type("WithAnObject", (), {"on_tool_start": odd})
        self.assertFalse(asz_langchain._watch(holder))
        self.assertIs(holder.__dict__["on_tool_start"], odd)

        watching = asz_langchain._watching
        asz_langchain._watching = False
        try:
            def invoke(look, quiet, config):
                tracer = LangChainTracer(project_name="code-project", client=quiet)
                look.invoke({"path": "a"}, config={**config, "callbacks": [tracer]})

            traced, sent = self._run(invoke)
            self.assertEqual(traced, "code-project")
            self.assertEqual(sent, {self._session("env-project")})
        finally:
            asz_langchain._watching = watching

    def test_the_bound_drops_the_oldest_run_and_its_changes(self):
        """The bound is only a safety limit, for runs a tracer starts and the
        plugin is never told of. Past it the oldest is dropped, and a tool
        whose project was dropped before the plugin took it records nothing."""
        import asz_langchain

        limit = asz_langchain._LIMIT
        asz_langchain._LIMIT = 2
        try:
            for run in ("first", "second", "third"):
                asz_langchain._tracer_started(run, "code-project")
            begun = []
            self.assertIsNone(asz_langchain._plugin_told("first", begun.append))
            self.assertEqual(asz_langchain._plugin_told("third", begun.append), "code-project")
            for run in ("first", "second", "third"):
                asz_langchain._plugin_done(run)
            self.assertEqual(begun, [])
            for run in ("first", "second", "third"):
                self.assertNotIn(run, asz_langchain._runs)
            self.assertIn("first", asz_langchain._finished)
        finally:
            asz_langchain._LIMIT = limit

    def test_one_begin_for_a_run_two_merged_managers_tell(self):
        """Two callback managers merged into one each bring the plugin and a
        tracer, so the run is told to a plugin, a tracer, a plugin and a
        tracer. Once a begin is sent for the run, no other is: a second one
        replaced asz-changes' note and opened a second window, under the
        second tracer's project."""
        import asz_langchain
        from langchain_core.callbacks import CallbackManager
        from langchain_core.tools import tool
        from langchain_core.tracers.langchain import LangChainTracer

        @tool
        def look(path: str) -> str:
            """Look at a path."""
            return "looked"

        quiet = _Quiet()
        handlers = [asz_langchain.AszChanges(), LangChainTracer(project_name="code-project", client=quiet),
                    asz_langchain.AszChanges(), LangChainTracer(project_name="another-project", client=quiet)]
        merged = CallbackManager(handlers=handlers, inheritable_handlers=handlers)
        look.invoke({"path": "a"}, config={"callbacks": merged, "metadata": {"thread_id": "t"}})
        traced = sorted(r.get("session_name") for r in quiet.created if r.get("run_type") == "tool")
        self.assertEqual(traced, ["another-project", "code-project"])
        self.assertEqual(self.sent, [self._session("code-project")] * 2)

    def test_a_begin_whose_tracer_never_comes_is_let_go(self):
        """A tool cancelled after the plugin is told and before its tracer is
        has no end, so nothing takes its waiting begin out. Every half of a
        start comes within one start of one tool, so a begin left waiting
        past the life of an entry is dropped, with the handler and the input
        it holds."""
        import gc
        import uuid
        import weakref

        import asz_langchain

        now = [1000.0]
        clock = asz_langchain._now
        asz_langchain._now = lambda: now[0]
        try:
            handler = asz_langchain.AszChanges()
            run = uuid.uuid4()
            handler.on_tool_start({"name": "look"}, "", run_id=run, metadata={"thread_id": "t"},
                                  inputs={"path": "x" * 100000})
            kept = weakref.ref(handler)
            del handler
            gc.collect()
            self.assertIsNotNone(kept(), "the waiting begin was dropped at once")
            now[0] += asz_langchain._LIFE + 1
            asz_langchain._tracer_started("another run", "code-project")
            gc.collect()
            self.assertIsNone(kept(), "the waiting begin still holds the handler and its input")
            self.assertNotIn(str(run), asz_langchain._runs)
            self.assertIn(str(run), asz_langchain._finished)
            asz_langchain._plugin_done("another run")
        finally:
            asz_langchain._now = clock
        self.assertEqual(self.sent, [])

    def _clocked(self):
        """A fake clock for the plugin's state, put back when the test ends."""
        import asz_langchain

        now = [1000.0]
        clock = asz_langchain._now
        asz_langchain._now = lambda: now[0]
        self.addCleanup(setattr, asz_langchain, "_now", clock)
        return now

    def test_a_sent_begin_stays_sent_past_the_life_of_an_entry(self):
        """Only a waiting begin expires. A run whose begin was sent stays sent
        until its tool ends, however long that takes: dropped with the
        waiting ones after ten minutes, a second plugin and tracer of the same
        run sent a second begin and a second end, under another project."""
        import uuid

        import asz_langchain

        now = self._clocked()
        first, second = asz_langchain.AszChanges(), asz_langchain.AszChanges()
        run = uuid.uuid4()
        start = dict(run_id=run, metadata={"thread_id": "t"}, inputs={"path": "a"})
        first.on_tool_start({"name": "look"}, "", **start)
        asz_langchain._tracer_started(str(run), "code-project")("code-project")
        now[0] += asz_langchain._LIFE + 1
        self.assertIsNone(asz_langchain._tracer_started("another run", "code-project"))
        second.on_tool_start({"name": "look"}, "", **start)
        self.assertIsNone(asz_langchain._tracer_started(str(run), "another-project"))
        for handler in (first, second):
            handler.on_tool_end("looked", run_id=run)
        asz_langchain._plugin_done("another run")
        self.assertEqual(self.sent, [self._session("code-project")] * 2)

    def test_an_expired_start_is_not_used_or_made_again(self):
        """The life of an entry is checked before it is read, and a run whose
        first half expired is finished. With more than ten minutes between
        the plugin and the tracer being told, in either order, nothing is
        recorded, though a second plugin and tracer of the same dispatch
        follow: read before it was dropped, the first half was used, and
        dropped, it was made again by the next callback and completed, once
        under the second tracer's project."""
        import uuid

        import asz_langchain

        now = self._clocked()
        for order in ("the plugin first", "the tracer first"):
            with self.subTest(order):
                self.sent.clear()
                handlers, run = [asz_langchain.AszChanges(), asz_langchain.AszChanges()], uuid.uuid4()

                def plugin(handler):
                    return lambda: handler.on_tool_start({"name": "look"}, "", run_id=run,
                                                         metadata={"thread_id": "t"}, inputs={"path": "a"})

                def tracer(project):
                    def started():
                        begin = asz_langchain._tracer_started(str(run), project)
                        if begin is not None:
                            begin(project)
                    return started

                pairs = [(plugin(handlers[0]), tracer("code-project")),
                         (plugin(handlers[1]), tracer("another-project"))]
                if order == "the tracer first":
                    pairs = [(t, p) for p, t in pairs]
                calls = [call for pair in pairs for call in pair]
                calls[0]()
                now[0] += asz_langchain._LIFE + 1
                for call in calls[1:]:
                    call()
                for handler in handlers:
                    handler.on_tool_end("looked", run_id=run)
                self.assertEqual(self.sent, [])

    def test_a_sent_run_stays_finished_after_its_handler_is_gone(self):
        """A tool cancelled while LangChain's worker threads still run its
        callbacks lets go of the handler that sent its begin, and a late
        plugin and tracer of the same dispatch follow. This is the state that
        leaves: the handler is gone, and two thousand more runs come and go
        without an end. Forgotten with its handler, once there were 1,024
        runs to look through, the run was sent a second begin under the late
        tracer's project. A finished run stays finished."""
        import gc
        import uuid

        import asz_langchain

        def started(run):
            handler = asz_langchain.AszChanges()
            handler.on_tool_start({"name": "look"}, "", run_id=run, metadata={"thread_id": "t"}, inputs={})
            begin = asz_langchain._tracer_started(str(run), "code-project")
            if begin is not None:
                begin("code-project")

        run = uuid.uuid4()
        started(run)
        others = 2048
        for _ in range(others):
            started(uuid.uuid4())
        gc.collect()
        late = asz_langchain.AszChanges()
        late.on_tool_start({"name": "look"}, "", run_id=run, metadata={"thread_id": "t"}, inputs={})
        self.assertIsNone(asz_langchain._tracer_started(str(run), "another-project"))
        late.on_tool_end("looked", run_id=run)
        self.assertEqual(self.sent, [self._session("code-project")] * (1 + others))

    def test_the_finished_runs_are_bounded_by_count(self):
        """The finished runs are kept by count alone, the oldest dropped
        first. A second begin for a run needs that many later runs first."""
        import uuid

        import asz_langchain

        limit = asz_langchain._FINISHED_LIMIT
        asz_langchain._FINISHED_LIMIT = 2
        try:
            runs = [str(uuid.uuid4()) for _ in range(3)]
            for run in runs:
                asz_langchain._plugin_told(run, lambda project: None)
                asz_langchain._tracer_started(run, "code-project")
            self.assertEqual(list(asz_langchain._finished), runs[1:])
        finally:
            asz_langchain._FINISHED_LIMIT = limit

    def test_a_handler_used_again_keeps_a_bounded_number_of_open_tools(self):
        """LangChain cancels an asynchronous tool without telling any handler
        it ended or failed, so the handler that sent its begin keeps the tool
        open. A handler used for many tools kept every cancelled one for ever;
        past the bound the oldest open tools are dropped, and their end
        record is not written."""
        import asyncio

        import asz_langchain
        from langchain_core.tools import tool
        from langchain_core.tracers.langchain import LangChainTracer

        gate = asyncio.Event()

        @tool
        async def wait(path: str) -> str:
            """Wait for the gate."""
            await gate.wait()
            return "done"

        quiet = _Quiet()
        tracer = LangChainTracer(project_name="code-project", client=quiet)
        handler = asz_langchain.AszChanges()
        tools = 5
        limit = asz_langchain._OPEN_LIMIT
        asz_langchain._OPEN_LIMIT = 2
        self.addCleanup(setattr, asz_langchain, "_OPEN_LIMIT", limit)

        async def cancelled():
            calls = [asyncio.ensure_future(wait.ainvoke({"path": str(i)}, config={
                "callbacks": [tracer, handler], "metadata": {"thread_id": "t"}})) for i in range(tools)]
            for _ in range(3000):
                if len(self.sent) >= tools:
                    break
                await asyncio.sleep(0.01)
            for call in calls:
                call.cancel()
            await asyncio.gather(*calls, return_exceptions=True)

        asyncio.run(cancelled())
        self.assertEqual(len(self.sent), tools, "every tool's begin was not sent before it was cancelled")
        self.assertEqual(len(handler._open), 2)

    def test_a_tool_traced_with_tracing_disabled_records_nothing(self):
        """Inside tracing_context(enabled=False) the tracer still starts the
        tool's run, marked as one it does not send, so no conversation holds
        it. It is treated as a run no tracer traces, whichever of the tracer
        and the plugin is told first."""
        from langchain_core.runnables import RunnableLambda
        from langchain_core.tools import tool
        from langchain_core.tracers.langchain import LangChainTracer
        from langsmith.run_helpers import tracing_context

        @tool
        def look(path: str) -> str:
            """Look at a path."""
            return "looked"

        def disabled(call):
            def inside(x, config):
                with tracing_context(enabled=False):
                    return call(x, config)
            return inside

        quiet = _Quiet()
        tracer = LangChainTracer(project_name="code-project", client=quiet)
        bound = look.with_config(callbacks=[LangChainTracer(project_name="code-project", client=quiet)])
        cases = {
            "the tracer first": lambda: RunnableLambda(disabled(lambda x, config: look.invoke({"path": x}, config))).invoke(
                "a", config={"callbacks": [tracer], "metadata": {"thread_id": "t"}}),
            "the plugin first": lambda: RunnableLambda(disabled(lambda x, config: bound.invoke({"path": x}, config))).invoke(
                "a", config={"metadata": {"thread_id": "t"}}),
        }
        for name, invoke in cases.items():
            with self.subTest(name):
                self.sent.clear()
                invoke()
                self.assertEqual([r for r in quiet.created if r.get("run_type") == "tool"], [])
                self.assertEqual(self.sent, [])


if __name__ == "__main__":
    unittest.main()
