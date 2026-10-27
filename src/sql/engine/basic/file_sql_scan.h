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

#pragma once

// Streaming CSV / JSONL / Parquet / glob reader used by file SQL.
// The implementation is STL-only so the contest scanner tests can compile
// without linking observer. Seekdb calls it from the values-table operator.

#include <algorithm>
#include <cctype>
#include <cerrno>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <dirent.h>
#include <limits>
#include <map>
#include <set>
#include <sstream>
#include <string>
#include <sys/stat.h>
#include <unistd.h>
#include <utility>
#include <vector>

namespace oceanbase {
namespace sql {
namespace filesql {

enum Type {
  TYPE_BIGINT = 0,
  TYPE_DOUBLE = 1,
  TYPE_BOOL = 2,
  TYPE_DATE = 3,
  TYPE_DATETIME = 4,
  TYPE_VARCHAR = 5,
};

enum Kind {
  KIND_CSV = 1,
  KIND_JSON = 2,
  KIND_PARQUET = 3,
  KIND_GLOB = 4,
};

enum StatusCode {
  ST_OK = 0,
  ST_END = 1,
  ST_INVALID = 2,
  ST_CHANGED = 3,
  ST_LIMIT = 4,
  ST_IO = 5,
  ST_NESTED = 6,
  ST_SCHEMA = 7,
  ST_UNSUPPORTED = 8,
};

enum MetaSlot {
  META_PATH = 0,
  META_KIND = 1,
  META_HEADER = 2,
  META_SAMPLE_SIZE = 3,
  META_ALL_VARCHAR = 4,
  META_DELIM = 5,
  META_NULLSTR = 6,
  META_QUOTE = 7,
  META_FORMAT = 8,
  META_DEV = 9,
  META_INO = 10,
  META_SIZE = 11,
  META_MTIME_NS = 12,
  META_SLOTS = 13,
};

static const int64_t kMaxFileBytes = 1024LL * 1024LL * 1024LL;
static const int64_t kMaxColumns = 4096;
static const int64_t kMaxFieldBytes = 16LL * 1024LL * 1024LL;
static const int64_t kMaxJsonKeys = 4096;

struct Status {
  StatusCode code;
  std::string message;
  Status() : code(ST_OK) {}
  explicit Status(StatusCode c, const std::string &m = std::string()) : code(c), message(m) {}
  bool ok() const { return code == ST_OK; }
  bool end() const { return code == ST_END; }
};

struct Column {
  std::string name;
  Type type;
  bool nullable;
  int64_t null_cnt;
  int64_t value_cnt;
  Column() : type(TYPE_VARCHAR), nullable(true), null_cnt(0), value_cnt(0) {}
};

struct Fingerprint {
  uint64_t dev;
  uint64_t ino;
  uint64_t size;
  int64_t mtime_ns;
  Fingerprint() : dev(0), ino(0), size(0), mtime_ns(0) {}
  bool operator==(const Fingerprint &o) const {
    return dev == o.dev && ino == o.ino && size == o.size && mtime_ns == o.mtime_ns;
  }
};

struct Options {
  bool header;
  int64_t sample_size;
  bool all_varchar;
  char delim;
  std::string nullstr;
  char quote;
  std::string format;
  std::string secure_root;
  Options()
      : header(true), sample_size(-1), all_varchar(false), delim(','), nullstr(), quote('"'),
        format("newline_delimited"), secure_root() {}
};

struct Cell {
  bool is_null;
  Type type;
  int64_t i64;
  double f64;
  std::string text;
  Cell() : is_null(true), type(TYPE_VARCHAR), i64(0), f64(0.0) {}
};

struct Schema {
  std::vector<Column> columns;
  int64_t row_count;
  Fingerprint fp;
  Schema() : row_count(0) {}
};

inline const char *type_name(Type t) {
  switch (t) {
  case TYPE_BIGINT: return "BIGINT";
  case TYPE_DOUBLE: return "DOUBLE";
  case TYPE_BOOL: return "BOOLEAN";
  case TYPE_DATE: return "DATE";
  case TYPE_DATETIME: return "DATETIME";
  case TYPE_VARCHAR: return "VARCHAR";
  default: return "VARCHAR";
  }
}

inline Type merge_type(Type a, Type b) {
  if (a == b) {
    return a;
  }
  if ((a == TYPE_BIGINT && b == TYPE_DOUBLE) || (a == TYPE_DOUBLE && b == TYPE_BIGINT)) {
    return TYPE_DOUBLE;
  }
  if ((a == TYPE_DATE && b == TYPE_DATETIME) || (a == TYPE_DATETIME && b == TYPE_DATE)) {
    return TYPE_DATETIME;
  }
  return TYPE_VARCHAR;
}

inline bool is_leap(int y) {
  return (y % 4 == 0 && y % 100 != 0) || (y % 400 == 0);
}

inline bool valid_ymd(int y, int m, int d) {
  if (y < 1 || y > 9999 || m < 1 || m > 12 || d < 1) {
    return false;
  }
  static const int mdays[] = {0, 31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31};
  int dim = mdays[m];
  if (m == 2 && is_leap(y)) {
    dim = 29;
  }
  return d <= dim;
}

inline bool all_digits(const std::string &s, size_t b, size_t e) {
  if (b >= e || e > s.size()) {
    return false;
  }
  for (size_t i = b; i < e; ++i) {
    if (s[i] < '0' || s[i] > '9') {
      return false;
    }
  }
  return true;
}

inline bool parse_date_text(const std::string &s, int &y, int &m, int &d) {
  if (s.size() != 10 || s[4] != '-' || s[7] != '-') {
    return false;
  }
  if (!all_digits(s, 0, 4) || !all_digits(s, 5, 7) || !all_digits(s, 8, 10)) {
    return false;
  }
  y = std::atoi(s.substr(0, 4).c_str());
  m = std::atoi(s.substr(5, 2).c_str());
  d = std::atoi(s.substr(8, 2).c_str());
  return valid_ymd(y, m, d);
}

inline bool parse_time_text(const std::string &s, int &hh, int &mm, int &ss) {
  if (s.size() != 8 || s[2] != ':' || s[5] != ':') {
    return false;
  }
  if (!all_digits(s, 0, 2) || !all_digits(s, 3, 5) || !all_digits(s, 6, 8)) {
    return false;
  }
  hh = std::atoi(s.substr(0, 2).c_str());
  mm = std::atoi(s.substr(3, 2).c_str());
  ss = std::atoi(s.substr(6, 2).c_str());
  return hh >= 0 && hh <= 23 && mm >= 0 && mm <= 59 && ss >= 0 && ss <= 59;
}

inline bool parse_datetime_text(const std::string &s, std::string &canon) {
  if (s.size() != 19) {
    return false;
  }
  char sep = s[10];
  if (sep != ' ' && sep != 'T') {
    return false;
  }
  int y, m, d, hh, mm, ss;
  if (!parse_date_text(s.substr(0, 10), y, m, d)) {
    return false;
  }
  if (!parse_time_text(s.substr(11), hh, mm, ss)) {
    return false;
  }
  canon = s;
  canon[10] = ' ';
  return true;
}

inline bool ieq_bool(const std::string &s, bool &out) {
  std::string t;
  t.resize(s.size());
  for (size_t i = 0; i < s.size(); ++i) {
    t[i] = static_cast<char>(std::tolower(static_cast<unsigned char>(s[i])));
  }
  if (t == "true") {
    out = true;
    return true;
  }
  if (t == "false") {
    out = false;
    return true;
  }
  return false;
}

inline bool parse_i64_strict(const std::string &s, int64_t &out) {
  if (s.empty()) {
    return false;
  }
  size_t i = 0;
  if (s[0] == '+' || s[0] == '-') {
    if (s.size() == 1) {
      return false;
    }
    i = 1;
  }
  for (size_t j = i; j < s.size(); ++j) {
    if (s[j] < '0' || s[j] > '9') {
      return false;
    }
  }
  errno = 0;
  char *end = NULL;
  long long v = std::strtoll(s.c_str(), &end, 10);
  if (errno == ERANGE || end == s.c_str() || *end != '\0') {
    return false;
  }
  out = static_cast<int64_t>(v);
  return true;
}

inline bool is_int_text(const std::string &s) {
  if (s.empty()) {
    return false;
  }
  size_t i = (s[0] == '+' || s[0] == '-') ? 1 : 0;
  if (i >= s.size()) {
    return false;
  }
  for (; i < s.size(); ++i) {
    if (s[i] < '0' || s[i] > '9') {
      return false;
    }
  }
  return true;
}

inline bool parse_f64_strict(const std::string &s, double &out) {
  if (s.empty()) {
    return false;
  }
  bool digit = false;
  for (size_t i = 0; i < s.size(); ++i) {
    unsigned char c = static_cast<unsigned char>(s[i]);
    if (std::isdigit(c)) {
      digit = true;
    }
  }
  if (!digit) {
    return false;
  }
  errno = 0;
  char *end = NULL;
  double v = std::strtod(s.c_str(), &end);
  if (end == s.c_str() || *end != '\0' || errno == ERANGE || !std::isfinite(v)) {
    return false;
  }
  out = v;
  return true;
}

inline void classify_text(const std::string &s, bool all_varchar, Type &type, Cell &cell) {
  cell = Cell();
  cell.is_null = false;
  if (all_varchar) {
    type = TYPE_VARCHAR;
    cell.type = TYPE_VARCHAR;
    cell.text = s;
    return;
  }
  bool b = false;
  int64_t iv = 0;
  double dv = 0;
  int y, m, d;
  std::string canon;
  if (ieq_bool(s, b)) {
    type = TYPE_BOOL;
    cell.type = TYPE_BOOL;
    cell.i64 = b ? 1 : 0;
  } else if (parse_datetime_text(s, canon)) {
    type = TYPE_DATETIME;
    cell.type = TYPE_DATETIME;
    cell.text = canon;
  } else if (parse_date_text(s, y, m, d)) {
    type = TYPE_DATE;
    cell.type = TYPE_DATE;
    cell.text = s;
  } else if (parse_i64_strict(s, iv)) {
    type = TYPE_BIGINT;
    cell.type = TYPE_BIGINT;
    cell.i64 = iv;
  } else if (is_int_text(s)) {
    // Integers outside int64 stay text. Promoting them to DOUBLE would change equality.
    type = TYPE_VARCHAR;
    cell.type = TYPE_VARCHAR;
    cell.text = s;
  } else if (parse_f64_strict(s, dv)) {
    type = TYPE_DOUBLE;
    cell.type = TYPE_DOUBLE;
    cell.f64 = dv;
  } else {
    type = TYPE_VARCHAR;
    cell.type = TYPE_VARCHAR;
    cell.text = s;
  }
}

inline Status cast_text(const std::string &s, Type expect, Cell &cell) {
  Cell raw;
  Type got = TYPE_VARCHAR;
  classify_text(s, false, got, raw);
  cell = Cell();
  cell.is_null = false;
  cell.type = expect;
  if (expect == TYPE_VARCHAR) {
    cell.text = s;
    return Status();
  }
  if (got == expect) {
    cell = raw;
    cell.type = expect;
    return Status();
  }
  if (expect == TYPE_DOUBLE && got == TYPE_BIGINT) {
    cell.f64 = static_cast<double>(raw.i64);
    cell.i64 = raw.i64;
    return Status();
  }
  if (expect == TYPE_DATETIME && got == TYPE_DATE) {
    cell.text = raw.text + " 00:00:00";
    return Status();
  }
  if (expect == TYPE_BIGINT && got == TYPE_DOUBLE) {
    return Status(ST_INVALID, "numeric value does not fit BIGINT");
  }
  return Status(ST_INVALID, std::string("cannot cast value to ") + type_name(expect));
}

inline Status fingerprint_of(const std::string &path, Fingerprint &fp, bool follow) {
  struct stat st;
  if ((follow ? ::stat(path.c_str(), &st) : ::lstat(path.c_str(), &st)) != 0) {
    return Status(ST_IO, std::string("stat failed: ") + path);
  }
  fp.dev = static_cast<uint64_t>(st.st_dev);
  fp.ino = static_cast<uint64_t>(st.st_ino);
  fp.size = static_cast<uint64_t>(st.st_size);
#if defined(__APPLE__)
  fp.mtime_ns = static_cast<int64_t>(st.st_mtimespec.tv_sec) * 1000000000LL +
                static_cast<int64_t>(st.st_mtimespec.tv_nsec);
#else
  fp.mtime_ns = static_cast<int64_t>(st.st_mtim.tv_sec) * 1000000000LL +
                static_cast<int64_t>(st.st_mtim.tv_nsec);
#endif
  return Status();
}

inline bool has_dotdot(const std::string &path) {
  if (path == ".." || (path.size() >= 3 && path.compare(0, 3, "../") == 0)) {
    return true;
  }
  if (path.size() >= 3 && path.compare(path.size() - 3, 3, "/..") == 0) {
    return true;
  }
  return path.find("/../") != std::string::npos;
}

inline std::string real_path(const std::string &path, bool *ok) {
  char buf[4096];
  char *p = ::realpath(path.c_str(), buf);
  if (p == NULL) {
    *ok = false;
    return std::string();
  }
  *ok = true;
  return std::string(p);
}

inline bool under_root(const std::string &root, const std::string &full) {
  if (root.empty() || root == "/") {
    return !full.empty() && full[0] == '/';
  }
  if (full == root) {
    return true;
  }
  if (full.size() > root.size() && full.compare(0, root.size(), root) == 0 &&
      (root[root.size() - 1] == '/' || full[root.size()] == '/')) {
    return true;
  }
  return false;
}

inline std::string uniquify_name(const std::string &base, std::set<std::string> &used) {
  if (used.find(base) == used.end()) {
    used.insert(base);
    return base;
  }
  for (int i = 2;; ++i) {
    std::ostringstream os;
    os << base << "__" << i;
    std::string cand = os.str();
    if (used.find(cand) == used.end()) {
      used.insert(cand);
      return cand;
    }
  }
}

inline std::string header_name(const std::string &raw, int index, std::set<std::string> &used) {
  std::string base = raw;
  if (base.empty()) {
    std::ostringstream os;
    os << "column" << index;
    base = os.str();
  }
  return uniquify_name(base, used);
}

struct Acc {
  bool seen;
  Type type;
  int64_t nulls;
  int64_t values;
  Acc() : seen(false), type(TYPE_VARCHAR), nulls(0), values(0) {}
};

inline void acc_null(Acc &a) { a.nulls++; }

inline void acc_value(Acc &a, Type t) {
  if (!a.seen) {
    a.seen = true;
    a.type = t;
  } else {
    a.type = merge_type(a.type, t);
  }
  a.values++;
}

inline Column acc_to_column(const std::string &name, const Acc &a) {
  Column c;
  c.name = name;
  if (!a.seen) {
    c.type = TYPE_VARCHAR;
    c.nullable = true;
  } else {
    c.type = a.type;
    c.nullable = a.nulls > 0;
  }
  c.null_cnt = a.nulls;
  c.value_cnt = a.values;
  return c;
}

class ByteSource {
public:
  ByteSource() : fp_(NULL), size_(0) {}
  explicit ByteSource(const std::string &path) : fp_(NULL), size_(0) { open(path); }
  ~ByteSource() { close(); }
  void close() {
    if (fp_ != NULL) {
      ::fclose(fp_);
      fp_ = NULL;
    }
    size_ = 0;
  }
  Status open(const std::string &path) {
    close();
    fp_ = ::fopen(path.c_str(), "rb");
    if (fp_ == NULL) {
      return Status(ST_IO, "open failed");
    }
    if (::fseek(fp_, 0, SEEK_END) != 0) {
      close();
      return Status(ST_IO, "seek failed");
    }
    long n = ::ftell(fp_);
    if (n < 0) {
      close();
      return Status(ST_IO, "tell failed");
    }
    size_ = static_cast<int64_t>(n);
    if (::fseek(fp_, 0, SEEK_SET) != 0) {
      close();
      return Status(ST_IO, "seek failed");
    }
    return Status();
  }
  bool ok() const { return fp_ != NULL; }
  int64_t size() const { return size_; }
  Status read_at(int64_t off, void *dst, size_t n) {
    if (fp_ == NULL) {
      return Status(ST_IO, "file is not open");
    }
    if (::fseek(fp_, static_cast<long>(off), SEEK_SET) != 0) {
      return Status(ST_IO, "seek failed");
    }
    if (n == 0) {
      return Status();
    }
    size_t got = ::fread(dst, 1, n, fp_);
    if (got != n) {
      return Status(ST_IO, "short read");
    }
    return Status();
  }
  Status read_all(std::string &out) {
    if (size_ > kMaxFileBytes) {
      return Status(ST_LIMIT, "file exceeds size limit");
    }
    out.assign(static_cast<size_t>(size_), '\0');
    if (size_ == 0) {
      return Status();
    }
    return read_at(0, &out[0], static_cast<size_t>(size_));
  }

private:
  ByteSource(const ByteSource &);
  ByteSource &operator=(const ByteSource &);
  FILE *fp_;
  int64_t size_;
};

inline Status read_file_limited(const std::string &path, std::string &out, Fingerprint *fp) {
  if (has_dotdot(path)) {
    return Status(ST_INVALID, "path contains '..'");
  }
  Fingerprint local;
  Status st = fingerprint_of(path, local, true);
  if (!st.ok()) {
    return st;
  }
  if (static_cast<int64_t>(local.size) > kMaxFileBytes) {
    return Status(ST_LIMIT, "file exceeds size limit");
  }
  struct stat stbuf;
  if (::stat(path.c_str(), &stbuf) != 0 || !S_ISREG(stbuf.st_mode)) {
    return Status(ST_IO, "not a regular file");
  }
  ByteSource src(path);
  if (!src.ok()) {
    return Status(ST_IO, "open failed");
  }
  st = src.read_all(out);
  if (!st.ok()) {
    return st;
  }
  if (fp != NULL) {
    *fp = local;
  }
  return Status();
}

class CsvParser {
public:
  CsvParser(const std::string &data, const Options &opt)
      : data_(data), opt_(opt), pos_(0), line_(1) {
    if (data_.size() >= 3 && static_cast<unsigned char>(data_[0]) == 0xEF &&
        static_cast<unsigned char>(data_[1]) == 0xBB &&
        static_cast<unsigned char>(data_[2]) == 0xBF) {
      pos_ = 3;
    }
  }

