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

"""Which conversation a tool call belongs to, named the way asz names it.

This is the one thing the shim and the receiver must agree on exactly. The
receiver lands the conversation and this lands what a tool call changed, and
they meet only in the name of the session directory. The same rule is written
in Go in internal/adapters/langsmith/identity.go, and testdata/identity.json
holds the cases both are checked against.
"""

import hashlib

# The keys that may carry the thread, in the order they are tried.
THREAD_KEYS = ("thread_id", "session_id", "conversation_id")

# The dimensions that together own a conversation, in order.
SCOPE = ("project", "thread")

# A slug stays readable and well inside the shortest path limit worth
# worrying about. The digest carries the identity, so truncating here costs
# nothing but legibility.
MAX_SLUG = 48


def thread_of(metadata, keys=None):
    """The thread key a run carries, or None when it supplies none."""
    for key in keys or THREAD_KEYS:
        value = (metadata or {}).get(key)
        if isinstance(value, str) and value:
            return value
    return None


def owner_values(project, metadata, keys=None, scope=None):
    """The dimensions that own this run's conversation, or None.

    This has to match what the receiver does with the same run, because the
    two meet only in the name of the session directory. The receiver reads
    its keys and its scope from the configuration, so this reads the same
    two settings rather than assuming the defaults: a root configured with
    another thread key landed its changes under a name no conversation had,
    and nothing said so - the records were simply never joined.
    """
    thread = thread_of(metadata, keys)
    if thread is None:
        return None
    values = []
    for dimension in scope or SCOPE:
        if dimension == "project":
            values.append(project)
        elif dimension == "thread":
            values.append(thread)
        else:
            value = (metadata or {}).get(dimension)
            values.append(value if isinstance(value, str) else "")
    return values


def slug_of(values):
    """The readable part of a session's name.

    Anything that is not a lower-case letter or a digit becomes a dash, upper
    case folds down, and runs of dashes collapse. A value in a script with no
    ASCII slugs to nothing, which is why the digest and not this makes the
    name unique.
    """
    out = []
    for i, value in enumerate(values):
        if i:
            out.append("-")
        for ch in value:
            if ("a" <= ch <= "z") or ("0" <= ch <= "9"):
                out.append(ch)
            elif "A" <= ch <= "Z":
                out.append(ch.lower())
            else:
                out.append("-")
    text = "".join(out)
    while "--" in text:
        text = text.replace("--", "-")
    text = text.strip("-")
    if len(text) > MAX_SLUG:
        text = text[:MAX_SLUG].strip("-")
    return text


def _tuple(values):
    """Encodes the dimensions so their boundaries cannot be forged.

    Joining with a separator is ambiguous, and not in a way a hash protects
    against: with a plain join, ["a\x1fb", "c"] and ["a", "b\x1fc"] produce
    the same bytes and so the same session. A supplied value can contain any
    character, so the length of each dimension goes in front of it.
    """
    out = bytearray()
    for v in values:
        encoded = v.encode("utf-8")
        out += b"%d:" % len(encoded)
        out += encoded
    return bytes(out)


def storage_id(values):
    """The session directory an owner's evidence lands under.

    A supplied key is never used as a path: applications really do send
    "../outside", "team/customer", "_hidden" and non-ASCII keys. The "ls-"
    prefix keeps the name off the leading underscore that session enumeration
    skips, and away from every Windows device name.
    """
    digest = hashlib.sha256(_tuple(values)).hexdigest()[:12]
    slug = slug_of(values)
    return "ls-" + digest if not slug else "ls-" + slug + "-" + digest
