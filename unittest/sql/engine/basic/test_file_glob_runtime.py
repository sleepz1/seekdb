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

"""Exercise SQL glob semantics with archive-style hidden sidecars on real seekdb."""
import os
import pathlib
import tempfile

import pymysql


def main():
    if os.environ.get("GITHUB_ACTIONS") != "true":
        raise SystemExit("Requires a disposable cloud runner and test instance")
    with tempfile.TemporaryDirectory(prefix="sql-glob-", dir=os.environ["RUNNER_TEMP"]) as directory:
        root = pathlib.Path(directory)
        visible = ["basic.csv", "basic.jsonl", "invalid.txt", "simple.parquet.b64"]
        hidden = ["._" + name for name in visible] + [".hidden.csv"]
        for name in visible + hidden:
            (root / name).write_text("id\n1\n", encoding="utf-8")
        connection = pymysql.connect(host="127.0.0.1", port=2881, user="root", database="test", autocommit=True)
        try:
            with connection.cursor() as cursor:
                cursor.execute("SET SESSION enable_file_sql=ON")
                for pattern, expected in [
                    ("*", visible), ("*.csv", ["basic.csv"]), ("?*", visible),
                    ("._*", hidden[:-1]), (".hidden.csv", [".hidden.csv"]), ("*.missing", []),
                ]:
                    cursor.execute("SELECT SUBSTRING_INDEX(file, '/', -1) FROM glob(%s) ORDER BY file", (str(root / pattern),))
                    actual = [row[0] for row in cursor.fetchall()]
                    assert actual == sorted(expected), (pattern, actual, expected)
                cursor.execute("SELECT COUNT(*) FROM glob(%s)", (str(root / "*"),))
                assert cursor.fetchone() == (4,)
                cursor.execute("SELECT id FROM read_csv(%s)", (str(root / ".hidden.csv"),))
                assert cursor.fetchall() == ((1,),)
        finally:
            connection.close()
    print("real SQL hidden-file glob regression passed (8 checks)")


if __name__ == "__main__":
    main()