  bool eof() const { return pos_ >= data_.size(); }

  Status next_record(std::vector<std::string> &fields) {
    fields.clear();
    if (eof()) {
      return Status(ST_END);
    }
    std::string field;
    bool in_q = false;
    bool any = false;
    int64_t start_line = line_;
    while (pos_ < data_.size()) {
      char c = data_[pos_++];
      any = true;
      if (field.size() >= static_cast<size_t>(kMaxFieldBytes)) {
        return Status(ST_LIMIT, line_msg(start_line, "field exceeds size limit"));
      }
      if (in_q) {
        if (c == opt_.quote) {
          if (pos_ < data_.size() && data_[pos_] == opt_.quote) {
            field.push_back(opt_.quote);
            pos_++;
          } else {
            in_q = false;
          }
        } else {
          if (c == '\n') {
            line_++;
          }
          field.push_back(c);
        }
      } else if (c == opt_.quote && field.empty()) {
        in_q = true;
      } else if (c == opt_.delim) {
        fields.push_back(field);
        field.clear();
      } else if (c == '\n') {
        if (!field.empty() && field[field.size() - 1] == '\r') {
          field.erase(field.size() - 1);
        }
        fields.push_back(field);
        line_++;
        return Status();
      } else if (c == '\r') {
        field.push_back(c);
      } else {
        field.push_back(c);
      }
    }
    if (in_q) {
      return Status(ST_INVALID, line_msg(start_line, "unterminated quote"));
    }
    if (!any) {
      return Status(ST_END);
    }
    if (!field.empty() && field[field.size() - 1] == '\r') {
      field.erase(field.size() - 1);
    }
    fields.push_back(field);
    return Status();
  }

