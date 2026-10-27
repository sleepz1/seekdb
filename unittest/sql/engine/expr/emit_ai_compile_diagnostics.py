#!/usr/bin/env python3
# Copyright (c) 2026 OceanBase.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Publish real compiler diagnostics as GitHub annotations; preserve native exit outside."""
import pathlib
import re
import sys

root = pathlib.Path.cwd().resolve()
def escape(text):
    return text.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
count = 0
for line in pathlib.Path(sys.argv[1]).read_text(errors="replace").splitlines():
    match = re.match(r"^(.+):(\d+):(\d+): (?:fatal )?error: (.+)$", line)
    if match:
        source, row, col, message = match.groups()
        try:
            path = pathlib.Path(source).resolve().relative_to(root).as_posix()
        except ValueError:
            continue
        print("::error file=%s,line=%s,col=%s::%s" %
              (escape(path), row, col, escape(message)))
        count += 1
        if count == 20:
            break
