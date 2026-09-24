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

"""The demo application: one real LangGraph agent per case asz has to handle.

Each case is a function named after what it exercises, and the model's
behaviour travels in the model name, so a case reads as the shape it produces
rather than as a script. Nothing here talks to a provider: the stand-in model
serves every call.
"""

import os
import threading
import time

from langchain_core.tools import tool
from langchain_openai import ChatOpenAI
from langgraph.checkpoint.memory import InMemorySaver
from langgraph.prebuilt import create_react_agent

BASE = "http://127.0.0.1:8931/v1"
SYSTEM = "You are a troubleshooting assistant.\n" + ("Rule: never guess.\n" * 800)


def model(behaviour):
    """A model whose name tells the stand-in which behaviour to play."""
    return ChatOpenAI(model=behaviour, base_url=BASE, api_key="stand-in", temperature=0)


@tool
def lookup_status(cluster: str = "prod-1", question: str = "", text: str = "") -> str:
    """Look up the health of a cluster."""
    return "cluster %s: 3/3 pods ready" % cluster


@tool
def failing_tool(cluster: str = "prod-1", question: str = "", text: str = "") -> str:
    """A tool that fails."""
    raise RuntimeError("the cluster API refused the request")


@tool
def slow_lookup(cluster: str = "prod-1", question: str = "", text: str = "") -> str:
    """A tool slow enough that its run is posted before it finishes."""
    time.sleep(float(os.environ.get("ASZ_SLOW_SECONDS", "12")))
    return "cluster %s: 3/3 pods ready" % cluster


@tool
def run_command(command: str = "", timeout: int = 30, cluster: str = "",
                question: str = "", text: str = "") -> str:
    """Run a command and return everything it printed."""
    return "POD OUTPUT LINE\n" * 2000   # 32 KB, far past any attribute limit


def ask(agent, text, thread=None, project=None):
    config = {"configurable": {"thread_id": thread}} if thread else {}
    return agent.invoke({"messages": [{"role": "user", "content": text}]}, config=config)


# --- the cases ------------------------------------------------------------

def case_plain():
    """One turn, no tools: the smallest conversation there is."""
    agent = create_react_agent(model("plain"), [], checkpointer=InMemorySaver())
    ask(agent, "Is prod-1 healthy?", "thread-plain")


def case_three_turns():
    """Three turns on one thread: one conversation, three traces."""
    agent = create_react_agent(model("tool"), [lookup_status], checkpointer=InMemorySaver())
    for question in ("Is prod-1 healthy?", "And the pods?", "Summarise that."):
        ask(agent, question, "thread-three-turns")


def case_tool_error():
    """A tool that raises: the failure has to survive as a failed result."""
    agent = create_react_agent(model("tool-error"), [failing_tool],
                               checkpointer=InMemorySaver())
    try:
        ask(agent, "Check prod-1.", "thread-tool-error")
    except Exception:
        pass


def case_parallel_tools():
    """Two tool calls in one model response, run in one graph step."""
    agent = create_react_agent(model("parallel"), [lookup_status],
                               checkpointer=InMemorySaver())
    ask(agent, "Check both clusters.", "thread-parallel")


def case_loop():
    """A graph that loops: the same node runs five times in one trace."""
    agent = create_react_agent(model("loop"), [lookup_status], checkpointer=InMemorySaver())
    ask(agent, "Keep checking until it settles.", "thread-loop")


def case_two_threads():
    """Two threads at once: one request carries parts of both."""
    agent = create_react_agent(model("tool"), [lookup_status], checkpointer=InMemorySaver())
    threads = [threading.Thread(target=ask, args=(agent, "Is %s healthy?" % t, "thread-%s" % t))
               for t in ("a", "b")]
    for t in threads:
        t.start()
    for t in threads:
        t.join()


def case_slow_tool():
    """A slow tool: runs are posted open and completed later by a patch."""
    agent = create_react_agent(model("slow"), [slow_lookup], checkpointer=InMemorySaver())
    ask(agent, "Check prod-1 slowly.", "thread-slow")


def case_large_content():
    """A 20 KB command, a 200 KB result and a 15 KB system prompt."""
    agent = create_react_agent(model("large"), [run_command], checkpointer=InMemorySaver(),
                               prompt=SYSTEM)
    ask(agent, "Which pods are unhealthy?", "thread-large")


def case_subagent():
    """An agent as a tool: its model calls have their own context."""
    analyst = create_react_agent(model("tool"), [lookup_status], name="analyst")

    @tool
    def delegate_to_analyst(question: str = "", cluster: str = "", text: str = "") -> str:
        """Hand the question to the analyst sub-agent."""
        return analyst.invoke(
            {"messages": [{"role": "user", "content": question}]})["messages"][-1].content

    @tool
    def summarise(text: str = "", question: str = "", cluster: str = "") -> str:
        """Summarise some text with one model call. No loop, no agent."""
        return model("plain").invoke(
            [{"role": "user", "content": "Summarise this: " + text}]).content

    supervisor = create_react_agent(model("every-tool"), [delegate_to_analyst, summarise],
                                    checkpointer=InMemorySaver())
    ask(supervisor, "Is prod-1 healthy?", "thread-subagent")