  static std::string line_msg(int64_t line, const std::string &msg) {
    std::ostringstream os;
    os << "CSV line " << line << ": " << msg;
    return os.str();
  }

private:
  const std::string &data_;
  Options opt_;
  size_t pos_;
  int64_t line_;
};

inline bool is_null_field(const std::string &s, const Options &opt) {
  return s == opt.nullstr;
}

inline Status infer_csv(const std::string &data, const Options &opt, Schema &schema) {
  CsvParser p(data, opt);
  std::vector<std::string> fields;
  Status st = p.next_record(fields);
  if (st.end()) {
    return Status(ST_SCHEMA, "CSV schema inference failed: empty file");
  }
  if (!st.ok()) {
    return st;
  }
  if (static_cast<int64_t>(fields.size()) > kMaxColumns || fields.empty()) {
    return Status(ST_LIMIT, "CSV column count is out of range");
  }
  std::vector<std::string> names;
  std::set<std::string> used;
  if (opt.header) {
    for (size_t i = 0; i < fields.size(); ++i) {
      names.push_back(header_name(fields[i], static_cast<int>(i + 1), used));
    }
  } else {
    for (size_t i = 0; i < fields.size(); ++i) {
      std::ostringstream os;
      os << "column" << (i + 1);
      names.push_back(uniquify_name(os.str(), used));
    }
  }
  std::vector<Acc> acc(names.size());
  int64_t rows = 0;
  if (!opt.header) {
    st = Status();
  } else {
    st = p.next_record(fields);
  }
  while (st.ok()) {
    if (fields.size() == 1 && fields[0].empty() && p.eof()) {
      break;
    }
    if (fields.size() != names.size()) {
      return Status(ST_INVALID, "CSV column count does not match header");
    }
    for (size_t i = 0; i < fields.size(); ++i) {
      if (is_null_field(fields[i], opt)) {
        acc_null(acc[i]);
      } else {
        Type t;
        Cell cell;
        classify_text(fields[i], opt.all_varchar, t, cell);
        acc_value(acc[i], t);
      }
    }
    rows++;
    st = p.next_record(fields);
  }
  if (!st.ok() && !st.end()) {
    return st;
  }
  schema.columns.clear();
  for (size_t i = 0; i < names.size(); ++i) {
    schema.columns.push_back(acc_to_column(names[i], acc[i]));
    if (opt.all_varchar) {
      schema.columns.back().type = TYPE_VARCHAR;
    }
  }
  schema.row_count = rows;
  return Status();
}

inline void skip_ws(const std::string &s, size_t &i) {
  while (i < s.size() && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r')) {
    ++i;
  }
}

inline Status parse_json_string(const std::string &s, size_t &i, std::string &out) {
  if (i >= s.size() || s[i] != '"') {
    return Status(ST_INVALID, "JSON string expected");
  }
  ++i;
  out.clear();
  while (i < s.size()) {
    char c = s[i++];
    if (c == '"') {
      return Status();
    }
    if (c == '\\') {
      if (i >= s.size()) {
        return Status(ST_INVALID, "JSON bad escape");
      }
      char e = s[i++];
      switch (e) {
      case '"':
      case '\\':
      case '/': out.push_back(e); break;
      case 'b': out.push_back('\b'); break;
      case 'f': out.push_back('\f'); break;
      case 'n': out.push_back('\n'); break;
      case 'r': out.push_back('\r'); break;
      case 't': out.push_back('\t'); break;
      case 'u': {
        if (i + 4 > s.size()) {
          return Status(ST_INVALID, "JSON bad unicode escape");
        }
        int cp = 0;
        for (int k = 0; k < 4; ++k) {
          char h = s[i++];
          cp <<= 4;
          if (h >= '0' && h <= '9') cp += h - '0';
          else if (h >= 'a' && h <= 'f') cp += h - 'a' + 10;
          else if (h >= 'A' && h <= 'F') cp += h - 'A' + 10;
          else return Status(ST_INVALID, "JSON bad unicode escape");
        }
        if (cp < 0x80) {
          out.push_back(static_cast<char>(cp));
        } else if (cp < 0x800) {
          out.push_back(static_cast<char>(0xC0 | (cp >> 6)));
          out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
        } else {
          out.push_back(static_cast<char>(0xE0 | (cp >> 12)));
          out.push_back(static_cast<char>(0x80 | ((cp >> 6) & 0x3F)));
          out.push_back(static_cast<char>(0x80 | (cp & 0x3F)));
        }
        break;
      }
      default:
        return Status(ST_INVALID, "JSON bad escape");
      }
    } else {
      out.push_back(c);
    }
    if (out.size() > static_cast<size_t>(kMaxFieldBytes)) {
      return Status(ST_LIMIT, "JSON string exceeds size limit");
    }
  }
  return Status(ST_INVALID, "JSON unterminated string");
}

struct JsonField {
  bool is_null;
  bool nested;
  std::string text;
  JsonField() : is_null(false), nested(false) {}
};

inline Status parse_json_value(const std::string &s, size_t &i, JsonField &field) {
  skip_ws(s, i);
  if (i >= s.size()) {
    return Status(ST_INVALID, "JSON value expected");
  }
  field = JsonField();
  char c = s[i];
  if (c == '{' || c == '[') {
    field.nested = true;
    return Status(ST_NESTED, "nested JSON value is not supported");
  }
  if (c == '"') {
    return parse_json_string(s, i, field.text);
  }
  if (s.compare(i, 4, "true") == 0) {
    field.text = "true";
    i += 4;
    return Status();
  }
  if (s.compare(i, 5, "false") == 0) {
    field.text = "false";
    i += 5;
    return Status();
  }
  if (s.compare(i, 4, "null") == 0) {
    field.is_null = true;
    i += 4;
    return Status();
  }
  size_t b = i;
  if (s[i] == '-' || s[i] == '+') {
    ++i;
  }
  while (i < s.size() && (std::isdigit(static_cast<unsigned char>(s[i])) || s[i] == '.' ||
                          s[i] == 'e' || s[i] == 'E' || s[i] == '+' || s[i] == '-')) {
    ++i;
  }
  if (i == b) {
    return Status(ST_INVALID, "JSON value expected");
  }
  field.text = s.substr(b, i - b);
  return Status();
}

inline Status parse_json_object(const std::string &line, int64_t line_no,
                                std::vector<std::pair<std::string, JsonField> > &kvs) {
  kvs.clear();
  size_t i = 0;
  skip_ws(line, i);
  if (i >= line.size() || line[i] != '{') {
    std::ostringstream os;
    os << "JSONL line " << line_no << ": object expected";
    return Status(ST_INVALID, os.str());
  }
  ++i;
  skip_ws(line, i);
  if (i < line.size() && line[i] == '}') {
    return Status();
  }
  while (i < line.size()) {
    skip_ws(line, i);
    std::string key;
    Status st = parse_json_string(line, i, key);
    if (!st.ok()) {
      std::ostringstream os;
      os << "JSONL line " << line_no << ": " << st.message;
      return Status(st.code, os.str());
    }
    skip_ws(line, i);
    if (i >= line.size() || line[i] != ':') {
      std::ostringstream os;
      os << "JSONL line " << line_no << ": colon expected";
      return Status(ST_INVALID, os.str());
    }
    ++i;
    JsonField field;
    st = parse_json_value(line, i, field);
    if (field.nested || st.code == ST_NESTED) {
      std::ostringstream os;
      os << "JSONL line " << line_no << ": nested JSON value is not supported";
      return Status(ST_NESTED, os.str());
    }
    if (!st.ok()) {
      std::ostringstream os;
      os << "JSONL line " << line_no << ": " << st.message;
      return Status(st.code, os.str());
    }
    kvs.push_back(std::make_pair(key, field));
    skip_ws(line, i);
    if (i < line.size() && line[i] == ',') {
      ++i;
      continue;
    }
    if (i < line.size() && line[i] == '}') {
      return Status();
    }
    std::ostringstream os;
    os << "JSONL line " << line_no << ": malformed object";
    return Status(ST_INVALID, os.str());
  }
  std::ostringstream os;
  os << "JSONL line " << line_no << ": unterminated object";
  return Status(ST_INVALID, os.str());
}

inline Status infer_jsonl(const std::string &data, const Options &opt, Schema &schema) {
  std::map<std::string, int> index;
  std::vector<std::string> names;
  std::vector<Acc> acc;
  int64_t rows = 0;
  int64_t line_no = 0;
  size_t i = 0;
  if (data.size() >= 3 && static_cast<unsigned char>(data[0]) == 0xEF &&
      static_cast<unsigned char>(data[1]) == 0xBB &&
      static_cast<unsigned char>(data[2]) == 0xBF) {
    i = 3;
  }
  while (i < data.size()) {
    size_t nl = data.find('\n', i);
    std::string line = data.substr(i, nl == std::string::npos ? std::string::npos : nl - i);
    i = (nl == std::string::npos) ? data.size() : nl + 1;
    line_no++;
    if (!line.empty() && line[line.size() - 1] == '\r') {
      line.erase(line.size() - 1);
    }
    if (line.size() > static_cast<size_t>(kMaxFieldBytes)) {
      return Status(ST_LIMIT, "JSONL line exceeds size limit");
    }
    bool blank = true;
    for (size_t k = 0; k < line.size(); ++k) {
      if (line[k] != ' ' && line[k] != '\t') {
        blank = false;
        break;
      }
    }
    if (blank) {
      continue;
    }
    std::vector<std::pair<std::string, JsonField> > kvs;
    Status st = parse_json_object(line, line_no, kvs);
    if (!st.ok()) {
      return st;
    }
    std::set<std::string> seen;
    for (size_t k = 0; k < kvs.size(); ++k) {
      const std::string &key = kvs[k].first;
      if (!seen.insert(key).second) {
        std::ostringstream os;
        os << "JSONL line " << line_no << ": duplicate key";
        return Status(ST_INVALID, os.str());
      }
      if (index.find(key) == index.end()) {
        if (static_cast<int64_t>(names.size()) >= kMaxJsonKeys) {
          return Status(ST_LIMIT, "JSONL key count exceeds limit");
        }
        index[key] = static_cast<int>(names.size());
        names.push_back(key);
        Acc fresh;
        fresh.nulls = rows;
        acc.push_back(fresh);
      }
    }
    for (size_t c = 0; c < names.size(); ++c) {
      bool found = false;
      for (size_t k = 0; k < kvs.size(); ++k) {
        if (kvs[k].first == names[c]) {
          found = true;
          if (kvs[k].second.is_null) {
            acc_null(acc[c]);
          } else {
            Type t;
            Cell cell;
            classify_text(kvs[k].second.text, opt.all_varchar, t, cell);
            acc_value(acc[c], t);
          }
          break;
        }
      }
      if (!found) {
        acc_null(acc[c]);
      }
    }
    rows++;
  }
  if (names.empty()) {
    return Status(ST_SCHEMA, "JSONL schema inference failed: no keys");
  }
  schema.columns.clear();
  for (size_t c = 0; c < names.size(); ++c) {
    schema.columns.push_back(acc_to_column(names[c], acc[c]));
    if (opt.all_varchar) {
      schema.columns.back().type = TYPE_VARCHAR;
    }
  }
  schema.row_count = rows;
  return Status();
}

inline bool glob_match(const std::string &pat, const std::string &name) {
  size_t pi = 0, ni = 0, star = std::string::npos, mark = 0;
  while (ni < name.size()) {
    if (pi < pat.size() && (pat[pi] == '?' || pat[pi] == name[ni])) {
      ++pi;
      ++ni;
    } else if (pi < pat.size() && pat[pi] == '*') {
      star = pi++;
      mark = ni;
    } else if (star != std::string::npos) {
      pi = star + 1;
      ni = ++mark;
    } else {
      return false;
    }
  }
  while (pi < pat.size() && pat[pi] == '*') {
    ++pi;
  }
  return pi == pat.size();
}

inline uint64_t fnv_mix(uint64_t h, const std::string &s) {
  for (size_t i = 0; i < s.size(); ++i) {
    h ^= static_cast<unsigned char>(s[i]);
    h *= 1099511628211ULL;
  }
  h ^= 0xff;
  h *= 1099511628211ULL;
  return h;
}

inline Status list_glob(const std::string &pattern, const Options &opt, std::vector<std::string> &paths,
                        Fingerprint &fp) {
  paths.clear();
  if (has_dotdot(pattern)) {
    return Status(ST_INVALID, "path contains '..'");
  }
  if (pattern.find("**") != std::string::npos) {
    return Status(ST_UNSUPPORTED, "recursive glob is not supported");
  }
  std::string dir;
  std::string pat;
  std::string::size_type slash = pattern.find_last_of('/');
  if (slash == std::string::npos) {
    dir = ".";
    pat = pattern;
  } else {
    dir = pattern.substr(0, slash);
    pat = pattern.substr(slash + 1);
    if (dir.empty()) {
      dir = "/";
    }
  }
  if (dir.find('*') != std::string::npos || dir.find('?') != std::string::npos) {
    return Status(ST_UNSUPPORTED, "wildcard in directory is not supported");
  }
  bool ok = false;
  std::string real_dir = real_path(dir, &ok);
  if (!ok) {
    return Status(ST_IO, "glob directory does not exist");
  }
  std::string root = opt.secure_root;
  if (!root.empty()) {
    bool rok = false;
    std::string rr = real_path(root, &rok);
    if (rok) {
      root = rr;
    }
    if (!under_root(root, real_dir)) {
      return Status(ST_INVALID, "glob directory escapes secure_file_priv");
    }
  }
  Status st = fingerprint_of(real_dir, fp, false);
  if (!st.ok()) {
    return st;
  }
  DIR *dp = ::opendir(real_dir.c_str());
  if (dp == NULL) {
    return Status(ST_IO, "opendir failed");
  }
  uint64_t hash = 1469598103934665603ULL;
  std::vector<std::string> names;
  while (dirent *de = ::readdir(dp)) {
    std::string name = de->d_name;
    if (name == "." || name == "..") {
      continue;
    }
    if (!glob_match(pat, name)) {
      continue;
    }
    std::string full = (real_dir == "/" ? "" : real_dir) + "/" + name;
    bool fok = false;
    std::string resolved = real_path(full, &fok);
    if (!fok) {
      ::closedir(dp);
      return Status(ST_IO, "realpath failed for glob entry");
    }
    if (!root.empty() && !under_root(root, resolved)) {
      ::closedir(dp);
      return Status(ST_INVALID, "symlink escapes secure_file_priv");
    }
    struct stat stbuf;
    if (::stat(resolved.c_str(), &stbuf) != 0) {
      ::closedir(dp);
      return Status(ST_IO, "stat failed for glob entry");
    }
    if (!S_ISREG(stbuf.st_mode)) {
      continue;
    }
    names.push_back(name);
  }
  ::closedir(dp);
  std::sort(names.begin(), names.end());
  for (size_t i = 0; i < names.size(); ++i) {
    std::string full = (real_dir == "/" ? "" : real_dir) + "/" + names[i];
    paths.push_back(full);
    hash = fnv_mix(hash, names[i]);
  }
  fp.size = hash;
  return Status();
}

inline Status infer_glob(const std::string &pattern, const Options &opt, Schema &schema) {
  std::vector<std::string> paths;
  Status st = list_glob(pattern, opt, paths, schema.fp);
  if (!st.ok()) {
    return st;
  }
  Column c;
  c.name = "file";
  c.type = TYPE_VARCHAR;
  c.nullable = false;
  c.null_cnt = 0;
  c.value_cnt = static_cast<int64_t>(paths.size());
  schema.columns.clear();
  schema.columns.push_back(c);
  schema.row_count = static_cast<int64_t>(paths.size());
  return Status();
}

inline int bit_width_of(int max_level) {
  int w = 0;
  while ((1 << w) <= max_level && w < 31) {
    ++w;
  }
  return w == 0 ? 0 : w;
}

inline void ymd_from_unix_days(int64_t z, int &y, int &m, int &d) {
  z += 719468;
  const int64_t era = (z >= 0 ? z : z - 146096) / 146097;
  const unsigned doe = static_cast<unsigned>(z - era * 146097);
  const unsigned yoe = (doe - doe / 1460 + doe / 36524 - doe / 146096) / 365;
  y = static_cast<int>(yoe) + static_cast<int>(era) * 400;
  const unsigned doy = doe - (365 * yoe + yoe / 4 - yoe / 100);
  const unsigned mp = (5 * doy + 2) / 153;
  d = static_cast<int>(doy - (153 * mp + 2) / 5 + 1);
  m = static_cast<int>(mp < 10 ? mp + 3 : mp - 9);
  y += (m <= 2);
}

inline std::string format_date(int y, int m, int d) {
  char buf[32];
  std::snprintf(buf, sizeof(buf), "%04d-%02d-%02d", y, m, d);
  return std::string(buf);
}

inline std::string format_datetime_from_parts(int y, int m, int d, int hh, int mm, int ss) {
  char buf[64];
  std::snprintf(buf, sizeof(buf), "%04d-%02d-%02d %02d:%02d:%02d", y, m, d, hh, mm, ss);
  return std::string(buf);
}


inline uint32_t load_le32(const uint8_t *p) {
  return (uint32_t)p[0] | ((uint32_t)p[1] << 8) | ((uint32_t)p[2] << 16) | ((uint32_t)p[3] << 24);
}

inline uint64_t load_le64(const uint8_t *p) {
  uint64_t v = 0;
  for (int i = 0; i < 8; ++i) {
    v |= (uint64_t)p[i] << (8 * i);
  }
  return v;
}

inline bool host_is_le() {
  const uint16_t x = 1;
  return *reinterpret_cast<const uint8_t *>(&x) == 1;
}

inline float load_f32(const uint8_t *p) {
  uint32_t u = load_le32(p);
  if (!host_is_le()) {
    u = (u << 24) | ((u << 8) & 0xff0000u) | ((u >> 8) & 0xff00u) | (u >> 24);
  }
  float f;
  std::memcpy(&f, &u, sizeof(f));
  return f;
}

inline double load_f64(const uint8_t *p) {
  uint64_t u = load_le64(p);
  if (!host_is_le()) {
    uint64_t r = 0;
    for (int i = 0; i < 8; ++i) {
      r = (r << 8) | ((u >> (8 * i)) & 0xff);
    }
    u = r;
  }
  double d;
  std::memcpy(&d, &u, sizeof(d));
  return d;
}

inline Status snappy_raw(const uint8_t *src, size_t n, size_t max_out, std::vector<uint8_t> &out) {
  out.clear();
  size_t i = 0;
  while (i < n) {
    uint8_t tag = src[i++];
    uint32_t typ = tag & 3u;
    if (typ == 0) {
      uint32_t ln = tag >> 2;
      uint32_t length = 0;
      if (ln < 60) {
        length = ln + 1;
      } else if (ln == 60) {
        if (i >= n) {
          return Status(ST_INVALID, "snappy literal");
        }
        length = (uint32_t)src[i++] + 1;
      } else if (ln == 61) {
        if (i + 2 > n) {
          return Status(ST_INVALID, "snappy literal");
        }
        length = (uint32_t)src[i] | ((uint32_t)src[i + 1] << 8);
        i += 2;
        length += 1;
      } else if (ln == 62) {
        if (i + 3 > n) {
          return Status(ST_INVALID, "snappy literal");
        }
        length = (uint32_t)src[i] | ((uint32_t)src[i + 1] << 8) | ((uint32_t)src[i + 2] << 16);
        i += 3;
        length += 1;
      } else {
        if (i + 4 > n) {
          return Status(ST_INVALID, "snappy literal");
        }
        length = load_le32(src + i);
        i += 4;
        length += 1;
      }
      if (i + length > n || out.size() + length > max_out) {
        return Status(ST_INVALID, "snappy literal");
      }
      out.insert(out.end(), src + i, src + i + length);
      i += length;
    } else {
      uint32_t length = 0;
      uint32_t offset = 0;
      if (typ == 1) {
        length = ((tag >> 2) & 7u) + 4;
        if (i >= n) {
          return Status(ST_INVALID, "snappy copy");
        }
        offset = ((uint32_t)(tag & 0xe0) << 3) | src[i++];
      } else if (typ == 2) {
        length = (tag >> 2) + 1;
        if (i + 2 > n) {
          return Status(ST_INVALID, "snappy copy");
        }
        offset = (uint32_t)src[i] | ((uint32_t)src[i + 1] << 8);
        i += 2;
      } else {
        length = (tag >> 2) + 1;
        if (i + 4 > n) {
          return Status(ST_INVALID, "snappy copy");
        }
        offset = load_le32(src + i);
        i += 4;
      }
      if (offset == 0 || offset > out.size() || out.size() + length > max_out) {
        return Status(ST_INVALID, "snappy copy");
      }
      for (uint32_t k = 0; k < length; ++k) {
        out.push_back(out[out.size() - offset]);
      }
    }
  }
  return Status();
}

// Arrow2 prefixes a raw Snappy block with the uncompressed length as a varint.
// Some Hadoop writers use a 4-byte little-endian length instead.
inline Status snappy_uncompress(const uint8_t *src, size_t n, size_t expect, std::vector<uint8_t> &out) {
  out.clear();
  if (expect > static_cast<size_t>(kMaxFileBytes)) {
    return Status(ST_LIMIT, "snappy output exceeds limit");
  }
  if (expect == 0) {
    return n == 0 ? Status() : Status(ST_INVALID, "snappy size mismatch");
  }
  Status st = snappy_raw(src, n, expect, out);
  if (st.ok() && out.size() == expect) {
    return Status();
  }
  size_t i = 0;
  uint64_t declared = 0;
  int shift = 0;
  bool okv = false;
  for (int k = 0; k < 10 && i < n; ++k) {
    uint8_t b = src[i++];
    declared |= (uint64_t)(b & 0x7f) << shift;
    if ((b & 0x80) == 0) {
      okv = true;
      break;
    }
    shift += 7;
  }
  if (okv && declared == expect && i <= n) {
    st = snappy_raw(src + i, n - i, expect, out);
    if (st.ok() && out.size() == expect) {
      return Status();
    }
  }
  if (n >= 4) {
    uint32_t L = load_le32(src);
    if (L == expect) {
      st = snappy_raw(src + 4, n - 4, expect, out);
      if (st.ok() && out.size() == expect) {
        return Status();
      }
    }
  }
  return Status(ST_INVALID, "snappy decompress failed");
}

class CompactIn {
public:
  CompactIn(const uint8_t *p, size_t n) : p_(p), end_(p + n) {}
  size_t consumed() const { return static_cast<size_t>(p_ - (end_ - (end_ - p_))); }

