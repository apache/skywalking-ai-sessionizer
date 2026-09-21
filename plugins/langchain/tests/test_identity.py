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

"""The Python half of the agreement with the receiver.

The tables under testdata are the contract, and the Go tests read the same
two files. Until this existed only Go was held to them, so a change on this
side could not fail anything: the tables would simply have described what
Python did, whatever that had become.

Nothing here imports langchain. The module is loaded from its path on
purpose, because importing the package would pull in langchain_core, and
then the one test that guards the join would need the whole framework
installed to run.
"""

import importlib.util
import json
import pathlib
import unittest

HERE = pathlib.Path(__file__).resolve().parent
PLUGIN = HERE.parent


def _identity():
    spec = importlib.util.spec_from_file_location(
        "asz_identity", PLUGIN / "asz_langchain" / "identity.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


identity = _identity()


def _table(name):
    with (PLUGIN / "testdata" / name).open(encoding="utf-8") as f:
        return json.load(f)


class TestStorageID(unittest.TestCase):
    def test_every_case_in_the_table(self):
        cases = _table("identity.json")
        self.assertTrue(cases, "the identity table is empty")
        for case in cases:
            with self.subTest(values=case["values"]):
                self.assertEqual(identity.storage_id(case["values"]), case["id"])

    def test_two_owners_cannot_share_a_name(self):
        """The boundaries between dimensions are part of the identity.

        With a plain separator these two are the same bytes and so the same
        session, which silently merges two conversations.
        """
        self.assertNotEqual(
            identity.storage_id(["a\x1fb", "c"]),
            identity.storage_id(["a", "b\x1fc"]))

    def test_a_key_is_never_used_as_a_path(self):
        for values in (["../outside", "t"], ["_hidden", "t"], ["CON", "t"]):
            name = identity.storage_id(values)
            self.assertTrue(name.startswith("ls-"), name)
            self.assertNotIn("/", name)
            self.assertNotIn("\\", name)
            self.assertNotIn("..", name)


class TestOwnership(unittest.TestCase):
    def test_every_case_in_the_table(self):
        cases = _table("ownership.json")
        self.assertTrue(cases, "the ownership table is empty")
        for case in cases:
            with self.subTest(metadata=case["metadata"]):
                got = identity.owner_values(
                    case["project"], case["metadata"], case["keys"], case["scope"])
                self.assertEqual(got, case["values"])
                if got is not None:
                    self.assertEqual(identity.storage_id(got), case["id"])

    def test_a_run_with_no_supplied_key_has_no_owner(self):
        """Nothing here invents an identity."""
        self.assertIsNone(identity.owner_values("p", {}))
        self.assertIsNone(identity.owner_values("p", {"thread_id": ""}))
        self.assertIsNone(identity.owner_values("p", {"thread_id": 7}))
        self.assertIsNone(identity.owner_values("p", None))


if __name__ == "__main__":
    unittest.main()


class TestProject(unittest.TestCase):
    """The project the client will report, resolved the client's way.

    The receiver reads the project out of each run the client sends, so the
    shim has to reach the same answer or the change records go to a session
    that holds no conversation.
    """

    def setUp(self):
        import importlib.util
        spec = importlib.util.spec_from_file_location(
            "asz_handler", PLUGIN / "asz_langchain" / "__init__.py")
        source = (PLUGIN / "asz_langchain" / "__init__.py").read_text(encoding="utf-8")
        head = "\n".join(line for line in source.split("class AszChanges")[0].splitlines()
                         if not line.startswith(("from langchain", "from .")))
        self.mod = {"os": __import__("os"), "__name__": "asz_handler"}
        exec(compile(head, "asz_langchain/__init__.py", "exec"), self.mod)

    def _project(self, env):
        import os
        for name in self.mod["_PROJECT_VARS"]:
            os.environ.pop(name, None)
        os.environ.update(env)
        try:
            return self.mod["_project"]()
        finally:
            for name in env:
                os.environ.pop(name, None)

    def test_the_clients_order(self):
        for env, want in (
            ({}, "default"),
            ({"LANGSMITH_PROJECT": "p1"}, "p1"),
            ({"LANGCHAIN_PROJECT": "p2"}, "p2"),
            ({"LANGSMITH_PROJECT": "p1", "LANGCHAIN_PROJECT": "p2"}, "p1"),
            ({"LANGCHAIN_SESSION": "p3"}, "p3"),
            ({"HOSTED_LANGSERVE_PROJECT_NAME": "h", "LANGSMITH_PROJECT": "p1"}, "h"),
        ):
            with self.subTest(env=env):
                self.assertEqual(self._project(env), want)

    def test_a_blank_value_is_not_a_name(self):
        """The client skips a value that is empty or all spaces.

        Taking it would put the change records in a session named for the
        whitespace, while the conversation went to the next variable's.
        """
        self.assertEqual(
            self._project({"LANGSMITH_PROJECT": "   ", "LANGCHAIN_PROJECT": "prod"}), "prod")
        self.assertEqual(self._project({"LANGSMITH_PROJECT": ""}), "default")
        # The hosted variable is the exception: the client reads it with a
        # plain lookup, so a blank value set there is the project.
        self.assertEqual(
            self._project({"HOSTED_LANGSERVE_PROJECT_NAME": "", "LANGSMITH_PROJECT": "p"}), "")
        self.assertEqual(
            self._project({"HOSTED_LANGSERVE_PROJECT_NAME": "   ", "LANGSMITH_PROJECT": "p"}), "   ")
