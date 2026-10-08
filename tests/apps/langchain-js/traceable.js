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

// A LangChain JS application traced to asz: a decorated function is handed the messages a model call is sent,
// with a system message, and calls a chat model with them. The model is a fake one, so no provider is called.
import { traceable } from "langsmith/traceable";
import { Client } from "langsmith";
import { SystemMessage, HumanMessage } from "@langchain/core/messages";
import { FakeListChatModel } from "@langchain/core/utils/testing";
import { awaitAllCallbacks } from "@langchain/core/callbacks/promises";

const client = new Client();
const model = new FakeListChatModel({ responses: ["Two pages changed: the setup page and the formats page."] });

const review = traceable(
  async (input) => {
    const reply = await model.invoke(input.messages);
    return { answer: reply.content };
  },
  { name: "review", client, metadata: { thread_id: "js-thread-1" } },
);

const out = await review({
  messages: [
    new SystemMessage("You are a careful reviewer."),
    new HumanMessage("what changed in the docs"),
  ],
  limit: 3,
});
console.log("answer:", out.answer);
await awaitAllCallbacks();
await client.awaitPendingTraceBatches();
console.log("flushed");