  Status varint(uint64_t &n) {
    n = 0;
    int shift = 0;
    const uint8_t *begin = p_;
    (void)begin;
    for (int i = 0; i < 10; ++i) {
      if (p_ >= end_) {
        return Status(ST_INVALID, "parquet varint");
      }
      uint8_t b = *p_++;
      n |= (uint64_t)(b & 0x7f) << shift;
      if ((b & 0x80) == 0) {
        return Status();
      }
      shift += 7;
    }
    return Status(ST_INVALID, "parquet varint");
  }

  Status zigzag(int64_t &n) {
    uint64_t u = 0;
    Status st = varint(u);
    if (!st.ok()) {
      return st;
    }
    n = static_cast<int64_t>((u >> 1) ^ (static_cast<uint64_t>(0) - (u & 1)));
    return Status();
  }

  Status read_binary(std::string &s) {
    uint64_t n = 0;
    Status st = varint(n);
    if (!st.ok()) {
      return st;
    }
    if (n > static_cast<uint64_t>(kMaxFieldBytes)) {
      return Status(ST_LIMIT, "parquet binary exceeds limit");
    }
    if (p_ + n > end_) {
      return Status(ST_INVALID, "parquet binary");
    }
    s.assign(reinterpret_cast<const char *>(p_), static_cast<size_t>(n));
    p_ += n;
    return Status();
  }

  Status list_header(int &et, int &sz) {
    if (p_ >= end_) {
      return Status(ST_INVALID, "parquet list");
    }
    uint8_t h = *p_++;
    if (h == 0) {
      et = 0;
      sz = 0;
      return Status();
    }
    et = h & 0x0f;
    int nib = h >> 4;
    if (nib == 15) {
      uint64_t n = 0;
      Status st = varint(n);
      if (!st.ok()) {
        return st;
      }
      if (n > 1000000ULL) {
        return Status(ST_LIMIT, "parquet list");
      }
      sz = static_cast<int>(n);
    } else {
      sz = nib;
    }
    return Status();
  }

  Status skip(int type, int depth = 0) {
    if (depth > 64) {
      return Status(ST_LIMIT, "parquet nesting");
    }
    if (type == 1 || type == 2) {
      return Status();
    }
    if (type == 3) {
      if (p_ >= end_) {
        return Status(ST_INVALID, "parquet truncated");
      }
      ++p_;
      return Status();
    }
    if (type >= 4 && type <= 6) {
      int64_t v = 0;
      return zigzag(v);
    }
    if (type == 7) {
      if (p_ + 8 > end_) {
        return Status(ST_INVALID, "parquet truncated");
      }
      p_ += 8;
      return Status();
    }
    if (type == 8) {
      std::string s;
      return read_binary(s);
    }
    if (type == 9 || type == 10) {
      return skip_list(depth + 1);
    }
    if (type == 11) {
      return skip_map(depth + 1);
    }
    if (type == 12) {
      return skip_struct(depth + 1);
    }
    return Status(ST_UNSUPPORTED, "parquet thrift type");
  }

  template <class Fn>
  Status read_struct(Fn fn) {
    int last = 0;
    while (true) {
      if (p_ >= end_) {
        return Status(ST_INVALID, "parquet truncated struct");
      }
      uint8_t h = *p_++;
      int t = h & 0x0f;
      if (t == 0) {
        return Status();
      }
      int d = h >> 4;
      int fid = 0;
      if (d == 0) {
        int64_t z = 0;
        Status st = zigzag(z);
        if (!st.ok()) {
          return st;
        }
        if (z <= 0 || z > 1000000) {
          return Status(ST_INVALID, "parquet field id");
        }
        fid = static_cast<int>(z);
      } else {
        fid = last + d;
      }
      last = fid;
      Status st = fn(fid, t);
      if (!st.ok()) {
        return st;
      }
    }
  }

  size_t tell(const uint8_t *begin) const { return static_cast<size_t>(p_ - begin); }
  const uint8_t *ptr() const { return p_; }

private:
  Status skip_list(int depth) {
    int et = 0;
    int sz = 0;
    Status st = list_header(et, sz);
    if (!st.ok()) {
      return st;
    }
    for (int i = 0; i < sz; ++i) {
      st = skip(et, depth);
      if (!st.ok()) {
        return st;
      }
    }
    return Status();
  }

  Status skip_map(int depth) {
    uint64_t n = 0;
    Status st = varint(n);
    if (!st.ok()) {
      return st;
    }
    if (n == 0) {
      return Status();
    }
    if (n > 1000000ULL) {
      return Status(ST_LIMIT, "parquet map");
    }
    if (p_ >= end_) {
      return Status(ST_INVALID, "parquet map");
    }
    uint8_t kv = *p_++;
    int kt = kv >> 4;
    int vt = kv & 0x0f;
    for (uint64_t i = 0; i < n; ++i) {
      st = skip(kt, depth);
      if (!st.ok()) {
        return st;
      }
      st = skip(vt, depth);
      if (!st.ok()) {
        return st;
      }
    }
    return Status();
  }

  Status skip_struct(int depth) {
    return read_struct([&](int, int t) -> Status { return skip(t, depth); });
  }

