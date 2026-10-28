/*
 * Copyright (c) 2026 OceanBase.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Standalone scanner checks for file SQL. Not linked into observer.
#include "src/sql/engine/basic/file_sql_scan.h"

#include <cmath>
#include <iostream>
#include <fstream>
#include <sstream>
#include <unistd.h>
#include <sys/stat.h>

using namespace oceanbase::sql::filesql;

static int g_fails = 0;

static void expect(bool cond, const std::string &msg) {
  if (!cond) {
    std::cerr << "FAIL " << msg << "\n";
    ++g_fails;
  }
}

static std::string cell_text(const Cell &c) {
  if (c.is_null) {
    return "NULL";
  }
  if (c.type == TYPE_BIGINT || c.type == TYPE_BOOL) {
    return std::to_string(c.i64);
  }
  if (c.type == TYPE_DOUBLE) {
    std::ostringstream os;
    os.setf(std::ios::fixed);
    os.precision(2);
    double v = c.f64;
    if (v == std::floor(v)) {
      os.precision(0);
    }
    os << v;
    return os.str();
  }
  return c.text;
}

static void read_all(FileSqlReader &r, std::vector<std::vector<Cell> > &rows) {
  rows.clear();
  for (;;) {
    std::vector<Cell> row;
    Status st = r.next(row);
    if (st.end()) {
      return;
    }
    if (!st.ok()) {
      std::cerr << "FAIL next: " << st.message << "\n";
      ++g_fails;
      return;
    }
    rows.push_back(row);
  }
}

static void test_snappy() {
  std::vector<uint8_t> out;
  const uint8_t a[] = {0x00, 0x61};
  Status st = snappy_uncompress(a, 2, 1, out);
  expect(st.ok() && out.size() == 1 && out[0] == 'a', "snappy literal a");
  const uint8_t b[] = {0x08, 0x61, 0x62, 0x63, 0x0a, 0x03, 0x00};
  st = snappy_uncompress(b, 7, 6, out);
  expect(st.ok() && std::string(out.begin(), out.end()) == "abcabc", "snappy copy abc");
}

static void test_csv(const std::string &dir) {
  Schema schema;
  Options opt;
  Status st = infer_schema(KIND_CSV, dir + "/basic.csv", opt, schema);
  expect(st.ok(), "infer csv " + st.message);
  expect(schema.columns.size() == 8, "csv cols");
  if (schema.columns.size() == 8) {
    expect(schema.columns[0].name == "id" && schema.columns[0].type == TYPE_BIGINT &&
               !schema.columns[0].nullable,
           "id type");
    expect(schema.columns[2].name == "score" && schema.columns[2].type == TYPE_DOUBLE, "score");
    expect(schema.columns[3].type == TYPE_BOOL && !schema.columns[3].nullable, "active");
    expect(schema.columns[4].type == TYPE_DATE && schema.columns[4].nullable, "day");
    expect(schema.columns[5].type == TYPE_DATETIME && schema.columns[5].nullable, "created");
    expect(schema.columns[6].type == TYPE_VARCHAR, "note");
    expect(schema.columns[7].name == "id__2" && schema.columns[7].type == TYPE_BIGINT, "id__2");
  }
  FileSqlReader r;
  st = r.open(KIND_CSV, dir + "/basic.csv", opt);
  expect(st.ok(), "open csv " + st.message);
  std::vector<std::vector<Cell> > rows;
  read_all(r, rows);
  expect(rows.size() == 3, "csv rows");
  if (rows.size() == 3) {
    expect(cell_text(rows[0][0]) == "1", "r0 id");
    expect(cell_text(rows[0][1]) == "Alice", "r0 name");
    expect(cell_text(rows[0][2]) == "98.50" || cell_text(rows[0][2]) == "98.5", "r0 score");
    expect(cell_text(rows[0][3]) == "1", "r0 active");
    expect(cell_text(rows[0][4]) == "2026-08-01", "r0 day");
    expect(cell_text(rows[0][5]) == "2026-08-01 10:11:12", "r0 created");
    expect(cell_text(rows[0][6]) == "hello,\nworld", "r0 note");
    expect(cell_text(rows[0][7]) == "101", "r0 id2");
    expect(cell_text(rows[1][4]) == "NULL" && cell_text(rows[1][5]) == "NULL", "bob nulls");
    expect(cell_text(rows[1][3]) == "0", "bob active");
    expect(cell_text(rows[2][5]) == "2026-08-03 12:13:14", "chen datetime");
    expect(cell_text(rows[2][6]) == "say \"hi\"", "chen note");
    double sum = rows[0][2].f64 + rows[1][2].f64 + rows[2][2].f64;
    expect(std::fabs(sum / 3.0 - 91.916666) < 0.001, "avg score");
    expect(rows[0][3].i64 + rows[1][3].i64 + rows[2][3].i64 == 2, "sum active");
  }
  st = r.rewind();
  expect(st.ok(), "rewind csv");
  read_all(r, rows);
  expect(rows.size() == 3, "rewind rows");
  Options bad = opt;
  bad.sample_size = 10;
  st = infer_schema(KIND_CSV, dir + "/basic.csv", bad, schema);
  expect(st.code == ST_UNSUPPORTED, "sample_size");
}

static void test_json(const std::string &dir) {
  Schema schema;
  Options opt;
  Status st = infer_schema(KIND_JSON, dir + "/basic.jsonl", opt, schema);
  expect(st.ok(), "infer json " + st.message);
  expect(schema.columns.size() == 6, "json cols");
  if (schema.columns.size() == 6) {
    const char *names[] = {"id", "name", "score", "active", "day", "note"};
    for (int i = 0; i < 6; ++i) {
      expect(schema.columns[static_cast<size_t>(i)].name == names[i], std::string("json name ") + names[i]);
    }
    expect(schema.columns[2].type == TYPE_DOUBLE && schema.columns[2].nullable, "json score");
    expect(schema.columns[3].type == TYPE_BOOL, "json active");
    expect(schema.columns[4].type == TYPE_DATE && schema.columns[4].nullable, "json day");
    expect(schema.columns[5].type == TYPE_VARCHAR && schema.columns[5].nullable, "json note");
  }
  FileSqlReader r;
  st = r.open(KIND_JSON, dir + "/basic.jsonl", opt);
  expect(st.ok(), "open json");
  std::vector<std::vector<Cell> > rows;
  read_all(r, rows);
  expect(rows.size() == 3, "json rows");
  if (rows.size() == 3) {
    expect(cell_text(rows[0][4]) == "NULL" && cell_text(rows[0][5]) == "NULL", "json r0 missing");
    expect(cell_text(rows[1][2]) == "NULL" && cell_text(rows[1][4]) == "2026-08-02", "json r1");
    expect(cell_text(rows[2][2]) == "90" || cell_text(rows[2][2]) == "90.00" || rows[2][2].f64 == 90.0,
           "json r2 score");
    expect(cell_text(rows[2][5]) == "ok", "json note");
  }
  std::string nested = "/tmp/file_sql_nested.jsonl";
  std::ofstream os(nested.c_str());
  os << "{\"id\":1,\"obj\":{\"a\":1}}\n";
  os.close();
  st = infer_schema(KIND_JSON, nested, opt, schema);
  expect(st.code == ST_NESTED, "nested json");
}

static void test_parquet(const std::string &path) {
  Schema schema;
  Options opt;
  Status st = infer_schema(KIND_PARQUET, path, opt, schema);
  expect(st.ok(), "infer parquet " + st.message);
  expect(schema.columns.size() == 1 && schema.columns[0].name == "c1", "parquet col");
  expect(schema.row_count == 7, "parquet rows meta");
  if (!schema.columns.empty()) {
    expect(schema.columns[0].type == TYPE_BIGINT, "parquet type");
    expect(schema.columns[0].nullable, "parquet nullable");
  }
  FileSqlReader r;
  st = r.open(KIND_PARQUET, path, opt);
  expect(st.ok(), "open parquet " + st.message);
  std::vector<std::vector<Cell> > rows;
  read_all(r, rows);
  expect(rows.size() == 7, "parquet scan rows");
  int64_t sum = 0;
  int64_t mn = 99;
  int64_t mx = -1;
  for (size_t i = 0; i < rows.size(); ++i) {
    expect(!rows[i].empty() && !rows[i][0].is_null, "parquet value");
    if (!rows[i].empty()) {
      sum += rows[i][0].i64;
      if (rows[i][0].i64 < mn) {
        mn = rows[i][0].i64;
      }
      if (rows[i][0].i64 > mx) {
        mx = rows[i][0].i64;
      }
    }
  }
  expect(sum == 21 && mn == 0 && mx == 6, "parquet agg");
  std::vector<char> none(1, 0);
  r.set_needed(none);
  st = r.rewind();
  expect(st.ok(), "parquet rewind");
  read_all(r, rows);
  expect(rows.size() == 7, "pruned count");
  if (!rows.empty()) {
    expect(rows[0][0].is_null, "pruned null");
  }
  st = r.check_unchanged();
  expect(st.ok(), "parquet unchanged");
}

static void test_glob_and_security(const std::string &dir) {
  Options opt;
  FileSqlReader r;
  Status st = r.open(KIND_GLOB, dir + "/*", opt);
  expect(st.ok(), "glob " + st.message);
  std::vector<std::vector<Cell> > rows;
  read_all(r, rows);
  bool saw_csv = false;
  bool saw_parquet = false;
  bool saw_invalid = false;
  for (size_t i = 0; i < rows.size(); ++i) {
    std::string name = rows[i][0].text;
    std::string::size_type slash = name.find_last_of('/');
    if (slash != std::string::npos) {
      name = name.substr(slash + 1);
    }
    if (name == "basic.csv") {
      saw_csv = true;
    }
    if (name == "simple.parquet") {
      saw_parquet = true;
    }
    if (name == "invalid.txt") {
      saw_invalid = true;
    }
  }
  expect(saw_csv && saw_parquet && saw_invalid, "glob names");
  opt.secure_root = dir;
  Schema blocked;
  st = infer_schema(KIND_CSV, "/etc/passwd", opt, blocked);
  expect(!st.ok(), "secure root blocks passwd");
  st = infer_schema(KIND_CSV, dir + "/../basic.csv", opt, blocked);
  expect(st.code == ST_INVALID, "dotdot rejected");
}

static void test_copy_shaped_csv() {
  const char *path = "/tmp/file_sql_copy_export.csv";
  {
    std::ofstream os(path);
    os << "id,name,score\n"
       << "1,Alice,98.5\n"
       << "2,Bob,87\n"
       << "3,Chen,90.25\n";
  }
  Options opt;
  opt.nullstr = "NULL";
  Schema schema;
  Status st = infer_schema(KIND_CSV, path, opt, schema);
  expect(st.ok() && schema.columns.size() == 3, "copy schema " + st.message);
  if (schema.columns.size() == 3) {
    expect(schema.columns[0].type == TYPE_BIGINT && !schema.columns[0].nullable, "copy id");
    expect(schema.columns[1].type == TYPE_VARCHAR && !schema.columns[1].nullable, "copy name");
    expect(schema.columns[2].type == TYPE_DOUBLE && !schema.columns[2].nullable, "copy score");
  }
  FileSqlReader reader;
  st = reader.open(KIND_CSV, path, opt);
  std::vector<std::vector<Cell> > rows;
  read_all(reader, rows);
  expect(rows.size() == 3, "copy rows");
  if (rows.size() == 3 && rows[0].size() == 3) {
    expect(rows[0][0].i64 == 1 && rows[0][1].text == "Alice" && rows[0][2].f64 == 98.5, "alice");
    expect(rows[1][0].i64 == 2 && rows[1][1].text == "Bob" && rows[1][2].f64 == 87.0, "bob");
    expect(rows[2][0].i64 == 3 && rows[2][1].text == "Chen" && rows[2][2].f64 == 90.25, "chen");
  }

  const char *quoted = "/tmp/file_sql_copy_quote.csv";
  {
    std::ofstream os(quoted);
    os << "active,note,day\n"
       << "true,\"hello,\nworld\",2026-08-01\n"
       << "false,\"say \"\"hi\"\"\",NULL\n"
       << "true,\"\",NULL\n";
  }
  Options qopt;
  qopt.nullstr = "NULL";
  st = infer_schema(KIND_CSV, quoted, qopt, schema);
  expect(st.ok() && schema.columns.size() == 3, "quote schema " + st.message);
  if (schema.columns.size() == 3) {
    expect(schema.columns[0].type == TYPE_BOOL && !schema.columns[0].nullable, "copy bool");
    expect(schema.columns[1].type == TYPE_VARCHAR && !schema.columns[1].nullable, "copy note");
    expect(schema.columns[2].type == TYPE_DATE && schema.columns[2].nullable, "copy day");
  }
  FileSqlReader qreader;
  st = qreader.open(KIND_CSV, quoted, qopt);
  rows.clear();
  read_all(qreader, rows);
  expect(rows.size() == 3, "quote rows");
  if (rows.size() == 3 && rows[0].size() == 3 && rows[2].size() == 3) {
    expect(rows[0][0].i64 == 1 && rows[0][1].text == "hello,\nworld", "multiline");
    expect(rows[0][2].text == "2026-08-01", "copy date text");
    expect(rows[1][0].i64 == 0 && rows[1][1].text == "say \"hi\"", "escaped quote");
    expect(rows[1][2].is_null, "nullstr NULL");
    expect(!rows[2][1].is_null && rows[2][1].text.empty(), "empty quoted field");
    expect(rows[2][2].is_null, "trailing null");
  }
}

static void test_change() {
  std::string path = "/tmp/file_sql_change.csv";
  {
    std::ofstream os(path.c_str());
    os << "id\n1\n";
  }
  Options opt;
  FileSqlReader r;
  Status st = r.open(KIND_CSV, path, opt);
  expect(st.ok(), "open change");
  std::vector<Cell> row;
  st = r.next(row);
  expect(st.ok() && row.size() == 1 && row[0].i64 == 1, "change row");
  {
    std::ofstream os(path.c_str(), std::ios::app);
    os << "2\n";
  }
  st = r.check_unchanged();
  expect(st.code == ST_CHANGED, "file changed");
}

int main(int argc, char **argv) {
  std::string dir = argc > 1 ? argv[1] : "tools/deploy/mysql_test/test_suite/file_sql/data";
  test_snappy();
  test_csv(dir);
  test_json(dir);
  test_parquet(dir + "/simple.parquet");
  test_glob_and_security(dir);
  test_copy_shaped_csv();
  test_change();
  if (g_fails != 0) {
    std::cerr << g_fails << " failed\n";
    return 1;
  }
  std::cout << "file sql scanner tests passed\n";
  return 0;
}
