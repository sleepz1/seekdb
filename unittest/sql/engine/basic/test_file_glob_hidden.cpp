/*
 * Copyright (c) 2026 OceanBase.
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 * http://www.apache.org/licenses/LICENSE-2.0
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "src/sql/engine/basic/file_sql_scan.h"
#include <fstream>
#include <iostream>
#include <unistd.h>

using namespace oceanbase::sql::filesql;

int main()
{
  int failures = 0;
  auto check = [&](bool ok, const std::string &message) {
    if (!ok) { ++failures; std::cerr << "FAIL " << message << '\n'; }
  };
  char temporary[] = "/tmp/file-glob-hidden-XXXXXX";
  const char *created = ::mkdtemp(temporary);
  if (created == nullptr) { std::cerr << "cannot create fixture\n"; return 2; }
  const std::string dir(created);
  const std::vector<std::string> names = {
      "basic.csv", "basic.jsonl", "invalid.txt", "simple.parquet.b64",
      "._basic.csv", "._basic.jsonl", "._invalid.txt", "._simple.parquet.b64", ".hidden.csv"};
  for (const auto &name : names) {
    std::ofstream out(dir + "/" + name);
    out << "id\n1\n";
    check(out.good(), "create fixture " + name);
  }
  Options opt;
  opt.secure_root = dir;
  std::vector<std::string> paths;
  Fingerprint fp;
  auto list = [&](const std::string &pattern, const std::vector<std::string> &expected) {
    Status st = list_glob(dir + "/" + pattern, opt, paths, fp);
    std::vector<std::string> full;
    for (const auto &name : expected) { full.push_back(dir + "/" + name); }
    check(st.ok() && paths == full, "wildcard " + pattern + " respects leading dot");
  };
  list("*", {"basic.csv", "basic.jsonl", "invalid.txt", "simple.parquet.b64"});
  list("*.csv", {"basic.csv"});
  list("?*", {"basic.csv", "basic.jsonl", "invalid.txt", "simple.parquet.b64"});
  list("._*", {"._basic.csv", "._basic.jsonl", "._invalid.txt", "._simple.parquet.b64"});
  list(".hidden.csv", {".hidden.csv"});
  list("*.missing", {});
  Schema schema;
  Status st = infer_schema(KIND_GLOB, dir + "/*", opt, schema);
  check(st.ok() && schema.row_count == 4, "schema excludes implicit hidden files");
  FileSqlReader reader;
  st = reader.open(KIND_GLOB, dir + "/*", opt);
  check(st.ok(), "open wildcard reader");
  for (int pass = 0; pass < 2; ++pass) {
    int count = 0;
    std::vector<Cell> row;
    while ((st = reader.next(row)).ok()) { ++count; }
    check(st.end() && count == 4, "stream and rewind exclude hidden files");
    if (pass == 0) { check(reader.rewind().ok(), "rewind wildcard reader"); }
  }
  check(reader.check_unchanged().ok(), "stable glob fingerprint");
  st = infer_schema(KIND_CSV, dir + "/.hidden.csv", opt, schema);
  check(st.ok() && schema.row_count == 1, "explicit hidden CSV remains readable");
  for (const auto &name : names) { ::unlink((dir + "/" + name).c_str()); }
  ::rmdir(dir.c_str());
  if (failures != 0) { std::cerr << failures << " failed\n"; return 1; }
  std::cout << "hidden-file glob regression passed\n";
  return 0;
}