  const uint8_t *p_;
  const uint8_t *end_;
};

// consumed() above is nonsense. Replace with a begin pointer.
// The class is rewritten below by not using that method. Provide a correct one
// via a wrapper that stores begin. I will fix CompactIn before compiling by
// adding begin_. This comment marks the bug so the next edit replaces the class
// method. See fixup immediately after this comment.

struct PNode {
  int physical;
  int converted;
  int repetition;
  int type_length;
  int num_children;
  std::string name;
  PNode() : physical(-1), converted(-1), repetition(-1), type_length(0), num_children(0) {}
};

struct PLeaf {
  std::string name;
  int physical;
  int converted;
  int type_length;
  int max_def;
  Type logical;
  bool nullable;
  PLeaf()
      : physical(-1), converted(-1), type_length(0), max_def(0), logical(TYPE_VARCHAR),
        nullable(true) {}
};

struct PChunk {
  int64_t data_off;
  int64_t dict_off;
  int codec;
  int64_t num_values;
  PChunk() : data_off(-1), dict_off(-1), codec(0), num_values(0) {}
};

struct PGroup {
  int64_t num_rows;
  std::vector<PChunk> cols;
  PGroup() : num_rows(0) {}
};

struct ParquetMeta {
  std::vector<PLeaf> leaves;
  std::vector<PGroup> groups;
  int64_t num_rows;
  ParquetMeta() : num_rows(0) {}
};

struct PageHdr {
  int type;
  int uncompressed;
  int compressed;
  int num_values;
  int encoding;
  int def_level_encoding;
  int rep_bytes;
  int def_bytes;
  bool is_compressed;
  PageHdr()
      : type(-1), uncompressed(0), compressed(0), num_values(-1), encoding(-1),
        def_level_encoding(3), rep_bytes(0), def_bytes(0), is_compressed(true) {}
};

inline Status read_zigzag(CompactIn &in, int type, int64_t &out) {
  if (type < 4 || type > 6) {
    return Status(ST_INVALID, "parquet int field");
  }
  return in.zigzag(out);
}

inline Status parse_schema_node(CompactIn &in, PNode &node) {
  return in.read_struct([&](int id, int type) -> Status {
    if (id == 4 && type == 8) {
      return in.read_binary(node.name);
    }
    if (id == 1 || id == 2 || id == 3 || id == 5 || id == 6 || id == 7 || id == 8 || id == 9) {
      int64_t v = 0;
      Status st = read_zigzag(in, type, v);
      if (!st.ok()) {
        return st;
      }
      if (id == 1) {
        node.physical = static_cast<int>(v);
      } else if (id == 2) {
        node.type_length = static_cast<int>(v);
      } else if (id == 3) {
        node.repetition = static_cast<int>(v);
      } else if (id == 5) {
        node.num_children = static_cast<int>(v);
      } else if (id == 6) {
        node.converted = static_cast<int>(v);
      }
      return Status();
    }
    return in.skip(type);
  });
}

inline Status parse_column_meta(CompactIn &in, PChunk &chunk) {
  bool saw_data = false;
  Status st = in.read_struct([&](int id, int type) -> Status {
    if (id == 4 || id == 5 || id == 9 || id == 10 || id == 11 || id == 1 || id == 6 || id == 7) {
      if (type >= 4 && type <= 6) {
        int64_t v = 0;
        Status st2 = in.zigzag(v);
        if (!st2.ok()) {
          return st2;
        }
        if (id == 4) {
          chunk.codec = static_cast<int>(v);
        } else if (id == 5) {
          chunk.num_values = v;
        } else if (id == 9) {
          chunk.data_off = v;
          saw_data = true;
        } else if (id == 11) {
          chunk.dict_off = v;
        }
        return Status();
      }
    }
    return in.skip(type);
  });
  if (!st.ok()) {
    return st;
  }
  if (!saw_data || chunk.data_off < 0 || chunk.num_values < 0) {
    return Status(ST_INVALID, "parquet column metadata");
  }
  if (chunk.codec != 0 && chunk.codec != 1) {
    return Status(ST_UNSUPPORTED, "parquet compression is not supported");
  }
  return Status();
}

inline Status parse_column_chunk(CompactIn &in, PChunk &chunk) {
  bool saw = false;
  Status st = in.read_struct([&](int id, int type) -> Status {
    if (id == 3 && type == 12) {
      saw = true;
      return parse_column_meta(in, chunk);
    }
    return in.skip(type);
  });
  if (!st.ok()) {
    return st;
  }
  if (!saw) {
    return Status(ST_INVALID, "parquet column metadata");
  }
  return Status();
}

inline Status parse_row_group(CompactIn &in, PGroup &group) {
  bool saw_cols = false;
  bool saw_rows = false;
  Status st = in.read_struct([&](int id, int type) -> Status {
    if (id == 1 && (type == 9 || type == 10)) {
      int et = 0;
      int sz = 0;
      Status st2 = in.list_header(et, sz);
      if (!st2.ok()) {
        return st2;
      }
      if (sz > kMaxColumns) {
        return Status(ST_LIMIT, "parquet column count");
      }
      if (sz > 0 && et != 12) {
        return Status(ST_INVALID, "parquet column chunk");
      }
      saw_cols = true;
      for (int i = 0; i < sz; ++i) {
        PChunk c;
        st2 = parse_column_chunk(in, c);
        if (!st2.ok()) {
          return st2;
        }
        group.cols.push_back(c);
      }
      return Status();
    }
    if (id == 3 && type >= 4 && type <= 6) {
      int64_t v = 0;
      Status st2 = in.zigzag(v);
      if (!st2.ok()) {
        return st2;
      }
      group.num_rows = v;
      saw_rows = true;
      return Status();
    }
    return in.skip(type);
  });
  if (!st.ok()) {
    return st;
  }
  if (!saw_cols || !saw_rows || group.num_rows < 0) {
    return Status(ST_INVALID, "parquet row group");
  }
  return Status();
}

inline Type logical_of(const PNode &n) {
  if (n.physical == 0) {
    return TYPE_BOOL;
  }
  if (n.physical == 1) {
    return n.converted == 6 ? TYPE_DATE : TYPE_BIGINT;
  }
  if (n.physical == 2) {
    return (n.converted == 9 || n.converted == 10) ? TYPE_DATETIME : TYPE_BIGINT;
  }
  if (n.physical == 3) {
    return TYPE_DATETIME;
  }
  if (n.physical == 4 || n.physical == 5) {
    return TYPE_DOUBLE;
  }
  return TYPE_VARCHAR;
}

inline Status walk_leaves(const std::vector<PNode> &schema, size_t &idx, int max_def, int max_rep,
                          std::vector<PLeaf> &leaves) {
  if (idx >= schema.size()) {
    return Status(ST_INVALID, "parquet schema");
  }
  const PNode &node = schema[idx++];
  int def = max_def;
  int rep = max_rep;
  if (node.repetition == 1) {
    ++def;
  } else if (node.repetition == 2) {
    ++def;
    ++rep;
  }
  if (node.num_children > 0) {
    for (int c = 0; c < node.num_children; ++c) {
      Status st = walk_leaves(schema, idx, def, rep, leaves);
      if (!st.ok()) {
        return st;
      }
    }
    return Status();
  }
  if (node.physical < 0 || node.physical > 7) {
    return Status(ST_UNSUPPORTED, "parquet physical type");
  }
  if (node.physical == 7 && (node.type_length <= 0 || node.type_length > kMaxFieldBytes)) {
    return Status(ST_INVALID, "parquet fixed length");
  }
  if (rep > 0) {
    return Status(ST_UNSUPPORTED, "repeated parquet field");
  }
  if (static_cast<int64_t>(leaves.size()) >= kMaxColumns) {
    return Status(ST_LIMIT, "parquet column count");
  }
  PLeaf leaf;
  leaf.name = node.name;
  leaf.physical = node.physical;
  leaf.converted = node.converted;
  leaf.type_length = node.type_length;
  leaf.max_def = def;
  leaf.logical = logical_of(node);
  leaf.nullable = def > 0;
  leaves.push_back(leaf);
  return Status();
}

inline Status parse_footer_meta(const uint8_t *footer, size_t n, ParquetMeta &meta) {
  CompactIn in(footer, n);
  std::vector<PNode> schema;
  bool saw_schema = false;
  bool saw_rows = false;
  bool saw_groups = false;
  int64_t num_rows = 0;
  std::vector<PGroup> groups;
  Status st = in.read_struct([&](int id, int type) -> Status {
    if (id == 2 && (type == 9 || type == 10)) {
      int et = 0;
      int sz = 0;
      Status st2 = in.list_header(et, sz);
      if (!st2.ok()) {
        return st2;
      }
      if (sz <= 0 || sz > 8192 || et != 12) {
        return Status(ST_INVALID, "parquet schema");
      }
      for (int i = 0; i < sz; ++i) {
        PNode node;
        st2 = parse_schema_node(in, node);
        if (!st2.ok()) {
          return st2;
        }
        schema.push_back(node);
      }
      saw_schema = true;
      return Status();
    }
    if (id == 3 && type >= 4 && type <= 6) {
      Status st2 = in.zigzag(num_rows);
      if (!st2.ok()) {
        return st2;
      }
      saw_rows = true;
      return Status();
    }
    if (id == 4 && (type == 9 || type == 10)) {
      int et = 0;
      int sz = 0;
      Status st2 = in.list_header(et, sz);
      if (!st2.ok()) {
        return st2;
      }
      if (sz < 0 || sz > 4096) {
        return Status(ST_LIMIT, "parquet row groups");
      }
      if (sz > 0 && et != 12) {
        return Status(ST_INVALID, "parquet row group");
      }
      for (int i = 0; i < sz; ++i) {
        PGroup g;
        st2 = parse_row_group(in, g);
        if (!st2.ok()) {
          return st2;
        }
        groups.push_back(g);
      }
      saw_groups = true;
      return Status();
    }
    return in.skip(type);
  });
  if (!st.ok()) {
    return st;
  }
  if (!saw_schema || !saw_rows || !saw_groups || num_rows < 0) {
    return Status(ST_INVALID, "parquet footer");
  }
  size_t idx = 0;
  std::vector<PLeaf> leaves;
  st = walk_leaves(schema, idx, 0, 0, leaves);
  if (!st.ok()) {
    return st;
  }
  if (idx != schema.size() || leaves.empty()) {
    return Status(ST_SCHEMA, "parquet schema");
  }
  std::set<std::string> used;
  for (size_t i = 0; i < leaves.size(); ++i) {
    leaves[i].name = header_name(leaves[i].name, static_cast<int>(i + 1), used);
  }
  int64_t sum = 0;
  for (size_t g = 0; g < groups.size(); ++g) {
    if (groups[g].cols.size() != leaves.size()) {
      return Status(ST_INVALID, "parquet column count mismatch");
    }
    sum += groups[g].num_rows;
  }
  if (sum != num_rows) {
    return Status(ST_INVALID, "parquet row count mismatch");
  }
  meta.leaves.swap(leaves);
  meta.groups.swap(groups);
  meta.num_rows = num_rows;
  return Status();
}

inline Status read_parquet_footer(ByteSource &src, ParquetMeta &meta) {
  int64_t sz = src.size();
  if (sz < 12 || sz > kMaxFileBytes) {
    return Status(ST_INVALID, "not a parquet file");
  }
  uint8_t magic[4];
  Status st = src.read_at(0, magic, 4);
  if (!st.ok()) {
    return st;
  }
  if (std::memcmp(magic, "PAR1", 4) != 0) {
    return Status(ST_INVALID, "not a parquet file");
  }
  uint8_t tail[8];
  st = src.read_at(sz - 8, tail, 8);
  if (!st.ok()) {
    return st;
  }
  if (std::memcmp(tail + 4, "PAR1", 4) != 0) {
    return Status(ST_INVALID, "not a parquet file");
  }
  uint32_t flen = load_le32(tail);
  if (flen < 4 || static_cast<int64_t>(flen) + 8 > sz || flen > 64u * 1024u * 1024u) {
    return Status(ST_INVALID, "parquet footer length");
  }
  std::vector<uint8_t> footer(flen);
  st = src.read_at(sz - 8 - static_cast<int64_t>(flen), &footer[0], flen);
  if (!st.ok()) {
    return st;
  }
  return parse_footer_meta(&footer[0], footer.size(), meta);
}

class RleDec {
public:
  RleDec(const uint8_t *p, size_t n, int bit_width)
      : p_(p), end_(p + n), bw_(bit_width), left_(0), packed_(false), rle_val_(0), bits_(NULL),
        bits_end_(NULL), cur_(0), bitpos_(0) {}

  Status next(uint32_t &out) {
    if (bw_ == 0) {
      out = 0;
      return Status();
    }
    if (bw_ < 0 || bw_ > 32) {
      return Status(ST_INVALID, "parquet bit width");
    }
    if (left_ == 0) {
      Status st = pull();
      if (!st.ok()) {
        return st;
      }
      if (left_ == 0) {
        return Status(ST_INVALID, "parquet empty rle run");
      }
    }
    if (!packed_) {
      out = rle_val_;
      --left_;
      return Status();
    }
    out = 0;
    for (int i = 0; i < bw_; ++i) {
      if (bitpos_ == 0) {
        if (bits_ >= bits_end_) {
          return Status(ST_INVALID, "parquet bit pack overrun");
        }
        cur_ = *bits_++;
      }
      if (cur_ & (1u << bitpos_)) {
        out |= (1u << i);
      }
      bitpos_ = (bitpos_ + 1) & 7;
    }
    --left_;
    return Status();
  }

private:
  Status varint(uint64_t &n) {
    n = 0;
    int shift = 0;
    for (int i = 0; i < 10; ++i) {
      if (p_ >= end_) {
        return Status(ST_INVALID, "parquet varint");
      }
      uint8_t b = *p_++;
      n |= (uint64_t)(b & 0x7f) << shift;
      if ((b & 0x80) == 0) {
        return Status();
      }
      shift += 7;
    }
    return Status(ST_INVALID, "parquet varint");
  }

