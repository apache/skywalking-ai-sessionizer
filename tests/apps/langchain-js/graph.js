// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// A LangGraph JS agent traced to asz: a graph with one tool, run on a thread, its first input carrying a system
// message and the person's question. The model is a small fake that asks for the tool once and then answers, so no
// provider is called.
import { StateGraph, MessagesAnnotation, START, END } from "@langchain/langgraph";
import { ToolNode } from "@langchain/langgraph/prebuilt";
import { tool } from "@langchain/core/tools";
import { AIMessage, HumanMessage, SystemMessage } from "@langchain/core/messages";
import { BaseChatModel } from "@langchain/core/language_models/chat_models";
import { z } from "zod";
import { Client } from "langsmith";
import { LangChainTracer } from "@langchain/core/tracers/tracer_langchain";
import { awaitAllCallbacks } from "@langchain/core/callbacks/promises";

const readDocs = tool(async ({ page }) => `The ${page} page gained a section on hiding.`, {
  name: "read_docs",
  description: "Read one page of the docs.",
  schema: z.object({ page: z.string() }),
});

class FakeToolModel extends BaseChatModel {
  _llmType() { return "fake-tool-model"; }
  bindTools(tools) { return this; }
  async _generate(messages) {
    const asked = messages.some((m) => m._getType() === "tool");
    const message = asked
      ? new AIMessage("The setup page gained a section on hiding.")
      : new AIMessage({ content: "", tool_calls: [{ id: "call-1", name: "read_docs", args: { page: "setup" } }] });
    return { generations: [{ message, text: typeof message.content === "string" ? message.content : "" }] };
  }
}

const model = new FakeToolModel({});
const graph = new StateGraph(MessagesAnnotation)
  .addNode("agent", async (state) => ({ messages: [await model.invoke(state.messages)] }))
  .addNode("tools", new ToolNode([readDocs]))
  .addEdge(START, "agent")
  .addConditionalEdges("agent", (state) => (state.messages.at(-1).tool_calls?.length ? "tools" : END))
  .addEdge("tools", "agent")
  .compile();

const client = new Client();
const tracer = new LangChainTracer({ client, projectName: process.env.LANGSMITH_PROJECT });
const result = await graph.invoke(
  { messages: [new SystemMessage("You are a docs assistant. You answer from the docs only."), new HumanMessage("what changed in the setup page")] },
  { configurable: { thread_id: "js-graph-thread-1" }, callbacks: [tracer] },
);
console.log("answer:", result.messages.at(-1).content);
await awaitAllCallbacks();
await client.awaitPendingTraceBatches();
console.log("flushed");