def case_subagent_own_thread():
    """A sub-agent invoked with its own thread key: a separate conversation."""
    analyst = create_react_agent(model("tool"), [lookup_status], name="analyst",
                                 checkpointer=InMemorySaver())

    @tool
    def delegate(question: str = "", cluster: str = "", text: str = "") -> str:
        """Hand the question to an analyst that keeps its own thread."""
        return analyst.invoke({"messages": [{"role": "user", "content": question}]},
                              config={"configurable": {"thread_id": "thread-analyst-own"}}
                              )["messages"][-1].content

    supervisor = create_react_agent(model("tool"), [delegate], checkpointer=InMemorySaver())
    ask(supervisor, "Ask the analyst.", "thread-subagent-parent")


def case_no_thread_key():
    """No thread key at all: ownership is not supplied and must not be invented."""
    agent = create_react_agent(model("tool"), [lookup_status])
    ask(agent, "Is prod-1 healthy?")


def case_shared_thread_key():
    """Two applications using one thread key: project alone is not isolation."""
    agent = create_react_agent(model("tool"), [lookup_status], checkpointer=InMemorySaver())
    ask(agent, "Application one asking.", "123")
    ask(agent, "Application two asking.", "123")


def case_unsafe_thread_key():
    """Thread keys that are not safe as a path, and one that is not ASCII."""
    agent = create_react_agent(model("plain"), [], checkpointer=InMemorySaver())
    for key in ("../outside", "team/customer", "_hidden", "会话-1"):
        ask(agent, "Hello.", key)


def case_long_conversation():
    """Twenty turns on one thread: how the repeated history grows.

    Every model call carries the whole history again, so a thread's bytes grow
    with the square of its turns. This is the case that says how much.
    """
    agent = create_react_agent(model("plain"), [], checkpointer=InMemorySaver())
    for i in range(int(os.environ.get("ASZ_TURNS", "20"))):
        ask(agent, "Turn %d: what is the status?" % (i + 1), "thread-long")


def case_summarized():
    """SummarizationMiddleware: the history is replaced by a summary, twice.

    The middleware marks what it does. Its summary message carries
    additional_kwargs lc_source=summarization, and its own model call carries
    the same key in its metadata. The mark is what asz takes a context reset
    from, so this case is the evidence for it.
    """
    from langchain.agents import create_agent
    from langchain.agents.middleware import SummarizationMiddleware

    agent = create_agent(model("tool"), [lookup_status], checkpointer=InMemorySaver(),
                         middleware=[SummarizationMiddleware(model=model("summary"),
                                                             trigger=("messages", 6),
                                                             keep=("messages", 2))])
    for cluster in ("prod-1", "prod-2", "prod-3", "prod-4"):
        ask(agent, "Check %s." % cluster, "thread-summarized")


def case_abandoned_run():
    """A process that dies mid-turn: the open runs are all that ever arrive.

    The client sends from a background thread, so what a crash leaves behind is
    whatever it had already sent. Nothing completes these runs, which is the
    evidence asz has to read as unfinished rather than as absent.
    """
    agent = create_react_agent(model("slow"), [slow_lookup], checkpointer=InMemorySaver())
    threading.Thread(target=ask, args=(agent, "Check prod-1.", "thread-abandoned"),
                     daemon=True).start()
    time.sleep(float(os.environ.get("ASZ_ABANDON_AFTER", "3")))
    os._exit(0)


def case_traceable_only():
    """No LangChain graph at all: the client's own decorator."""
    from langsmith import traceable

    @traceable(run_type="tool", name="lookup_status")
    def lookup(cluster):
        return "cluster %s: 3/3 ready" % cluster

    @traceable(run_type="chain", name="troubleshoot",
               metadata={"thread_id": "thread-traceable"})
    def troubleshoot(question):
        return "answer: " + lookup("prod-1")

    troubleshoot("is prod-1 ok?")


CASES = {
    "plain": case_plain,
    "three-turns": case_three_turns,
    "tool-error": case_tool_error,
    "parallel-tools": case_parallel_tools,
    "loop": case_loop,
    "two-threads": case_two_threads,
    "slow-tool": case_slow_tool,
    "large-content": case_large_content,
    "subagent": case_subagent,
    "subagent-own-thread": case_subagent_own_thread,
    "no-thread-key": case_no_thread_key,
    "shared-thread-key": case_shared_thread_key,
    "unsafe-thread-key": case_unsafe_thread_key,
    "long-conversation": case_long_conversation,
    "summarized": case_summarized,
    "abandoned-run": case_abandoned_run,
    "traceable-only": case_traceable_only,
}