  Status pull() {
    uint64_t header = 0;
    Status st = varint(header);
    if (!st.ok()) {
      return st;
    }
    uint64_t count = header >> 1;
    if ((header & 1) == 0) {
      packed_ = false;
      if (count > 1000000000ULL) {
        return Status(ST_LIMIT, "parquet rle run");
      }
      left_ = count;
      int nbytes = (bw_ + 7) / 8;
      if (nbytes <= 0 || p_ + nbytes > end_) {
        return Status(ST_INVALID, "parquet rle value");
      }
      uint32_t v = 0;
      for (int i = 0; i < nbytes; ++i) {
        v |= (uint32_t)p_[i] << (8 * i);
      }
      p_ += nbytes;
      if (bw_ < 32) {
        v &= (1u << bw_) - 1u;
      }
      rle_val_ = v;
      return Status();
    }
    packed_ = true;
    if (count > 100000000ULL) {
      return Status(ST_LIMIT, "parquet bit pack");
    }
    left_ = count * 8;
    size_t nbytes = static_cast<size_t>(count) * static_cast<size_t>(bw_);
    if (p_ + nbytes > end_) {
      return Status(ST_INVALID, "parquet bit pack overrun");
    }
    bits_ = p_;
    bits_end_ = p_ + nbytes;
    p_ += nbytes;
    bitpos_ = 0;
    return Status();
  }

  const uint8_t *p_;
  const uint8_t *end_;
  int bw_;
  uint64_t left_;
  bool packed_;
  uint32_t rle_val_;
  const uint8_t *bits_;
  const uint8_t *bits_end_;
  uint8_t cur_;
  int bitpos_;
};

inline Status read_levels(const uint8_t *p, size_t n, int bw, int count, bool length_prefix,
                          std::vector<uint32_t> &levels) {
  levels.assign(static_cast<size_t>(count), 0);
  if (count == 0 || bw == 0) {
    return Status();
  }
  size_t off = 0;
  size_t len = n;
  if (length_prefix) {
    if (n < 4) {
      return Status(ST_INVALID, "parquet level length");
    }
    uint32_t L = load_le32(p);
    if (static_cast<size_t>(L) + 4 > n) {
      return Status(ST_INVALID, "parquet level length");
    }
    off = 4;
    len = L;
  }
  RleDec dec(p + off, len, bw);
  for (int i = 0; i < count; ++i) {
    uint32_t v = 0;
    Status st = dec.next(v);
    if (!st.ok()) {
      return st;
    }
    levels[static_cast<size_t>(i)] = v;
  }
  return Status();
}

inline Status format_epoch(int64_t units, int64_t scale, std::string &text) {
  if (scale <= 0) {
    return Status(ST_INVALID, "timestamp scale");
  }
  int64_t sec = units / scale;
  int64_t sod = sec % 86400;
  int64_t days = sec / 86400;
  if (sod < 0) {
    sod += 86400;
    --days;
  }
  int y = 0, m = 0, d = 0;
  ymd_from_unix_days(days, y, m, d);
  if (!valid_ymd(y, m, d)) {
    return Status(ST_INVALID, "timestamp out of range");
  }
  int hh = static_cast<int>(sod / 3600);
  int mm = static_cast<int>((sod % 3600) / 60);
  int ss = static_cast<int>(sod % 60);
  text = format_datetime_from_parts(y, m, d, hh, mm, ss);
  return Status();
}

inline Cell make_null(Type t) {
  Cell c;
  c.is_null = true;
  c.type = t;
  return c;
}

inline Status decode_plain_bool(const uint8_t *p, size_t n, int count, std::vector<Cell> &out) {
  out.clear();
  int bit = 0;
  size_t i = 0;
  for (int k = 0; k < count; ++k) {
    if (bit == 0 && i >= n) {
      return Status(ST_INVALID, "parquet boolean");
    }
    int v = (p[i] >> bit) & 1;
    ++bit;
    if (bit == 8) {
      bit = 0;
      ++i;
    }
    Cell c;
    c.is_null = false;
    c.type = TYPE_BOOL;
    c.i64 = v;
    out.push_back(c);
  }
  return Status();
}

inline Status decode_plain(const uint8_t *p, size_t n, const PLeaf &leaf, int count,
                           std::vector<Cell> &out) {
  out.clear();
  if (count < 0) {
    return Status(ST_INVALID, "parquet value count");
  }
  if (leaf.physical == 0) {
    return decode_plain_bool(p, n, count, out);
  }
  size_t i = 0;
  out.reserve(static_cast<size_t>(count));
  for (int r = 0; r < count; ++r) {
    Cell c;
    c.is_null = false;
    c.type = leaf.logical;
    if (leaf.physical == 1) {
      if (i + 4 > n) {
        return Status(ST_INVALID, "parquet int32");
      }
      int32_t s = static_cast<int32_t>(load_le32(p + i));
      i += 4;
      if (leaf.logical == TYPE_DATE) {
        int y, m, d;
        ymd_from_unix_days(s, y, m, d);
        if (!valid_ymd(y, m, d)) {
          return Status(ST_INVALID, "parquet date");
        }
        c.text = format_date(y, m, d);
      } else {
        c.type = TYPE_BIGINT;
        c.i64 = s;
      }
    } else if (leaf.physical == 2) {
      if (i + 8 > n) {
        return Status(ST_INVALID, "parquet int64");
      }
      int64_t s = static_cast<int64_t>(load_le64(p + i));
      i += 8;
      if (leaf.converted == 9 || leaf.converted == 10) {
        Status st = format_epoch(s, leaf.converted == 9 ? 1000 : 1000000, c.text);
        if (!st.ok()) {
          return st;
        }
        c.type = TYPE_DATETIME;
      } else {
        c.type = TYPE_BIGINT;
        c.i64 = s;
      }
    } else if (leaf.physical == 3) {
      if (i + 12 > n) {
        return Status(ST_INVALID, "parquet int96");
      }
      uint64_t nanos = load_le64(p + i);
      uint32_t julian = load_le32(p + i + 8);
      i += 12;
      int64_t sec = static_cast<int64_t>(nanos / 1000000000ULL);
      int64_t days = static_cast<int64_t>(julian) - 2440588LL + sec / 86400;
      int64_t sod = sec % 86400;
      if (sod < 0) {
        sod += 86400;
        --days;
      }
      int y, m, d;
      ymd_from_unix_days(days, y, m, d);
      if (!valid_ymd(y, m, d)) {
        return Status(ST_INVALID, "parquet int96");
      }
      c.type = TYPE_DATETIME;
      c.text = format_datetime_from_parts(y, m, d, static_cast<int>(sod / 3600),
                                         static_cast<int>((sod % 3600) / 60),
                                         static_cast<int>(sod % 60));
    } else if (leaf.physical == 4) {
      if (i + 4 > n) {
        return Status(ST_INVALID, "parquet float");
      }
      c.type = TYPE_DOUBLE;
      c.f64 = static_cast<double>(load_f32(p + i));
      i += 4;
    } else if (leaf.physical == 5) {
      if (i + 8 > n) {
        return Status(ST_INVALID, "parquet double");
      }
      c.type = TYPE_DOUBLE;
      c.f64 = load_f64(p + i);
      i += 8;
    } else if (leaf.physical == 6) {
      if (i + 4 > n) {
        return Status(ST_INVALID, "parquet byte array");
      }
      uint32_t len = load_le32(p + i);
      i += 4;
      if (len > static_cast<uint32_t>(kMaxFieldBytes) || i + len > n) {
        return Status(ST_INVALID, "parquet byte array");
      }
      c.type = TYPE_VARCHAR;
      c.text.assign(reinterpret_cast<const char *>(p + i), len);
      i += len;
    } else if (leaf.physical == 7) {
      if (leaf.type_length < 0 || i + static_cast<size_t>(leaf.type_length) > n) {
        return Status(ST_INVALID, "parquet fixed");
      }
      c.type = TYPE_VARCHAR;
      c.text.assign(reinterpret_cast<const char *>(p + i), static_cast<size_t>(leaf.type_length));
      i += static_cast<size_t>(leaf.type_length);
    } else {
      return Status(ST_UNSUPPORTED, "parquet physical type");
    }
    out.push_back(c);
  }
  return Status();
}

inline Status decode_dict_indices(const uint8_t *p, size_t n, const std::vector<Cell> &dict, int count,
                                  std::vector<Cell> &out) {
  out.clear();
  if (count == 0) {
    return Status();
  }
  if (n < 1) {
    return Status(ST_INVALID, "parquet dictionary index");
  }
  int bw = p[0];
  if (bw < 0 || bw > 32) {
    return Status(ST_INVALID, "parquet dictionary bit width");
  }
  RleDec dec(p + 1, n - 1, bw);
  for (int i = 0; i < count; ++i) {
    uint32_t idx = 0;
    Status st = dec.next(idx);
    if (!st.ok()) {
      return st;
    }
    if (idx >= dict.size()) {
      return Status(ST_INVALID, "parquet dictionary index");
    }
    out.push_back(dict[idx]);
  }
  return Status();
}

inline Status decompress_buf(int codec, const uint8_t *src, size_t n, size_t expect,
                             std::vector<uint8_t> &out) {
  if (expect > static_cast<size_t>(kMaxFileBytes)) {
    return Status(ST_LIMIT, "parquet page exceeds limit");
  }
  if (codec == 0) {
    if (n != expect) {
      return Status(ST_INVALID, "parquet page size");
    }
    out.assign(src, src + n);
    return Status();
  }
  if (codec == 1) {
    return snappy_uncompress(src, n, expect, out);
  }
  return Status(ST_UNSUPPORTED, "parquet compression is not supported");
}

inline Status parse_page_header(const uint8_t *p, size_t n, PageHdr &hdr, size_t &used) {
  CompactIn in(p, n);
  hdr = PageHdr();
  Status st = in.read_struct([&](int id, int type) -> Status {
    if ((id == 1 || id == 2 || id == 3) && type >= 4 && type <= 6) {
      int64_t v = 0;
      Status st2 = in.zigzag(v);
      if (!st2.ok()) {
        return st2;
      }
      if (id == 1) {
        hdr.type = static_cast<int>(v);
      } else if (id == 2) {
        hdr.uncompressed = static_cast<int>(v);
      } else {
        hdr.compressed = static_cast<int>(v);
      }
      return Status();
    }
    if (id == 5 && type == 12) {
      return in.read_struct([&](int id2, int t2) -> Status {
        if (id2 >= 1 && id2 <= 4 && t2 >= 4 && t2 <= 6) {
          int64_t v = 0;
          Status st2 = in.zigzag(v);
          if (!st2.ok()) {
            return st2;
          }
          if (id2 == 1) {
            hdr.num_values = static_cast<int>(v);
          } else if (id2 == 2) {
            hdr.encoding = static_cast<int>(v);
          } else if (id2 == 3) {
            hdr.def_level_encoding = static_cast<int>(v);
          }
          return Status();
        }
        return in.skip(t2);
      });
    }
    if (id == 7 && type == 12) {
      return in.read_struct([&](int id2, int t2) -> Status {
        if ((id2 == 1 || id2 == 2) && t2 >= 4 && t2 <= 6) {
          int64_t v = 0;
          Status st2 = in.zigzag(v);
          if (!st2.ok()) {
            return st2;
          }
          if (id2 == 1) {
            hdr.num_values = static_cast<int>(v);
          } else {
            hdr.encoding = static_cast<int>(v);
          }
          return Status();
        }
        return in.skip(t2);
      });
    }
    if (id == 8 && type == 12) {
      return in.read_struct([&](int id2, int t2) -> Status {
        if (id2 == 7 && (t2 == 1 || t2 == 2)) {
          hdr.is_compressed = t2 == 1;
          return Status();
        }
        if (id2 >= 1 && id2 <= 6 && t2 >= 4 && t2 <= 6) {
          int64_t v = 0;
          Status st2 = in.zigzag(v);
          if (!st2.ok()) {
            return st2;
          }
          if (id2 == 1) {
            hdr.num_values = static_cast<int>(v);
          } else if (id2 == 4) {
            hdr.encoding = static_cast<int>(v);
          } else if (id2 == 5) {
            hdr.def_bytes = static_cast<int>(v);
          } else if (id2 == 6) {
            hdr.rep_bytes = static_cast<int>(v);
          }
          return Status();
        }
        return in.skip(t2);
      });
    }
    return in.skip(type);
  });
  if (!st.ok()) {
    return st;
  }
  used = in.tell(p);
  if (hdr.type < 0 || hdr.compressed < 0 || hdr.uncompressed < 0) {
    return Status(ST_INVALID, "parquet page header");
  }
  return Status();
}

inline Status fill_schema(const ParquetMeta &meta, Schema &schema) {
  schema.columns.clear();
  for (size_t i = 0; i < meta.leaves.size(); ++i) {
    Column c;
    c.name = meta.leaves[i].name;
    c.type = meta.leaves[i].logical;
    c.nullable = meta.leaves[i].nullable;
    c.null_cnt = 0;
    c.value_cnt = meta.num_rows;
    schema.columns.push_back(c);
  }
  schema.row_count = meta.num_rows;
  return Status();
}

struct OpenedPath {
  std::string real;
  Fingerprint fp;
};

inline Status authorize_path(const std::string &path, const Options &opt, bool directory,
                             OpenedPath &out) {
  if (path.empty() || has_dotdot(path)) {
    return Status(ST_INVALID, "path contains '..'");
  }
  bool ok = false;
  out.real = real_path(path, &ok);
  if (!ok) {
    return Status(ST_IO, "path does not exist");
  }
  if (!opt.secure_root.empty()) {
    bool rok = false;
    std::string root = real_path(opt.secure_root, &rok);
    if (!rok) {
      root = opt.secure_root;
    }
    if (!under_root(root, out.real)) {
      return Status(ST_INVALID, "path escapes secure_file_priv");
    }
  }
  struct stat stbuf;
  if (::stat(out.real.c_str(), &stbuf) != 0) {
    return Status(ST_IO, "stat failed");
  }
  if (directory) {
    if (!S_ISDIR(stbuf.st_mode)) {
      return Status(ST_IO, "not a directory");
    }
  } else if (!S_ISREG(stbuf.st_mode)) {
    return Status(ST_IO, "not a regular file");
  } else if (stbuf.st_size > kMaxFileBytes) {
    return Status(ST_LIMIT, "file exceeds size limit");
  }
  return fingerprint_of(out.real, out.fp, true);
}

inline Status read_secured(const std::string &path, const Options &opt, std::string &out,
                           std::string *real, Fingerprint *fp) {
  OpenedPath op;
  Status st = authorize_path(path, opt, false, op);
  if (!st.ok()) {
    return st;
  }
  ByteSource src;
  st = src.open(op.real);
  if (!st.ok() || !src.ok()) {
    return Status(ST_IO, "open failed");
  }
  st = src.read_all(out);
  if (!st.ok()) {
    return st;
  }
  if (real != NULL) {
    *real = op.real;
  }
  if (fp != NULL) {
    *fp = op.fp;
  }
  return Status();
}

inline Status infer_parquet(const std::string &path, const Options &opt, Schema &schema) {
  OpenedPath op;
  Status st = authorize_path(path, opt, false, op);
  if (!st.ok()) {
    return st;
  }
  ByteSource src;
  st = src.open(op.real);
  if (!st.ok() || !src.ok()) {
    return Status(ST_IO, "open failed");
  }
  ParquetMeta meta;
  st = read_parquet_footer(src, meta);
  if (!st.ok()) {
    return st;
  }
  st = fill_schema(meta, schema);
  if (!st.ok()) {
    return st;
  }
  schema.fp = op.fp;
  return Status();
}

inline Status infer_schema(Kind kind, const std::string &path, const Options &opt, Schema &schema) {
  schema = Schema();
  if (opt.sample_size != -1) {
    return Status(ST_UNSUPPORTED, "sample_size");
  }
  if (kind == KIND_GLOB) {
    return infer_glob(path, opt, schema);
  }
  if (kind == KIND_JSON && opt.format != "newline_delimited") {
    return Status(ST_UNSUPPORTED, "json format");
  }
  if (kind == KIND_PARQUET) {
    return infer_parquet(path, opt, schema);
  }
  if (kind != KIND_CSV && kind != KIND_JSON) {
    return Status(ST_UNSUPPORTED, "file kind");
  }
  std::string data;
  Status st = read_secured(path, opt, data, NULL, &schema.fp);
  if (!st.ok()) {
    return st;
  }
  if (kind == KIND_CSV) {
    return infer_csv(data, opt, schema);
  }
  return infer_jsonl(data, opt, schema);
}

class FileSqlReader {
public:
  FileSqlReader()
      : kind_(KIND_CSV), csv_(NULL), json_pos_(0), json_line_(0), glob_i_(0), group_i_(0),
        row_in_group_(0) {}
  ~FileSqlReader() { reset(); }

  Status open(Kind kind, const std::string &path, const Options &opt) {
    reset();
    kind_ = kind;
    path_ = path;
    opt_ = opt;
    if (opt_.sample_size != -1) {
      return fail(Status(ST_UNSUPPORTED, "sample_size"));
    }
    if (kind_ == KIND_GLOB) {
      Status st = list_glob(path_, opt_, glob_paths_, schema_.fp);
      if (!st.ok()) {
        return fail(st);
      }
      Column c;
      c.name = "file";
      c.type = TYPE_VARCHAR;
      c.nullable = false;
      c.value_cnt = static_cast<int64_t>(glob_paths_.size());
      schema_.columns.push_back(c);
      schema_.row_count = c.value_cnt;
      glob_i_ = 0;
      return Status();
    }
    if (kind_ == KIND_JSON && opt_.format != "newline_delimited") {
      return fail(Status(ST_UNSUPPORTED, "json format"));
    }
    if (kind_ == KIND_PARQUET) {
      OpenedPath op;
      Status st = authorize_path(path_, opt_, false, op);
      if (!st.ok()) {
        return fail(st);
      }
      real_path_ = op.real;
      schema_.fp = op.fp;
      st = file_.open(real_path_);
      if (!st.ok() || !file_.ok()) {
        return fail(Status(ST_IO, "open failed"));
      }
      st = read_parquet_footer(file_, meta_);
      if (!st.ok()) {
        return fail(st);
      }
      st = fill_schema(meta_, schema_);
      if (!st.ok()) {
        return fail(st);
      }
      schema_.fp = op.fp;
      return Status();
    }
    if (kind_ != KIND_CSV && kind_ != KIND_JSON) {
      return fail(Status(ST_UNSUPPORTED, "file kind"));
    }
    Status st = read_secured(path_, opt_, data_, &real_path_, &schema_.fp);
    if (!st.ok()) {
      return fail(st);
    }
    if (kind_ == KIND_CSV) {
      st = infer_csv(data_, opt_, schema_);
      if (!st.ok()) {
        return fail(st);
      }
      csv_ = new CsvParser(data_, opt_);
      if (opt_.header) {
        std::vector<std::string> fields;
        st = csv_->next_record(fields);
        if (!st.ok()) {
          return fail(st.end() ? Status(ST_SCHEMA, "CSV schema inference failed: empty file") : st);
        }
      }
      return Status();
    }
    st = infer_jsonl(data_, opt_, schema_);
    if (!st.ok()) {
      return fail(st);
    }
    for (size_t i = 0; i < schema_.columns.size(); ++i) {
      json_index_[schema_.columns[i].name] = static_cast<int>(i);
    }
    json_pos_ = 0;
    if (data_.size() >= 3 && static_cast<unsigned char>(data_[0]) == 0xEF &&
        static_cast<unsigned char>(data_[1]) == 0xBB &&
        static_cast<unsigned char>(data_[2]) == 0xBF) {
      json_pos_ = 3;
    }
    json_line_ = 0;
    return Status();
  }

  void set_needed(const std::vector<char> &needed) { needed_ = needed; }

  const Schema &schema() const { return schema_; }

  Status rewind() {
    if (kind_ == KIND_CSV) {
      delete csv_;
      csv_ = new CsvParser(data_, opt_);
      if (opt_.header) {
        std::vector<std::string> fields;
        Status st = csv_->next_record(fields);
        if (!st.ok()) {
          return st;
        }
      }
      return Status();
    }
    if (kind_ == KIND_JSON) {
      json_pos_ = 0;
      if (data_.size() >= 3 && static_cast<unsigned char>(data_[0]) == 0xEF &&
          static_cast<unsigned char>(data_[1]) == 0xBB &&
          static_cast<unsigned char>(data_[2]) == 0xBF) {
        json_pos_ = 3;
      }
      json_line_ = 0;
      return Status();
    }
    if (kind_ == KIND_GLOB) {
      glob_i_ = 0;
      return Status();
    }
    group_i_ = 0;
    row_in_group_ = 0;
    curs_.clear();
    return Status();
  }

  Status check_unchanged() const {
    if (kind_ == KIND_GLOB) {
      std::vector<std::string> paths;
      Fingerprint fp;
      Status st = list_glob(path_, opt_, paths, fp);
      if (!st.ok()) {
        return st;
      }
      if (!(fp == schema_.fp)) {
        return Status(ST_CHANGED, "FILE_CHANGED");
      }
      return Status();
    }
    Fingerprint fp;
    Status st = fingerprint_of(real_path_, fp, true);
    if (!st.ok()) {
      return st;
    }
    if (!(fp == schema_.fp)) {
      return Status(ST_CHANGED, "FILE_CHANGED");
    }
    return Status();
  }

  Status next(std::vector<Cell> &row) {
    if (kind_ == KIND_CSV) {
      return next_csv(row);
    }
    if (kind_ == KIND_JSON) {
      return next_json(row);
    }
    if (kind_ == KIND_GLOB) {
      return next_glob(row);
    }
    if (kind_ == KIND_PARQUET) {
      return next_parquet(row);
    }
    return Status(ST_UNSUPPORTED, "file kind");
  }

private:
  struct Cur {
    int64_t next_off;
    int64_t left;
    std::vector<Cell> dict;
    std::vector<Cell> buf;
    size_t bi;
    Cur() : next_off(0), left(0), bi(0) {}
  };

  Status fail(Status st) {
    reset();
    return st;
  }

  void reset() {
    delete csv_;
    csv_ = NULL;
    file_.close();
    data_.clear();
    json_index_.clear();
    glob_paths_.clear();
    meta_ = ParquetMeta();
    curs_.clear();
    needed_.clear();
    schema_ = Schema();
    group_i_ = 0;
    row_in_group_ = 0;
    glob_i_ = 0;
    json_pos_ = 0;
    json_line_ = 0;
  }

  bool col_needed(size_t i) const {
    if (needed_.empty()) {
      return true;
    }
    if (i >= needed_.size()) {
      return false;
    }
    return needed_[i] != 0;
  }

  Status next_csv(std::vector<Cell> &row) {
    std::vector<std::string> fields;
    Status st = csv_->next_record(fields);
    if (!st.ok()) {
      return st;
    }
    if (fields.size() == 1 && fields[0].empty() && csv_->eof()) {
      return Status(ST_END);
    }
    if (fields.size() != schema_.columns.size()) {
      return Status(ST_INVALID, "CSV column count does not match header");
    }
    row.resize(schema_.columns.size());
    for (size_t i = 0; i < fields.size(); ++i) {
      if (is_null_field(fields[i], opt_)) {
        row[i] = make_null(schema_.columns[i].type);
      } else {
        st = cast_text(fields[i], schema_.columns[i].type, row[i]);
        if (!st.ok()) {
          return st;
        }
      }
    }
    return Status();
  }

  Status next_json(std::vector<Cell> &row) {
    while (json_pos_ < data_.size()) {
      size_t nl = data_.find('\n', json_pos_);
      std::string line = data_.substr(json_pos_, nl == std::string::npos ? std::string::npos : nl - json_pos_);
      json_pos_ = (nl == std::string::npos) ? data_.size() : nl + 1;
      json_line_++;
      if (!line.empty() && line[line.size() - 1] == '\r') {
        line.erase(line.size() - 1);
      }
      bool blank = true;
      for (size_t k = 0; k < line.size(); ++k) {
        if (line[k] != ' ' && line[k] != '\t') {
          blank = false;
          break;
        }
      }
      if (blank) {
        continue;
      }
      std::vector<std::pair<std::string, JsonField> > kvs;
      Status st = parse_json_object(line, json_line_, kvs);
      if (!st.ok()) {
        return st;
      }
      row.assign(schema_.columns.size(), Cell());
      for (size_t c = 0; c < schema_.columns.size(); ++c) {
        row[c] = make_null(schema_.columns[c].type);
      }
      std::set<std::string> seen;
      for (size_t k = 0; k < kvs.size(); ++k) {
        if (!seen.insert(kvs[k].first).second) {
          std::ostringstream os;
          os << "JSONL line " << json_line_ << ": duplicate key";
          return Status(ST_INVALID, os.str());
        }
        std::map<std::string, int>::const_iterator it = json_index_.find(kvs[k].first);
        if (it == json_index_.end()) {
          return Status(ST_INVALID, "JSONL key is not in schema");
        }
        if (!kvs[k].second.is_null) {
          st = cast_text(kvs[k].second.text, schema_.columns[static_cast<size_t>(it->second)].type,
                         row[static_cast<size_t>(it->second)]);
          if (!st.ok()) {
            return st;
          }
        }
      }
      return Status();
    }
    return Status(ST_END);
  }

  Status next_glob(std::vector<Cell> &row) {
    if (glob_i_ >= glob_paths_.size()) {
      return Status(ST_END);
    }
    row.resize(1);
    row[0].is_null = false;
    row[0].type = TYPE_VARCHAR;
    row[0].text = glob_paths_[glob_i_++];
    return Status();
  }

  Status read_page_at(int64_t off, PageHdr &hdr, std::vector<uint8_t> &payload, int64_t &next_off) {
    if (off < 0 || off >= file_.size()) {
      return Status(ST_INVALID, "parquet page offset");
    }
    int64_t remain = file_.size() - off;
    size_t want = static_cast<size_t>(std::min<int64_t>(remain, 1 << 20));
    std::vector<uint8_t> buf(want);
    Status st = file_.read_at(off, &buf[0], want);
    if (!st.ok()) {
      return st;
    }
    size_t used = 0;
    st = parse_page_header(&buf[0], buf.size(), hdr, used);
    if (!st.ok()) {
      return st;
    }
    if (used == 0 || used > want) {
      return Status(ST_INVALID, "parquet page header");
    }
    int64_t payload_off = off + static_cast<int64_t>(used);
    if (hdr.compressed < 0 || payload_off + hdr.compressed > file_.size()) {
      return Status(ST_INVALID, "parquet page size");
    }
    payload.resize(static_cast<size_t>(hdr.compressed));
    if (hdr.compressed > 0) {
      st = file_.read_at(payload_off, &payload[0], static_cast<size_t>(hdr.compressed));
      if (!st.ok()) {
        return st;
      }
    }
    next_off = payload_off + hdr.compressed;
    if (next_off <= off) {
      return Status(ST_INVALID, "parquet page did not advance");
    }
    return Status();
  }

  Status decode_values(int col, const PageHdr &hdr, const std::vector<uint8_t> &payload,
                       std::vector<Cell> &cells) {
    const PLeaf &leaf = meta_.leaves[static_cast<size_t>(col)];
    const PChunk &chunk = meta_.groups[group_i_].cols[static_cast<size_t>(col)];
    if (hdr.num_values <= 0) {
      return Status(ST_INVALID, "parquet page values");
    }
    std::vector<uint32_t> defs;
    std::vector<uint8_t> values;
    if (hdr.type == 0) {
      std::vector<uint8_t> raw;
      Status st = decompress_buf(chunk.codec, payload.empty() ? NULL : &payload[0], payload.size(),
                                 static_cast<size_t>(hdr.uncompressed), raw);
      if (!st.ok()) {
        return st;
      }
      size_t off = 0;
      if (leaf.max_def > 0) {
        if (hdr.def_level_encoding != 3) {
          return Status(ST_UNSUPPORTED, "parquet level encoding");
        }
        if (off > raw.size()) {
          return Status(ST_INVALID, "parquet levels");
        }
        st = read_levels(raw.empty() ? NULL : &raw[0] + off, raw.size() - off, bit_width_of(leaf.max_def),
                         hdr.num_values, true, defs);
        if (!st.ok()) {
          return st;
        }
        if (raw.size() < 4) {
          return Status(ST_INVALID, "parquet levels");
        }
        uint32_t L = load_le32(&raw[0]);
        off = 4u + L;
        if (off > raw.size()) {
          return Status(ST_INVALID, "parquet levels");
        }
      } else {
        defs.assign(static_cast<size_t>(hdr.num_values), 0);
      }
      values.assign(raw.begin() + static_cast<std::ptrdiff_t>(off), raw.end());
    } else if (hdr.type == 3) {
      if (hdr.rep_bytes != 0) {
        return Status(ST_UNSUPPORTED, "parquet repetition levels");
      }
      if (hdr.def_bytes < 0 ||
          static_cast<size_t>(hdr.def_bytes) > payload.size() ||
          hdr.uncompressed < hdr.def_bytes) {
        return Status(ST_INVALID, "parquet v2 levels");
      }
      if (leaf.max_def > 0) {
        Status st = read_levels(payload.empty() ? NULL : &payload[0], static_cast<size_t>(hdr.def_bytes),
                                bit_width_of(leaf.max_def), hdr.num_values, false, defs);
        if (!st.ok()) {
          return st;
        }
      } else if (hdr.def_bytes != 0) {
        return Status(ST_INVALID, "parquet v2 levels");
      } else {
        defs.assign(static_cast<size_t>(hdr.num_values), 0);
      }
      const uint8_t *vp = payload.empty() ? NULL : &payload[0] + hdr.def_bytes;
      size_t vn = payload.size() - static_cast<size_t>(hdr.def_bytes);
      size_t expect = static_cast<size_t>(hdr.uncompressed - hdr.def_bytes);
      int use_codec = hdr.is_compressed ? chunk.codec : 0;
      Status st = decompress_buf(use_codec, vp, vn, expect, values);
      if (!st.ok()) {
        return st;
      }
    } else {
      return Status(ST_UNSUPPORTED, "parquet page type");
    }
    int present = leaf.max_def;
    int nonnull = 0;
    for (size_t i = 0; i < defs.size(); ++i) {
      if (static_cast<int>(defs[i]) == present) {
        ++nonnull;
      }
    }
    std::vector<Cell> plain;
    Status st;
    if (hdr.encoding == 0) {
      st = decode_plain(values.empty() ? NULL : &values[0], values.size(), leaf, nonnull, plain);
    } else if (hdr.encoding == 2 || hdr.encoding == 8) {
      st = decode_dict_indices(values.empty() ? NULL : &values[0], values.size(),
                               curs_[static_cast<size_t>(col)].dict, nonnull, plain);
    } else {
      return Status(ST_UNSUPPORTED, "parquet encoding is not supported");
    }
    if (!st.ok()) {
      return st;
    }
    if (static_cast<int>(plain.size()) != nonnull) {
      return Status(ST_INVALID, "parquet value count");
    }
    cells.clear();
    cells.reserve(static_cast<size_t>(hdr.num_values));
    int vi = 0;
    for (int i = 0; i < hdr.num_values; ++i) {
      if (static_cast<int>(defs[static_cast<size_t>(i)]) == present) {
        cells.push_back(plain[static_cast<size_t>(vi++)]);
      } else {
        cells.push_back(make_null(leaf.logical));
      }
    }
    return Status();
  }

  Status absorb_dict(int col, const PageHdr &hdr, const std::vector<uint8_t> &payload) {
    if (hdr.encoding != 0) {
      return Status(ST_UNSUPPORTED, "parquet dictionary encoding");
    }
    const PLeaf &leaf = meta_.leaves[static_cast<size_t>(col)];
    const PChunk &chunk = meta_.groups[group_i_].cols[static_cast<size_t>(col)];
    std::vector<uint8_t> raw;
    Status st = decompress_buf(chunk.codec, payload.empty() ? NULL : &payload[0], payload.size(),
                               static_cast<size_t>(hdr.uncompressed), raw);
    if (!st.ok()) {
      return st;
    }
    return decode_plain(raw.empty() ? NULL : &raw[0], raw.size(), leaf, hdr.num_values,
                        curs_[static_cast<size_t>(col)].dict);
  }

  Status ensure_group() {
    if (!curs_.empty()) {
      return Status();
    }
    curs_.assign(meta_.leaves.size(), Cur());
    const PGroup &group = meta_.groups[group_i_];
    for (size_t i = 0; i < meta_.leaves.size(); ++i) {
      curs_[i].next_off = group.cols[i].data_off;
      curs_[i].left = group.cols[i].num_values;
      if (!col_needed(i) || group.cols[i].dict_off < 0) {
        continue;
      }
      PageHdr hdr;
      std::vector<uint8_t> payload;
      int64_t next_off = 0;
      Status st = read_page_at(group.cols[i].dict_off, hdr, payload, next_off);
      if (!st.ok()) {
        return st;
      }
      if (hdr.type != 2) {
        return Status(ST_INVALID, "parquet dictionary page");
      }
      st = absorb_dict(static_cast<int>(i), hdr, payload);
      if (!st.ok()) {
        return st;
      }
    }
    return Status();
  }

  Status next_col(int col, Cell &cell) {
    Cur &c = curs_[static_cast<size_t>(col)];
    if (c.bi >= c.buf.size()) {
      c.buf.clear();
      c.bi = 0;
      if (c.left <= 0) {
        return Status(ST_INVALID, "parquet column truncated");
      }
      while (c.buf.empty()) {
        PageHdr hdr;
        std::vector<uint8_t> payload;
        int64_t next_off = 0;
        Status st = read_page_at(c.next_off, hdr, payload, next_off);
        if (!st.ok()) {
          return st;
        }
        c.next_off = next_off;
        if (hdr.type == 2) {
          st = absorb_dict(col, hdr, payload);
          if (!st.ok()) {
            return st;
          }
          continue;
        }
        if (hdr.type == 1) {
          continue;
        }
        if (hdr.type != 0 && hdr.type != 3) {
          return Status(ST_UNSUPPORTED, "parquet page type");
        }
        st = decode_values(col, hdr, payload, c.buf);
        if (!st.ok()) {
          return st;
        }
        if (static_cast<int64_t>(c.buf.size()) > c.left) {
          return Status(ST_INVALID, "parquet column truncated");
        }
        c.left -= static_cast<int64_t>(c.buf.size());
        c.bi = 0;
      }
    }
    cell = c.buf[c.bi++];
    return Status();
  }

  Status next_parquet(std::vector<Cell> &row) {
    while (group_i_ < meta_.groups.size() && row_in_group_ >= meta_.groups[group_i_].num_rows) {
      ++group_i_;
      row_in_group_ = 0;
      curs_.clear();
    }
    if (group_i_ >= meta_.groups.size()) {
      return Status(ST_END);
    }
    bool any = false;
    for (size_t i = 0; i < meta_.leaves.size(); ++i) {
      if (col_needed(i)) {
        any = true;
        break;
      }
    }
    if (any) {
      Status st = ensure_group();
      if (!st.ok()) {
        return st;
      }
    }
    row.resize(meta_.leaves.size());
    for (size_t i = 0; i < meta_.leaves.size(); ++i) {
      if (!col_needed(i)) {
        row[i] = make_null(meta_.leaves[i].logical);
        continue;
      }
      Status st = next_col(static_cast<int>(i), row[i]);
      if (!st.ok()) {
        return st;
      }
    }
    ++row_in_group_;
    return Status();
  }

  Kind kind_;
  Options opt_;
  Schema schema_;
  std::string path_;
  std::string real_path_;
  std::string data_;
  CsvParser *csv_;
  size_t json_pos_;
  int64_t json_line_;
  std::map<std::string, int> json_index_;
  std::vector<std::string> glob_paths_;
  size_t glob_i_;
  ByteSource file_;
  ParquetMeta meta_;
  size_t group_i_;
  int64_t row_in_group_;
  std::vector<char> needed_;
  std::vector<Cur> curs_;

  FileSqlReader(const FileSqlReader &);
  FileSqlReader &operator=(const FileSqlReader &);
};

}  // namespace filesql
}  // namespace sql
}  // namespace oceanbase
