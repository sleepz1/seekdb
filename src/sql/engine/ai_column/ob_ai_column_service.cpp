/*
 * Copyright (c) 2025 OceanBase.
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

#define USING_LOG_PREFIX SQL_ENG

#include "sql/engine/ai_column/ob_ai_column_service.h"

#include <cstdio>
#include <cstring>
#include <unistd.h>

#include "common/mysqlclient/ob_mysql_proxy.h"
#include "common/mysqlclient/ob_mysql_result.h"
#include "common/mysqlclient/ob_mysql_transaction.h"
#include "common/object/ob_obj_type.h"
#include "lib/allocator/page_arena.h"
#include "lib/atomic/ob_atomic.h"
#include "lib/charset/ob_charset.h"
#include "lib/oblog/ob_log_module.h"
#include "lib/string/ob_sql_string.h"
#include "lib/thread/threads.h"
#include "lib/time/ob_time_utility.h"
#include "query/ai/ob_ai_endpoint_resolver.h"
#include "share/ob_errno.h"
#include "share/ob_server_struct.h"
#include "share/rc/ob_server_runtime.h"
#include "share/schema/ob_multi_version_schema_service.h"
#include "share/schema/ob_schema_getter_guard.h"
#include "share/schema/ob_schema_struct.h"
#include "sql/das/ob_das_context.h"
#include "sql/engine/dml/ob_table_delete_op.h"
#include "sql/engine/dml/ob_table_insert_op.h"
#include "sql/engine/dml/ob_table_update_op.h"
#include "sql/engine/expr/ob_expr_ai/ob_ai_func_utils.h"
#include "sql/engine/ob_exec_context.h"
#include "sql/resolver/ddl/ob_alter_table_stmt.h"
#include "sql/session/ob_sql_session_info.h"

namespace oceanbase
{
namespace sql
{
using namespace common;
using namespace share;
using namespace share::schema;

namespace
{
constexpr int64_t AI_MAX_COLS = 8;
constexpr int64_t AI_NAME = 128;
constexpr int64_t AI_EXPR = 1500;
constexpr int64_t AI_PROMPT = 8192;

static thread_local int g_hidden_depth = 0;
static thread_local bool g_allow_hidden = false;
static thread_local bool g_in_maintenance = false;
static int g_meta_ready = 0;

struct AiTlsGuard
{
  explicit AiTlsGuard(bool maintenance)
    : prev_hidden_(g_allow_hidden), prev_maint_(g_in_maintenance)
  {
    ++g_hidden_depth;
    g_allow_hidden = true;
    if (maintenance) {
      g_in_maintenance = true;
    }
  }
  ~AiTlsGuard()
  {
    --g_hidden_depth;
    g_allow_hidden = prev_hidden_;
    g_in_maintenance = prev_maint_;
  }
  bool prev_hidden_;
  bool prev_maint_;
};

bool missing_table(int ret)
{
  return OB_TABLE_NOT_EXIST == ret || OB_ERR_UNKNOWN_TABLE == ret || OB_ERR_BAD_TABLE == ret;
}

bool ident_char(char c)
{
  return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
         (c >= '0' && c <= '9') || c == '_';
}

void copy_buf(char *dst, int64_t cap, const char *src, int64_t len)
{
  if (cap <= 0) {
    return;
  }
  if (len < 0) {
    len = 0;
  }
  if (len >= cap) {
    len = cap - 1;
  }
  if (len > 0 && src != nullptr) {
    MEMCPY(dst, src, len);
  }
  dst[len] = '\0';
}

void copy_str(char *dst, int64_t cap, const ObString &src)
{
  copy_buf(dst, cap, src.ptr(), src.length());
}

void copy_cstr(char *dst, int64_t cap, const char *src)
{
  copy_buf(dst, cap, src, src == nullptr ? 0 : static_cast<int64_t>(strlen(src)));
}

int append_quoted(ObSqlString &sql, const char *text)
{
  int ret = OB_SUCCESS;
  if (OB_FAIL(sql.append("'"))) {
  }
  for (const char *p = text; OB_SUCC(ret) && p != nullptr && *p != '\0'; ++p) {
    if (*p == '\'' || *p == '\\') {
      if (OB_FAIL(sql.append(*p == '\'' ? "''" : "\\\\"))) {
      }
    } else if (OB_FAIL(sql.append(p, 1))) {
    }
  }
  if (OB_SUCC(ret)) {
    ret = sql.append("'");
  }
  return ret;
}

int append_ident(ObSqlString &sql, const char *name)
{
  int ret = OB_SUCCESS;
  if (OB_FAIL(sql.append("`"))) {
  }
  for (const char *p = name; OB_SUCC(ret) && p != nullptr && *p != '\0'; ++p) {
    if (*p == '`') {
      if (OB_FAIL(sql.append("``"))) {
      }
    } else if (OB_FAIL(sql.append(p, 1))) {
    }
  }
  if (OB_SUCC(ret)) {
    ret = sql.append("`");
  }
  return ret;
}

bool contains_ident(const char *sql, int64_t len, const char *name)
{
  if (sql == nullptr || name == nullptr || len <= 0) {
    return false;
  }
  const int64_t nlen = static_cast<int64_t>(strlen(name));
  for (int64_t i = 0; i < len;) {
    const char c = sql[i];
    if (c == '\'' || c == '"') {
      const char quote = c;
      ++i;
      while (i < len) {
        if (sql[i] == '\\' && i + 1 < len) {
          i += 2;
        } else if (sql[i] == quote && i + 1 < len && sql[i + 1] == quote) {
          i += 2;
        } else if (sql[i] == quote) {
          ++i;
          break;
        } else {
          ++i;
        }
      }
    } else if (c == '`') {
      const int64_t start = ++i;
      while (i < len && sql[i] != '`') {
        ++i;
      }
      if (i - start == nlen && 0 == strncasecmp(sql + start, name, nlen)) {
        return true;
      }
      if (i < len) {
        ++i;
      }
    } else if (c == '#' || (c == '-' && i + 1 < len && sql[i + 1] == '-')) {
      while (i < len && sql[i] != '\n') {
        ++i;
      }
    } else if (c == '/' && i + 1 < len && sql[i + 1] == '*') {
      i += 2;
      while (i + 1 < len && !(sql[i] == '*' && sql[i + 1] == '/')) {
        ++i;
      }
      i = (i + 1 < len) ? i + 2 : len;
    } else if (ident_char(c)) {
      const int64_t start = i;
      while (i < len && ident_char(sql[i])) {
        ++i;
      }
      if (i - start == nlen && 0 == strncasecmp(sql + start, name, nlen)) {
        return true;
      }
    } else {
      ++i;
    }
  }
  return false;
}

int rewrite_status(const char *src, int64_t len, const char *state_col, ObSqlString &out)
{
  int ret = OB_SUCCESS;
  if (src == nullptr || len <= 0) {
    return ret;
  }
  for (int64_t i = 0; OB_SUCC(ret) && i < len;) {
    const char c = src[i];
    if (c == '\'' || c == '"') {
      const char quote = c;
      const int64_t start = i++;
      while (i < len) {
        if (src[i] == '\\' && i + 1 < len) {
          i += 2;
        } else if (src[i] == quote && i + 1 < len && src[i + 1] == quote) {
          i += 2;
        } else if (src[i] == quote) {
          ++i;
          break;
        } else {
          ++i;
        }
      }
      ret = out.append(src + start, i - start);
    } else if (c == '`') {
      const int64_t start = i++;
      while (i < len && src[i] != '`') {
        ++i;
      }
      if (i < len) {
        ++i;
      }
      ret = out.append(src + start, i - start);
    } else if (ident_char(c)) {
      const int64_t start = i;
      while (i < len && ident_char(src[i])) {
        ++i;
      }
      int64_t prev = start - 1;
      while (prev >= 0 && (src[prev] == ' ' || src[prev] == '\t' || src[prev] == '\n' || src[prev] == '\r')) {
        --prev;
      }
      const bool qualified = prev >= 0 && src[prev] == '.';
      if (!qualified && i - start == 6 && 0 == strncasecmp(src + start, "status", 6)) {
        if (OB_FAIL(out.append("`"))) {
        } else if (OB_FAIL(out.append(state_col))) {
        } else if (OB_FAIL(out.append("`"))) {
        }
      } else if (OB_FAIL(out.append(src + start, i - start))) {
      }
    } else if (OB_FAIL(out.append(src + i, 1))) {
      ++i;
    } else {
      ++i;
    }
  }
  return ret;
}

struct AiParsed
{
  char kind[16];
  char model[AI_NAME];
  char result_kind[16];
  char deps[512];
  char system_prompt[512];
  char user_expr[AI_EXPR];
  char normalized[2048];
  int64_t max_retries;
  AiParsed() { reset(); }
  void reset() { memset(this, 0, sizeof(*this)); }
};

bool parse_comment(const ObString &comment, AiParsed &out)
{
  out.reset();
  if (comment.length() < 6 || comment.ptr() == nullptr ||
      0 != memcmp(comment.ptr(), "AICOL1", 6)) {
    return false;
  }
  char *dest[8] = {nullptr, out.kind, out.model, nullptr, out.result_kind,
                   out.deps, out.system_prompt, out.user_expr};
  int64_t caps[8] = {16, 16, AI_NAME, 32, 16, 512, 512, AI_EXPR};
  char retries[32] = {0};
  char magic[16] = {0};
  dest[0] = magic;
  dest[3] = retries;
  const char *p = comment.ptr();
  const int64_t len = comment.length();
  int field = 0;
  int64_t start = 0;
  for (int64_t i = 0; i <= len && field < 8; ++i) {
    if (i == len || (p[i] == '\x1f' && field < 7)) {
      copy_buf(dest[field], caps[field], p + start, i - start);
      ++field;
      start = i + 1;
    }
  }
  if (field != 8 || 0 != strcmp(magic, "AICOL1")) {
    return false;
  }
  out.max_retries = atoll(retries);
  snprintf(out.normalized, sizeof(out.normalized), "SYSTEM %s EXPR %s",
           out.system_prompt, out.user_expr);
  return true;
}

const ParseNode *node_child(const ParseNode *node, int64_t idx)
{
  if (node == nullptr || node->children_ == nullptr || idx < 0 || idx >= node->num_child_) {
    return nullptr;
  }
  return node->children_[idx];
}

void node_text(const ParseNode *node, char *dst, int64_t cap)
{
  if (node != nullptr && node->str_value_ != nullptr) {
    copy_buf(dst, cap, node->str_value_, node->str_len_);
  } else if (cap > 0) {
    dst[0] = '\0';
  }
}

void add_dep(char *deps, int64_t cap, const char *name)
{
  if (name == nullptr || name[0] == '\0') {
    return;
  }
  if (deps[0] != '\0') {
    const int64_t used = static_cast<int64_t>(strlen(deps));
    if (used + 1 < cap) {
      deps[used] = ',';
      deps[used + 1] = '\0';
    }
  }
  const int64_t used = static_cast<int64_t>(strlen(deps));
  copy_buf(deps + used, cap - used, name, static_cast<int64_t>(strlen(name)));
}

void walk_deps(const ParseNode *node, char *deps, int64_t cap)
{
  if (node == nullptr) {
    return;
  }
  if (T_COLUMN_REF == node->type_) {
    const ParseNode *name = node_child(node, 2);
    char tmp[AI_NAME] = {0};
    node_text(name, tmp, sizeof(tmp));
    add_dep(deps, cap, tmp);
  }
  for (int64_t i = 0; i < node->num_child_; ++i) {
    walk_deps(node_child(node, i), deps, cap);
  }
}

bool name_eq(const char *lhs, const ObString &rhs)
{
  if (lhs == nullptr || rhs.ptr() == nullptr) {
    return false;
  }
  const int64_t n = static_cast<int64_t>(strlen(lhs));
  return n == rhs.length() && 0 == strncasecmp(lhs, rhs.ptr(), n);
}

bool name_eq_cstr(const char *lhs, const char *rhs)
{
  return lhs != nullptr && rhs != nullptr && 0 == strcasecmp(lhs, rhs);
}

int reject_ai_deps(const char *deps, const ObIArray<ObString> &ai_names)
{
  int ret = OB_SUCCESS;
  if (deps == nullptr || deps[0] == '\0') {
    return ret;
  }
  const char *p = deps;
  while (OB_SUCC(ret) && *p != '\0') {
    while (*p == ',') {
      ++p;
    }
    const char *start = p;
    while (*p != '\0' && *p != ',') {
      ++p;
    }
    ObString dep(static_cast<int32_t>(p - start), start);
    if (dep.empty()) {
      continue;
    }
    for (int64_t i = 0; OB_SUCC(ret) && i < ai_names.count(); ++i) {
      if (name_eq(dep.ptr() == nullptr ? "" : "", ai_names.at(i))) {
      }
      const ObString &ai_name = ai_names.at(i);
      if (dep.length() == ai_name.length() && ai_name.ptr() != nullptr &&
          0 == strncasecmp(start, ai_name.ptr(), dep.length())) {
        ret = OB_NOT_SUPPORTED;
      }
    }
  }
  return ret;
}

int64_t vector_dim(const ObColumnSchemaV2 &column)
{
  const ObIArray<ObString> &ext = column.get_extended_type_info();
  if (ext.count() <= 0 || ext.at(0).ptr() == nullptr) {
    return -1;
  }
  char tmp[64] = {0};
  copy_str(tmp, sizeof(tmp), ext.at(0));
  if (0 != strncasecmp(tmp, "VECTOR(", 7)) {
    return -1;
  }
  return atoll(tmp + 7);
}

const char *result_kind_of(const ObColumnSchemaV2 &column)
{
  if (ObJsonType == column.get_data_type()) {
    return "json";
  }
  if (ob_is_int_tc(column.get_data_type()) || ob_is_uint_tc(column.get_data_type())) {
    return "int";
  }
  if (column.is_collection() && vector_dim(column) > 0) {
    return "vector";
  }
  return "string";
}

const char *system_prompt_of(const char *result_kind)
{
  if (0 == strcmp(result_kind, "json")) {
    return "Return exactly one valid JSON value";
  }
  if (0 == strcmp(result_kind, "int")) {
    return "Return exactly one base-10 integer";
  }
  return "";
}

int hidden_name(char *dst, int64_t cap, const char *prefix, const char *ai_name)
{
  const int n = snprintf(dst, cap, "%s%s", prefix, ai_name);
  if (n <= 0 || n >= cap) {
    return OB_SIZE_OVERFLOW;
  }
  return OB_SUCCESS;
}

struct ColSnap
{
  uint64_t column_id;
  int64_t max_retries;
  bool changed;
  char name[AI_NAME];
  char state_name[160];
  char request_name[160];
  char spec_name[160];
  char kind[16];
  char result_kind[16];
  char model[AI_NAME];
  char system_prompt[512];
  char user_expr[AI_EXPR];
  char deps[512];
};

struct RowSnap
{
  RowSnap *next;
  int op; // 1 insert, 2 update, 3 delete
  uint64_t table_id;
  bool pk_changed;
  int ncol;
  char db[AI_NAME];
  char table[AI_NAME];
  char old_pred[1024];
  char new_pred[1024];
  char request_id[80];
  ColSnap cols[AI_MAX_COLS];
};

struct CaptureHead
{
  RowSnap *head;
  RowSnap *tail;
};

int make_request_id(char *dst, int64_t cap)
{
  static uint64_t seq = 0;
  const uint64_t n = ATOMIC_AAF(&seq, 1);
  const int written = snprintf(dst, cap, "%016lx%016lx",
                               static_cast<unsigned long>(ObTimeUtility::current_time()),
                               static_cast<unsigned long>(n));
  return written > 0 && written < cap ? OB_SUCCESS : OB_SIZE_OVERFLOW;
}

const ObDMLBaseCtDef *primary_ctdef(ObTableModifyOp &op)
{
  const ObTableModifySpec &spec = op.get_spec();
  const ObDMLBaseCtDef *found = nullptr;
  if (PHY_INSERT == spec.get_type()) {
    const ObTableInsertSpec &ins = static_cast<const ObTableInsertSpec &>(spec);
    for (int64_t i = 0; i < ins.ins_ctdefs_.count(); ++i) {
      for (int64_t j = 0; j < ins.ins_ctdefs_.at(i).count(); ++j) {
        ObInsCtDef *ct = ins.ins_ctdefs_.at(i).at(j);
        if (ct != nullptr && ct->is_primary_index_) {
          found = ct;
        }
      }
    }
  } else if (PHY_UPDATE == spec.get_type()) {
    const ObTableUpdateSpec &upd = static_cast<const ObTableUpdateSpec &>(spec);
    for (int64_t i = 0; i < upd.upd_ctdefs_.count(); ++i) {
      for (int64_t j = 0; j < upd.upd_ctdefs_.at(i).count(); ++j) {
        ObUpdCtDef *ct = upd.upd_ctdefs_.at(i).at(j);
        if (ct != nullptr && ct->is_primary_index_) {
          found = ct;
        }
      }
    }
  } else if (PHY_DELETE == spec.get_type()) {
    const ObTableDeleteSpec &del = static_cast<const ObTableDeleteSpec &>(spec);
    for (int64_t i = 0; i < del.del_ctdefs_.count(); ++i) {
      for (int64_t j = 0; j < del.del_ctdefs_.at(i).count(); ++j) {
        ObDelCtDef *ct = del.del_ctdefs_.at(i).at(j);
        if (ct != nullptr && ct->is_primary_index_) {
          found = ct;
        }
      }
    }
  }
  return found;
}

template <typename Arr>
int64_t find_idx(const Arr &ids, uint64_t id)
{
  for (int64_t i = 0; i < ids.count(); ++i) {
    if (ids.at(i) == id) {
      return i;
    }
  }
  return -1;
}

int format_datum(const ObExpr *expr, const ObDatum *datum, ObSqlString &sql)
{
  int ret = OB_SUCCESS;
  if (OB_ISNULL(expr) || OB_ISNULL(datum) || datum->is_null()) {
    ret = sql.append("NULL");
  } else if (ob_is_int_tc(expr->datum_meta_.type_)) {
    ret = sql.append_fmt("%ld", datum->get_int());
  } else if (ob_is_uint_tc(expr->datum_meta_.type_)) {
    ret = sql.append_fmt("%lu", datum->get_uint());
  } else if (ob_is_string_tc(expr->datum_meta_.type_) || ob_is_text_tc(expr->datum_meta_.type_)) {
    const ObString text = datum->get_string();
    char tmp[1024] = {0};
    copy_str(tmp, sizeof(tmp), text);
    ret = append_quoted(sql, tmp);
  } else {
    ret = sql.append("NULL");
  }
  return ret;
}

int read_canon(ObTableModifyOp &op, const ExprFixedArray &row, int64_t idx,
               char *buf, int64_t cap, bool &is_null, bool &present)
{
  int ret = OB_SUCCESS;
  present = false;
  is_null = true;
  if (idx < 0 || idx >= row.count() || row.at(idx) == nullptr) {
    return ret;
  }
  ObDatum *datum = nullptr;
  if (OB_FAIL(row.at(idx)->eval(op.get_eval_ctx(), datum))) {
  } else if (OB_ISNULL(datum)) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    present = true;
    is_null = datum->is_null();
    if (!is_null && (ob_is_int_tc(row.at(idx)->datum_meta_.type_))) {
      snprintf(buf, cap, "%ld", datum->get_int());
    } else if (!is_null && ob_is_uint_tc(row.at(idx)->datum_meta_.type_)) {
      snprintf(buf, cap, "%lu", datum->get_uint());
    } else if (!is_null) {
      copy_str(buf, cap, datum->get_string());
    } else if (cap > 0) {
      buf[0] = '\0';
    }
  }
  return ret;
}

bool canon_diff(bool a_present, bool a_null, const char *a,
                bool b_present, bool b_null, const char *b)
{
  if (!a_present || !b_present) {
    return false;
  }
  if (a_null != b_null) {
    return true;
  }
  if (a_null) {
    return false;
  }
  return 0 != strcmp(a, b);
}

int build_predicate(ObTableModifyOp &op, const ObTableSchema &schema,
                    const ObDMLBaseCtDef &ctdef, bool use_new, ObSqlString &pred)
{
  int ret = OB_SUCCESS;
  const ExprFixedArray &row = use_new ? ctdef.new_row_ : ctdef.old_row_;
  const ObRowkeyInfo &rk = schema.get_rowkey_info();
  for (int64_t i = 0; OB_SUCC(ret) && i < rk.get_size(); ++i) {
    uint64_t col_id = 0;
    const ObColumnSchemaV2 *col = nullptr;
    if (OB_FAIL(rk.get_column_id(i, col_id))) {
    } else if (OB_ISNULL(col = schema.get_column_schema(col_id))) {
      ret = OB_ERR_UNEXPECTED;
    } else {
      const int64_t idx = find_idx(ctdef.column_ids_, col_id);
      ObDatum *datum = nullptr;
      if (idx < 0 || idx >= row.count() || OB_ISNULL(row.at(idx))) {
        ret = OB_ERR_UNEXPECTED;
      } else if (OB_FAIL(row.at(idx)->eval(op.get_eval_ctx(), datum))) {
      } else if (i > 0 && OB_FAIL(pred.append(" AND "))) {
      } else if (OB_FAIL(append_ident(pred, col->get_column_name()))) {
      } else if (OB_FAIL(pred.append(" = "))) {
      } else if (OB_FAIL(format_datum(row.at(idx), datum, pred))) {
      }
    }
  }
  return ret;
}

int fill_col_from_schema(const ObColumnSchemaV2 &column, ColSnap &snap)
{
  int ret = OB_SUCCESS;
  AiParsed parsed;
  memset(&snap, 0, sizeof(snap));
  if (!parse_comment(column.get_comment_str(), parsed)) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_FAIL(hidden_name(snap.state_name, sizeof(snap.state_name), "__ai_st_", column.get_column_name()))) {
  } else if (OB_FAIL(hidden_name(snap.request_name, sizeof(snap.request_name), "__ai_rq_", column.get_column_name()))) {
  } else if (OB_FAIL(hidden_name(snap.spec_name, sizeof(snap.spec_name), "__ai_sp_", column.get_column_name()))) {
  } else {
    snap.column_id = column.get_column_id();
    snap.max_retries = parsed.max_retries;
    copy_cstr(snap.name, sizeof(snap.name), column.get_column_name());
    copy_cstr(snap.kind, sizeof(snap.kind), parsed.kind);
    copy_cstr(snap.result_kind, sizeof(snap.result_kind), parsed.result_kind);
    copy_cstr(snap.model, sizeof(snap.model), parsed.model);
    copy_cstr(snap.system_prompt, sizeof(snap.system_prompt), parsed.system_prompt);
    copy_cstr(snap.user_expr, sizeof(snap.user_expr), parsed.user_expr);
    copy_cstr(snap.deps, sizeof(snap.deps), parsed.deps);
  }
  return ret;
}

bool dep_changed(ObTableModifyOp &op, const ObTableSchema &schema, const ObDMLBaseCtDef &ctdef,
                 const char *deps)
{
  if (deps == nullptr || deps[0] == '\0') {
    return false;
  }
  const char *p = deps;
  while (*p != '\0') {
    while (*p == ',') {
      ++p;
    }
    const char *start = p;
    while (*p != '\0' && *p != ',') {
      ++p;
    }
    if (p == start) {
      continue;
    }
    char dep[AI_NAME] = {0};
    copy_buf(dep, sizeof(dep), start, p - start);
    const ObColumnSchemaV2 *col = schema.get_column_schema(dep);
    if (col == nullptr) {
      continue;
    }
    const int64_t idx = find_idx(ctdef.column_ids_, col->get_column_id());
    char oldv[1024] = {0};
    char newv[1024] = {0};
    bool old_null = true;
    bool new_null = true;
    bool old_present = false;
    bool new_present = false;
    if (OB_SUCCESS != read_canon(op, ctdef.old_row_, idx, oldv, sizeof(oldv), old_null, old_present) ||
        OB_SUCCESS != read_canon(op, ctdef.new_row_, idx, newv, sizeof(newv), new_null, new_present)) {
      return true;
    }
    if (canon_diff(old_present, old_null, oldv, new_present, new_null, newv)) {
      return true;
    }
  }
  return false;
}

const char *META_TABLE =
    "CREATE TABLE IF NOT EXISTS oceanbase.__all_ai_column ("
    "table_id bigint NOT NULL, column_id bigint NOT NULL, column_name varchar(128) NOT NULL, "
    "state_column_id bigint NOT NULL, request_column_id bigint NOT NULL, "
    "result_spec_column_id bigint NOT NULL, normalized_expr longtext, max_retries bigint NOT NULL, "
    "sync_mode varchar(32), model_name varchar(128), kind varchar(32), user_expr longtext, "
    "dep_columns varchar(1024), result_kind varchar(32), system_prompt varchar(512), "
    "state_column_name varchar(128), request_column_name varchar(128), "
    "result_spec_column_name varchar(128), database_name varchar(128), table_name varchar(128), "
    "primary key (table_id, column_id))";

const char *TASK_TABLE =
    "CREATE TABLE IF NOT EXISTS oceanbase.__all_ai_column_task ("
    "table_id bigint NOT NULL, column_id bigint NOT NULL, rowkey_predicate varchar(1024) NOT NULL, "
    "request_id varchar(64) NOT NULL, eligible_ts datetime(6) NOT NULL, state bigint NOT NULL, "
    "retry_count bigint NOT NULL, prompt_text longtext, system_prompt varchar(512), "
    "model_name varchar(128), kind varchar(32), max_retries bigint NOT NULL, "
    "column_name varchar(128), state_column_name varchar(128), request_column_name varchar(128), "
    "result_kind varchar(32), database_name varchar(128), table_name varchar(128), "
    "primary key (table_id, column_id, rowkey_predicate))";

int ensure_meta_tables()
{
  int ret = OB_SUCCESS;
  ObMySQLProxy *proxy = GCTX.sql_proxy_;
  int64_t affected = 0;
  if (OB_ISNULL(proxy)) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_FAIL(proxy->write(META_TABLE, affected))) {
    LOG_WARN("create ai column meta table failed", K(ret));
  } else if (OB_FAIL(proxy->write(TASK_TABLE, affected))) {
    LOG_WARN("create ai column task table failed", K(ret));
  } else {
    ATOMIC_STORE(&g_meta_ready, 1);
  }
  return ret;
}

int load_schema(uint64_t table_id, int64_t schema_version, ObSchemaGetterGuard &guard,
                const ObTableSchema *&schema)
{
  int ret = OB_SUCCESS;
  schema = nullptr;
  ObMultiVersionSchemaService *service = GCTX.schema_service_;
  int64_t saved_version = 0;
  if (OB_ISNULL(service)) {
    ret = OB_ERR_UNEXPECTED;
  } else if (schema_version > 0 && OB_FAIL(service->async_refresh_schema(schema_version))) {
    LOG_WARN("async refresh schema failed", K(ret), K(schema_version), K(table_id));
    ret = OB_SUCCESS;
  }
  // ALTER ADD publishes the new columns after the simple-schema version moves.
  // Keep reading the latest guard until that version is visible.
  for (int attempt = 0; OB_SUCC(ret) && attempt < 20; ++attempt) {
    schema = nullptr;
    if (schema_version > 0 &&
        OB_FAIL(service->retry_get_schema_guard(schema_version, table_id, guard, saved_version))) {
      LOG_WARN("retry schema guard failed", K(ret), K(table_id), K(schema_version));
      ret = service->get_runtime_schema_guard(guard);
    } else if (schema_version <= 0 && OB_FAIL(service->get_runtime_schema_guard(guard))) {
    }
    if (OB_FAIL(ret)) {
    } else if (OB_FAIL(guard.get_table_schema(table_id, schema))) {
    } else if (schema != nullptr &&
               (schema_version <= 0 || schema->get_schema_version() >= schema_version)) {
      break;
    } else if (attempt + 1 < 20) {
      ob_usleep(50 * 1000);
    }
  }
  return ret;
}

int schema_names(ObSchemaGetterGuard &guard, const ObTableSchema &schema,
                 char *db, int64_t db_cap, char *table, int64_t table_cap)
{
  int ret = OB_SUCCESS;
  const ObDatabaseSchema *db_schema = nullptr;
  copy_str(table, table_cap, schema.get_table_name_str());
  if (OB_FAIL(guard.get_database_schema(schema.get_database_id(), db_schema))) {
  } else if (OB_ISNULL(db_schema)) {
    ret = OB_ERR_BAD_DATABASE;
  } else {
    copy_str(db, db_cap, db_schema->get_database_name_str());
  }
  return ret;
}

class AIColumnWorker : public lib::Threads
{
public:
  AIColumnWorker() : lib::Threads(1), started_(0) {}
  int ensure_started()
  {
    int ret = OB_SUCCESS;
    if (0 != ATOMIC_LOAD(&started_)) {
    } else if (ATOMIC_BCAS(&started_, 0, 1)) {
      lib::IRunWrapper *expect = lib::Threads::get_expect_run_wrapper();
      set_run_wrapper(expect != nullptr ? expect : share::server_runtime());
      if (OB_FAIL(start())) {
        ATOMIC_STORE(&started_, 0);
        LOG_WARN("start ai column worker failed", K(ret));
      }
    }
    return ret;
  }
  void run1() override
  {
    while (!has_set_stop()) {
      (void)ObAIColumnService::process_tasks();
      usleep(20000);
    }
  }
private:
  int started_;
};

AIColumnWorker &worker()
{
  static AIColumnWorker instance;
  return instance;
}

struct TaskRow
{
  uint64_t table_id;
  uint64_t column_id;
  int64_t retry_count;
  int64_t max_retries;
  char predicate[1024];
  char request_id[80];
  char prompt[AI_PROMPT];
  char system_prompt[512];
  char model[AI_NAME];
  char kind[16];
  char column_name[AI_NAME];
  char state_name[160];
  char request_name[160];
  char result_kind[16];
  char db[AI_NAME];
  char table[AI_NAME];
  char output[16384];
  bool http_ok;
};

int copy_mysql_str(sqlclient::ObMySQLResult &res, int64_t idx, char *dst, int64_t cap)
{
  int ret = OB_SUCCESS;
  ObString value;
  if (OB_FAIL(res.get_varchar(idx, value))) {
    if (OB_ERR_NULL_VALUE == ret) {
      dst[0] = '\0';
      ret = OB_SUCCESS;
    }
  } else {
    copy_str(dst, cap, value);
  }
  return ret;
}

void unwrap_json_string(const char *in, char *out, int64_t cap)
{
  const int64_t n = in == nullptr ? 0 : static_cast<int64_t>(strlen(in));
  if (n >= 2 && in[0] == '"' && in[n - 1] == '"') {
    int64_t w = 0;
    for (int64_t i = 1; i < n - 1 && w + 1 < cap; ++i) {
      if (in[i] == '\\' && i + 1 < n - 1) {
        const char e = in[++i];
        out[w++] = (e == 'n') ? '\n' : (e == 't') ? '\t' : e;
      } else {
        out[w++] = in[i];
      }
    }
    out[w] = '\0';
  } else {
    copy_cstr(out, cap, in);
  }
}

void normalize_vector(const char *in, char *out, int64_t cap)
{
  int64_t w = 0;
  if (in == nullptr) {
    copy_cstr(out, cap, "[]");
    return;
  }
  for (const char *p = in; *p != '\0' && w + 1 < cap;) {
    if (*p == ' ' || *p == '\n' || *p == '\t') {
      ++p;
      continue;
    }
    if (*p == '[' || *p == ']' || *p == ',') {
      out[w++] = *p++;
      continue;
    }
    const char *start = p;
    if (*p == '-') {
      ++p;
    }
    while ((*p >= '0' && *p <= '9') || *p == '.') {
      ++p;
    }
    char num[128] = {0};
    copy_buf(num, sizeof(num), start, p - start);
    char *dot = strchr(num, '.');
    if (dot != nullptr) {
      char *end = num + strlen(num) - 1;
      while (end > dot && *end == '0') {
        *end-- = '\0';
      }
      if (end == dot) {
        *end = '\0';
      }
    }
    for (const char *q = num; *q != '\0' && w + 1 < cap; ++q) {
      out[w++] = *q;
    }
  }
  out[w] = '\0';
}

int call_model(TaskRow &task)
{
  int ret = OB_SUCCESS;
  ObArenaAllocator allocator("AiColHttp");
  ObAIFuncExprInfo *info = nullptr;
  share::ObAiModelEndpointInfo endpoint;
  query::ObIAiEndpointResolver *resolver =
      ::oceanbase::share::server_service<::oceanbase::query::ObIAiEndpointResolver>();
  ObString model(task.model);
  ObString prompt(task.prompt);
  ObString system_prompt(task.system_prompt);
  ObString raw;
  task.http_ok = false;
  task.output[0] = '\0';
  if (OB_ISNULL(resolver)) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_FAIL(ObAIFuncUtils::get_ai_func_info(allocator, model, info)) || OB_ISNULL(info)) {
  } else if (OB_FAIL(resolver->resolve_by_model_name(model, allocator, endpoint))) {
  } else {
    ObAIFuncModel caller(allocator, *info, endpoint);
    if (0 == strcasecmp(task.kind, "embed")) {
      if (OB_FAIL(caller.call_dense_embedding(prompt, nullptr, raw))) {
      } else {
        char tmp[16384] = {0};
        unwrap_json_string(raw.ptr(), tmp, sizeof(tmp));
        normalize_vector(tmp[0] == '\0' ? raw.ptr() : tmp, task.output, sizeof(task.output));
        task.http_ok = true;
      }
    } else if (OB_FAIL(caller.call_completion_with_system(system_prompt, prompt, nullptr, raw))) {
    } else {
      unwrap_json_string(raw.ptr(), task.output, sizeof(task.output));
      task.http_ok = true;
    }
  }
  return ret;
}

int append_sql_result(ObSqlString &sql, const TaskRow &task)
{
  int ret = OB_SUCCESS;
  if (0 == strcmp(task.result_kind, "int")) {
    ret = sql.append(task.output[0] == '\0' ? "NULL" : task.output);
  } else if (0 == strcmp(task.result_kind, "vector")) {
    ret = sql.append(task.output[0] == '\0' ? "NULL" : task.output);
  } else if (0 == strcmp(task.result_kind, "json")) {
    if (OB_FAIL(sql.append("CAST("))) {
    } else if (OB_FAIL(append_quoted(sql, task.output))) {
    } else if (OB_FAIL(sql.append(" AS JSON)"))) {
    }
  } else if (OB_FAIL(append_quoted(sql, task.output))) {
  }
  return ret;
}

int publish_group(ObMySQLProxy &proxy, TaskRow *rows, int64_t begin, int64_t end, bool ok)
{
  int ret = OB_SUCCESS;
  ObMySQLTransaction trans;
  ObSqlString sql;
  int64_t affected = 0;
  bool started = false;
  if (OB_FAIL(trans.start(&proxy))) {
  } else {
    started = true;
  }
  if (OB_FAIL(ret)) {
  } else if (ok) {
    if (OB_FAIL(sql.append("UPDATE "))) {
    } else if (OB_FAIL(append_ident(sql, rows[begin].db))) {
    } else if (OB_FAIL(sql.append("."))) {
    } else if (OB_FAIL(append_ident(sql, rows[begin].table))) {
    } else if (OB_FAIL(sql.append(" SET "))) {
    }
    for (int64_t i = begin; OB_SUCC(ret) && i < end; ++i) {
      if (i > begin && OB_FAIL(sql.append(", "))) {
      } else if (OB_FAIL(append_ident(sql, rows[i].column_name))) {
      } else if (OB_FAIL(sql.append(" = "))) {
      } else if (OB_FAIL(append_sql_result(sql, rows[i]))) {
      } else if (OB_FAIL(sql.append(", "))) {
      } else if (OB_FAIL(append_ident(sql, rows[i].state_name))) {
      } else if (OB_FAIL(sql.append(" = 'READY'"))) {
      }
    }
    if (OB_SUCC(ret) && OB_FAIL(sql.append(" WHERE "))) {
    } else if (OB_SUCC(ret) && OB_FAIL(sql.append(rows[begin].predicate))) {
    }
    for (int64_t i = begin; OB_SUCC(ret) && i < end; ++i) {
      if (OB_FAIL(sql.append(" AND "))) {
      } else if (OB_FAIL(append_ident(sql, rows[i].request_name))) {
      } else if (OB_FAIL(sql.append(" = "))) {
      } else if (OB_FAIL(append_quoted(sql, rows[i].request_id))) {
      }
    }
    if (OB_SUCC(ret) && OB_FAIL(trans.write(sql.ptr(), affected))) {
      LOG_WARN("publish ai column failed", K(ret), K(sql));
    }
    if (OB_SUCC(ret) && affected > 0) {
      for (int64_t i = begin; OB_SUCC(ret) && i < end; ++i) {
        ObSqlString del;
        if (OB_FAIL(del.append_fmt(
                "DELETE FROM oceanbase.__all_ai_column_task WHERE table_id = %lu AND column_id = %lu AND request_id = ",
                rows[i].table_id, rows[i].column_id))) {
        } else if (OB_FAIL(append_quoted(del, rows[i].request_id))) {
        } else if (OB_FAIL(del.append(" AND rowkey_predicate = "))) {
        } else if (OB_FAIL(append_quoted(del, rows[i].predicate))) {
        } else if (OB_FAIL(trans.write(del.ptr(), affected))) {
        }
      }
    } else if (OB_SUCC(ret)) {
      for (int64_t i = begin; OB_SUCC(ret) && i < end; ++i) {
        ObSqlString del;
        if (OB_FAIL(del.append_fmt(
                "DELETE FROM oceanbase.__all_ai_column_task WHERE table_id = %lu AND column_id = %lu AND request_id = ",
                rows[i].table_id, rows[i].column_id))) {
        } else if (OB_FAIL(append_quoted(del, rows[i].request_id))) {
        } else if (OB_FAIL(del.append(" AND rowkey_predicate = "))) {
        } else if (OB_FAIL(append_quoted(del, rows[i].predicate))) {
        } else if (OB_FAIL(trans.write(del.ptr(), affected))) {
        }
      }
    }
  } else {
    bool can_retry = true;
    for (int64_t i = begin; i < end; ++i) {
      if (rows[i].retry_count >= rows[i].max_retries) {
        can_retry = false;
      }
    }
    if (can_retry) {
      for (int64_t i = begin; OB_SUCC(ret) && i < end; ++i) {
        ObSqlString upd;
        if (OB_FAIL(upd.append(
                "UPDATE oceanbase.__all_ai_column_task SET state = 3, retry_count = retry_count + 1, "
                "eligible_ts = DATE_ADD(NOW(6), INTERVAL 800000 MICROSECOND) WHERE table_id = "))) {
        } else if (OB_FAIL(upd.append_fmt("%lu AND column_id = %lu AND request_id = ",
                                          rows[i].table_id, rows[i].column_id))) {
        } else if (OB_FAIL(append_quoted(upd, rows[i].request_id))) {
        } else if (OB_FAIL(upd.append(" AND rowkey_predicate = "))) {
        } else if (OB_FAIL(append_quoted(upd, rows[i].predicate))) {
        } else if (OB_FAIL(trans.write(upd.ptr(), affected))) {
        }
      }
    } else {
      for (int64_t i = begin; OB_SUCC(ret) && i < end; ++i) {
        ObSqlString upd;
        if (OB_FAIL(upd.append("UPDATE "))) {
        } else if (OB_FAIL(append_ident(upd, rows[i].db))) {
        } else if (OB_FAIL(upd.append("."))) {
        } else if (OB_FAIL(append_ident(upd, rows[i].table))) {
        } else if (OB_FAIL(upd.append(" SET "))) {
        } else if (OB_FAIL(append_ident(upd, rows[i].state_name))) {
        } else if (OB_FAIL(upd.append(" = 'FAILED' WHERE "))) {
        } else if (OB_FAIL(upd.append(rows[i].predicate))) {
        } else if (OB_FAIL(upd.append(" AND "))) {
        } else if (OB_FAIL(append_ident(upd, rows[i].request_name))) {
        } else if (OB_FAIL(upd.append(" = "))) {
        } else if (OB_FAIL(append_quoted(upd, rows[i].request_id))) {
        } else if (OB_FAIL(trans.write(upd.ptr(), affected))) {
          if (missing_table(ret)) {
            ret = OB_SUCCESS;
          }
        }
        if (OB_SUCC(ret)) {
          ObSqlString del;
          if (OB_FAIL(del.append_fmt(
                  "DELETE FROM oceanbase.__all_ai_column_task WHERE table_id = %lu AND column_id = %lu AND request_id = ",
                  rows[i].table_id, rows[i].column_id))) {
          } else if (OB_FAIL(append_quoted(del, rows[i].request_id))) {
          } else if (OB_FAIL(del.append(" AND rowkey_predicate = "))) {
          } else if (OB_FAIL(append_quoted(del, rows[i].predicate))) {
          } else if (OB_FAIL(trans.write(del.ptr(), affected))) {
          }
        }
      }
    }
  }
  if (started) {
    const int end_ret = trans.end(OB_SUCCESS == ret);
    if (OB_SUCCESS == ret) {
      ret = end_ret;
    }
  }
  return ret;
}

int delete_tasks(ObIInnerSQLConnection &conn, uint64_t table_id, uint64_t column_id, const char *pred)
{
  int ret = OB_SUCCESS;
  ObSqlString sql;
  int64_t affected = 0;
  if (pred == nullptr || pred[0] == '\0') {
    return ret;
  } else if (OB_FAIL(sql.append_fmt(
                 "DELETE FROM oceanbase.__all_ai_column_task WHERE table_id = %lu", table_id))) {
  } else if (column_id != 0 && OB_FAIL(sql.append_fmt(" AND column_id = %lu", column_id))) {
  } else if (OB_FAIL(sql.append(" AND rowkey_predicate = "))) {
  } else if (OB_FAIL(append_quoted(sql, pred))) {
  } else if (OB_FAIL(conn.execute_write(ObString(sql.ptr()), affected, false))) {
  }
  return ret;
}

int insert_task(ObIInnerSQLConnection &conn, const RowSnap &row, const ColSnap &col, const char *prompt)
{
  int ret = OB_SUCCESS;
  ObSqlString sql;
  int64_t affected = 0;
  if (OB_FAIL(sql.append(
          "INSERT INTO oceanbase.__all_ai_column_task (table_id, column_id, rowkey_predicate, "
          "request_id, eligible_ts, state, retry_count, prompt_text, system_prompt, model_name, "
          "kind, max_retries, column_name, state_column_name, request_column_name, result_kind, "
          "database_name, table_name) VALUES ("))) {
  } else if (OB_FAIL(sql.append_fmt("%lu, %lu, ", row.table_id, col.column_id))) {
  } else if (OB_FAIL(append_quoted(sql, row.new_pred))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, row.request_id))) {
  } else if (OB_FAIL(sql.append(", NOW(6), 0, 0, "))) {
  } else if (OB_FAIL(append_quoted(sql, prompt))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, col.system_prompt))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, col.model))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, col.kind))) {
  } else if (OB_FAIL(sql.append_fmt(", %ld, ", col.max_retries))) {
  } else if (OB_FAIL(append_quoted(sql, col.name))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, col.state_name))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, col.request_name))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, col.result_kind))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, row.db))) {
  } else if (OB_FAIL(sql.append(", "))) {
  } else if (OB_FAIL(append_quoted(sql, row.table))) {
  } else if (OB_FAIL(sql.append(")"))) {
  } else if (OB_FAIL(conn.execute_write(ObString(sql.ptr()), affected, false))) {
  }
  return ret;
}

int read_prompts(ObTableModifyOp &op, RowSnap &row, char prompts[][AI_PROMPT])
{
  int ret = OB_SUCCESS;
  ObSqlString sql;
  int touched = 0;
  int indexes[AI_MAX_COLS] = {0};
  for (int i = 0; i < row.ncol; ++i) {
    prompts[i][0] = '\0';
    if (row.cols[i].changed) {
      indexes[touched++] = i;
    }
  }
  if (0 == touched) {
    return ret;
  }
  if (OB_FAIL(sql.append("SELECT "))) {
  }
  for (int i = 0; OB_SUCC(ret) && i < touched; ++i) {
    if (i > 0 && OB_FAIL(sql.append(", "))) {
    } else if (OB_FAIL(sql.append("IFNULL(CAST(("))) {
    } else if (OB_FAIL(sql.append(row.cols[indexes[i]].user_expr))) {
    } else if (OB_FAIL(sql.append(") AS CHAR), '')"))) {
    }
  }
  if (OB_SUCC(ret) && OB_FAIL(sql.append(" FROM "))) {
  } else if (OB_SUCC(ret) && OB_FAIL(append_ident(sql, row.db))) {
  } else if (OB_SUCC(ret) && OB_FAIL(sql.append("."))) {
  } else if (OB_SUCC(ret) && OB_FAIL(append_ident(sql, row.table))) {
  } else if (OB_SUCC(ret) && OB_FAIL(sql.append(" WHERE "))) {
  } else if (OB_SUCC(ret) && OB_FAIL(sql.append(row.new_pred))) {
  }
  if (OB_FAIL(ret)) {
  } else {
    ObMySQLProxy::MySQLResult res;
    if (OB_FAIL(op.execute_read(sql.ptr(), res))) {
      LOG_WARN("read ai prompt failed", K(ret), K(sql));
    } else if (OB_ISNULL(res.get_result())) {
      ret = OB_ERR_UNEXPECTED;
    } else if (OB_FAIL(res.get_result()->next())) {
      if (OB_ITER_END == ret) {
        ret = OB_SUCCESS;
      }
    } else {
      for (int i = 0; OB_SUCC(ret) && i < touched; ++i) {
        ret = copy_mysql_str(*res.get_result(), i, prompts[indexes[i]], AI_PROMPT);
      }
    }
    int close_ret = res.close();
    if (OB_SUCCESS == ret) {
      ret = close_ret;
    }
  }
  return ret;
}

int invalidate_row(ObTableModifyOp &op, RowSnap &row, char prompts[][AI_PROMPT])
{
  int ret = OB_SUCCESS;
  ObSqlString sql;
  int64_t affected = 0;
  bool first = true;
  if (OB_FAIL(sql.append("UPDATE "))) {
  } else if (OB_FAIL(append_ident(sql, row.db))) {
  } else if (OB_FAIL(sql.append("."))) {
  } else if (OB_FAIL(append_ident(sql, row.table))) {
  } else if (OB_FAIL(sql.append(" SET "))) {
  }
  for (int i = 0; OB_SUCC(ret) && i < row.ncol; ++i) {
    if (!row.cols[i].changed) {
      continue;
    }
    if (!first && OB_FAIL(sql.append(", "))) {
    } else if (OB_FAIL(append_ident(sql, row.cols[i].name))) {
    } else if (OB_FAIL(sql.append(" = NULL, "))) {
    } else if (OB_FAIL(append_ident(sql, row.cols[i].state_name))) {
    } else if (OB_FAIL(sql.append(" = 'PENDING', "))) {
    } else if (OB_FAIL(append_ident(sql, row.cols[i].request_name))) {
    } else if (OB_FAIL(sql.append(" = "))) {
    } else if (OB_FAIL(append_quoted(sql, row.request_id))) {
    } else if (OB_FAIL(sql.append(", "))) {
    } else if (OB_FAIL(append_ident(sql, row.cols[i].spec_name))) {
    } else if (OB_FAIL(sql.append(" = "))) {
    } else if (OB_FAIL(append_quoted(sql, prompts[i]))) {
    } else {
      first = false;
    }
  }
  if (OB_SUCC(ret) && first) {
    return ret;
  }
  if (OB_SUCC(ret) && OB_FAIL(sql.append(" WHERE "))) {
  } else if (OB_SUCC(ret) && OB_FAIL(sql.append(row.new_pred))) {
  } else if (OB_ISNULL(op.inner_conn_)) {
    ret = OB_NOT_INIT;
  } else if (OB_FAIL(op.inner_conn_->execute_write(ObString(sql.ptr()), affected, false))) {
    LOG_WARN("invalidate ai column failed", K(ret), K(sql));
  }
  for (int i = 0; OB_SUCC(ret) && i < row.ncol; ++i) {
    if (!row.cols[i].changed) {
      continue;
    }
    if (row.pk_changed && OB_FAIL(delete_tasks(*op.inner_conn_, row.table_id, row.cols[i].column_id, row.old_pred))) {
    } else if (OB_FAIL(delete_tasks(*op.inner_conn_, row.table_id, row.cols[i].column_id, row.new_pred))) {
    } else if (OB_FAIL(insert_task(*op.inner_conn_, row, row.cols[i], prompts[i]))) {
    }
  }
  return ret;
}

} // namespace

bool ObAIColumnService::allow_hidden_access()
{
  return g_allow_hidden;
}

bool ObAIColumnService::is_aux_column_name(const ObString &name)
{
  return name.length() > 8 && name.ptr() != nullptr &&
         (0 == strncasecmp(name.ptr(), "__ai_st_", 8) ||
          0 == strncasecmp(name.ptr(), "__ai_rq_", 8) ||
          0 == strncasecmp(name.ptr(), "__ai_sp_", 8));
}

bool ObAIColumnService::is_ai_column(const ObColumnSchemaV2 &column)
{
  const ObString &comment = column.get_comment_str();
  if (comment.length() >= 6 && comment.ptr() != nullptr &&
      0 == memcmp(comment.ptr(), "AICOL1", 6)) {
    return true;
  }
  return (column.get_column_flags() & RESERVED_COLUMN_FLAG_4) != 0 &&
         comment.length() >= 6;
}

int ObAIColumnService::fill_hidden_column(ObColumnSchemaV2 &column,
                                          const ObString &ai_name,
                                          const char *prefix,
                                          int32_t data_length)
{
  int ret = OB_SUCCESS;
  char name[256] = {0};
  const int n = snprintf(name, sizeof(name), "%s%.*s", prefix, ai_name.length(), ai_name.ptr());
  if (n <= 0 || n >= static_cast<int>(sizeof(name))) {
    ret = OB_SIZE_OVERFLOW;
  } else if (OB_FAIL(column.set_column_name(name))) {
  } else {
    column.set_rowkey_position(0);
    column.set_index_position(0);
    column.set_tbl_part_key_pos(0);
    column.set_is_hidden(true);
    column.set_nullable(true);
    column.set_data_type(ObVarcharType);
    column.set_data_length(data_length);
    column.set_collation_type(CS_TYPE_UTF8MB4_GENERAL_CI);
    column.set_charset_type(ObCharset::charset_type_by_coll(CS_TYPE_UTF8MB4_GENERAL_CI));
    column.set_prev_column_id(UINT64_MAX);
    column.set_next_column_id(UINT64_MAX);
  }
  return ret;
}

int ObAIColumnService::define_column(ObColumnSchemaV2 &column, const ParseNode *node)
{
  int ret = OB_SUCCESS;
  const ParseNode *func = node == nullptr ? nullptr : node_child(node, 4);
  const ParseNode *params = func == nullptr ? nullptr : node_child(func, 1);
  const ParseNode *name = func == nullptr ? nullptr : node_child(func, 0);
  const ParseNode *options = node == nullptr ? nullptr : node_child(node, 5);
  char func_name[AI_NAME] = {0};
  char model[AI_NAME] = {0};
  char user_expr[AI_EXPR] = {0};
  char deps[512] = {0};
  int64_t embed_dim = -1;
  int64_t max_retries = 0;
  if (OB_ISNULL(node) || node->num_child_ != 6 || node->value_ != 1 || OB_ISNULL(func) ||
      OB_ISNULL(params) || params->num_child_ < 2) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    node_text(name, func_name, sizeof(func_name));
    node_text(node_child(params, 0), model, sizeof(model));
    node_text(node_child(params, 1), user_expr, sizeof(user_expr));
    walk_deps(node_child(params, 1), deps, sizeof(deps));
    if (params->num_child_ >= 3 && node_child(params, 2) != nullptr) {
      embed_dim = node_child(params, 2)->value_;
    }
  }
  if (OB_SUCC(ret) && options != nullptr) {
    // merge_nodes flattens T_LINK_NODE pairs into name, value, name, value.
    for (int64_t i = 0; i + 1 < options->num_child_; i += 2) {
      char opt_name[AI_NAME] = {0};
      node_text(node_child(options, i), opt_name, sizeof(opt_name));
      const ParseNode *val = node_child(options, i + 1);
      if (0 == strcasecmp(opt_name, "max_retries") && val != nullptr) {
        if (T_INT == val->type_ || T_UINT64 == val->type_) {
          max_retries = val->value_;
        } else if (val->str_value_ != nullptr) {
          max_retries = atoll(val->str_value_);
        }
      }
    }
  }
  const bool embed = 0 == strcasecmp(func_name, "AI_EMBED");
  const char *result_kind = result_kind_of(column);
  const int64_t expected_dim = vector_dim(column);
  if (OB_FAIL(ret)) {
  } else if (!embed && 0 != strcasecmp(func_name, "AI_COMPLETE")) {
    ret = OB_NOT_SUPPORTED;
  } else if (embed && embed_dim > 0 && expected_dim > 0 && embed_dim != expected_dim) {
    ret = OB_ERR_INVALID_VECTOR_DIM;
    LOG_USER_ERROR(OB_ERR_INVALID_VECTOR_DIM, static_cast<uint32_t>(expected_dim),
                   static_cast<uint32_t>(embed_dim));
  } else {
    char comment[2048] = {0};
    const int n = snprintf(comment, sizeof(comment),
                           "AICOL1\x1f%s\x1f%s\x1f%ld\x1f%s\x1f%s\x1f%s\x1f%s",
                           embed ? "embed" : "complete", model, max_retries, result_kind, deps,
                           system_prompt_of(result_kind), user_expr);
    if (n <= 0 || n >= static_cast<int>(sizeof(comment))) {
      ret = OB_SIZE_OVERFLOW;
    } else if (OB_FAIL(column.set_comment(comment))) {
    } else {
      column.add_column_flag(RESERVED_COLUMN_FLAG_4);
      column.set_nullable(true);
    }
  }
  return ret;
}

int ObAIColumnService::check_create_dependencies(const ObIArray<ObColumnSchemaV2> &columns)
{
  int ret = OB_SUCCESS;
  ObSEArray<ObString, 8> names;
  for (int64_t i = 0; OB_SUCC(ret) && i < columns.count(); ++i) {
    if (is_ai_column(columns.at(i)) && OB_FAIL(names.push_back(columns.at(i).get_column_name_str()))) {
    }
  }
  for (int64_t i = 0; OB_SUCC(ret) && i < columns.count(); ++i) {
    AiParsed parsed;
    if (!is_ai_column(columns.at(i))) {
    } else if (!parse_comment(columns.at(i).get_comment_str(), parsed)) {
    } else if (OB_FAIL(reject_ai_deps(parsed.deps, names))) {
    }
  }
  return ret;
}

int ObAIColumnService::check_alter_dependency(const ObTableSchema &existing,
                                              const ObTableSchema &added,
                                              const ObColumnSchemaV2 &column)
{
  int ret = OB_SUCCESS;
  ObSEArray<ObString, 8> names;
  AiParsed parsed;
  if (OB_FAIL(names.push_back(column.get_column_name_str()))) {
  }
  for (int64_t i = 0; OB_SUCC(ret) && i < existing.get_column_count(); ++i) {
    const ObColumnSchemaV2 *col = existing.get_column_schema_by_idx(i);
    if (col != nullptr && is_ai_column(*col) && OB_FAIL(names.push_back(col->get_column_name_str()))) {
    }
  }
  for (int64_t i = 0; OB_SUCC(ret) && i < added.get_column_count(); ++i) {
    const ObColumnSchemaV2 *col = added.get_column_schema_by_idx(i);
    if (col != nullptr && is_ai_column(*col) && OB_FAIL(names.push_back(col->get_column_name_str()))) {
    }
  }
  if (OB_FAIL(ret)) {
  } else if (!parse_comment(column.get_comment_str(), parsed)) {
    ret = OB_SUCCESS;
  } else if (OB_FAIL(reject_ai_deps(parsed.deps, names))) {
  }
  return ret;
}


int ObAIColumnService::register_after_ddl(uint64_t table_id, int64_t schema_version)
{
  int ret = OB_SUCCESS;
  ObSchemaGetterGuard guard;
  const ObTableSchema *schema = nullptr;
  if (OB_INVALID_ID == table_id) {
    return ret;
  } else if (OB_FAIL(load_schema(table_id, schema_version, guard, schema))) {
    LOG_WARN("load schema after ddl failed", K(ret), K(table_id), K(schema_version));
    return OB_SUCCESS;
  } else if (OB_ISNULL(schema)) {
    return ret;
  }
  bool has_ai = false;
  for (int64_t i = 0; i < schema->get_column_count(); ++i) {
    const ObColumnSchemaV2 *col = schema->get_column_schema_by_idx(i);
    if (col != nullptr && is_ai_column(*col)) {
      has_ai = true;
      AiParsed parsed_probe;
      if (!parse_comment(col->get_comment_str(), parsed_probe)) {
        LOG_WARN("ai column comment is not parseable", K(table_id), K(col->get_column_name_str()),
                 K(col->get_comment_str()));
      }
    }
  }
  ObMySQLProxy *proxy = GCTX.sql_proxy_;
  if (!has_ai) {
    if (proxy != nullptr && 0 != ATOMIC_LOAD(&g_meta_ready)) {
      int64_t affected = 0;
      ObSqlString sql;
      if (OB_FAIL(sql.append_fmt("DELETE FROM oceanbase.__all_ai_column WHERE table_id = %lu", table_id))) {
      } else if (OB_FAIL(proxy->write(sql.ptr(), affected))) {
        if (missing_table(ret)) {
          ret = OB_SUCCESS;
        }
      }
    }
    return OB_SUCCESS;
  }
  char db[AI_NAME] = {0};
  char table[AI_NAME] = {0};
  if (OB_ISNULL(proxy)) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_FAIL(ensure_meta_tables())) {
  } else if (OB_FAIL(schema_names(guard, *schema, db, sizeof(db), table, sizeof(table)))) {
  } else {
    ObMySQLTransaction trans;
    bool started = false;
    int64_t affected = 0;
    ObSqlString del;
    if (OB_FAIL(trans.start(proxy))) {
    } else if (FALSE_IT(started = true)) {
    } else if (OB_FAIL(del.append_fmt("DELETE FROM oceanbase.__all_ai_column WHERE table_id = %lu", table_id))) {
    } else if (OB_FAIL(trans.write(del.ptr(), affected))) {
    }
    for (int64_t i = 0; OB_SUCC(ret) && i < schema->get_column_count(); ++i) {
      const ObColumnSchemaV2 *col = schema->get_column_schema_by_idx(i);
      AiParsed parsed;
      if (col == nullptr || !parse_comment(col->get_comment_str(), parsed)) {
        continue;
      }
      char state_name[160] = {0};
      char request_name[160] = {0};
      char spec_name[160] = {0};
      const ObColumnSchemaV2 *state = nullptr;
      const ObColumnSchemaV2 *request = nullptr;
      const ObColumnSchemaV2 *spec = nullptr;
      if (OB_FAIL(hidden_name(state_name, sizeof(state_name), "__ai_st_", col->get_column_name()))) {
      } else if (OB_FAIL(hidden_name(request_name, sizeof(request_name), "__ai_rq_", col->get_column_name()))) {
      } else if (OB_FAIL(hidden_name(spec_name, sizeof(spec_name), "__ai_sp_", col->get_column_name()))) {
      } else if (OB_ISNULL(state = schema->get_column_schema(state_name)) ||
                 OB_ISNULL(request = schema->get_column_schema(request_name)) ||
                 OB_ISNULL(spec = schema->get_column_schema(spec_name))) {
        ret = OB_ERR_UNEXPECTED;
        LOG_WARN("ai hidden column missing", K(ret), K(state_name));
      } else {
        ObSqlString ins;
        if (OB_FAIL(ins.append(
                "INSERT INTO oceanbase.__all_ai_column (table_id, column_id, column_name, "
                "state_column_id, request_column_id, result_spec_column_id, normalized_expr, "
                "max_retries, sync_mode, model_name, kind, user_expr, dep_columns, result_kind, "
                "system_prompt, state_column_name, request_column_name, result_spec_column_name, "
                "database_name, table_name) VALUES ("))) {
        } else if (OB_FAIL(ins.append_fmt("%lu, %lu, ", table_id, col->get_column_id()))) {
        } else if (OB_FAIL(append_quoted(ins, col->get_column_name()))) {
        } else if (OB_FAIL(ins.append_fmt(", %lu, %lu, %lu, ", state->get_column_id(),
                                          request->get_column_id(), spec->get_column_id()))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.normalized))) {
        } else if (OB_FAIL(ins.append_fmt(", %ld, 'async', ", parsed.max_retries))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.model))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.kind))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.user_expr))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.deps))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.result_kind))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, parsed.system_prompt))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, state_name))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, request_name))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, spec_name))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, db))) {
        } else if (OB_FAIL(ins.append(", "))) {
        } else if (OB_FAIL(append_quoted(ins, table))) {
        } else if (OB_FAIL(ins.append(")"))) {
        } else if (OB_FAIL(trans.write(ins.ptr(), affected))) {
          LOG_WARN("insert ai column meta failed", K(ret), K(ins));
        }
      }
    }
    if (started) {
      const int end_ret = trans.end(OB_SUCCESS == ret);
      if (OB_SUCCESS == ret) {
        ret = end_ret;
      }
    }
    if (OB_SUCC(ret)) {
      ret = worker().ensure_started();
    }
  }
  return ret;
}

int ObAIColumnService::capture_row(ObTableModifyOp &op)
{
  int ret = OB_SUCCESS;
  if (g_in_maintenance || 0 == ATOMIC_LOAD(&g_meta_ready)) {
    return ret;
  }
  const ObDMLBaseCtDef *ctdef = primary_ctdef(op);
  if (ctdef == nullptr) {
    return ret;
  }
  const int op_type = PHY_INSERT == op.get_spec().get_type() ? 1 :
                      PHY_DELETE == op.get_spec().get_type() ? 3 : 2;
  // das_base_ctdef_.table_id_ is the optimizer location id. The schema id is index_tid_.
  const uint64_t schema_table_id = ctdef->das_base_ctdef_.index_tid_;
  ObSchemaGetterGuard guard;
  const ObTableSchema *schema = nullptr;
  if (OB_INVALID_ID == schema_table_id) {
    return ret;
  } else if (OB_FAIL(GCTX.schema_service_ == nullptr
                         ? OB_ERR_UNEXPECTED
                         : GCTX.schema_service_->get_runtime_schema_guard(guard))) {
    return OB_SUCCESS;
  } else if (OB_FAIL(guard.get_table_schema(schema_table_id, schema)) || OB_ISNULL(schema)) {
    return OB_SUCCESS;
  }
  RowSnap probe;
  memset(&probe, 0, sizeof(probe));
  probe.op = op_type;
  probe.table_id = schema->get_table_id();
  if (OB_FAIL(schema_names(guard, *schema, probe.db, sizeof(probe.db), probe.table, sizeof(probe.table)))) {
    return OB_SUCCESS;
  }
  for (int64_t i = 0; i < schema->get_column_count() && probe.ncol < AI_MAX_COLS; ++i) {
    const ObColumnSchemaV2 *col = schema->get_column_schema_by_idx(i);
    if (col != nullptr && is_ai_column(*col)) {
      if (OB_FAIL(fill_col_from_schema(*col, probe.cols[probe.ncol]))) {
        LOG_WARN("parse ai column failed", K(ret), K(col->get_column_name_str()));
        return OB_SUCCESS;
      }
      ++probe.ncol;
    }
  }
  if (0 == probe.ncol) {
    return OB_SUCCESS;
  }
  ObSqlString old_pred;
  ObSqlString new_pred;
  if (1 != op_type && OB_FAIL(build_predicate(op, *schema, *ctdef, false, old_pred))) {
  } else if (3 != op_type && OB_FAIL(build_predicate(op, *schema, *ctdef, true, new_pred))) {
  }
  if (OB_FAIL(ret)) {
    LOG_WARN("build ai row predicate failed", K(ret));
    return ret;
  }
  copy_cstr(probe.old_pred, sizeof(probe.old_pred), old_pred.ptr());
  copy_cstr(probe.new_pred, sizeof(probe.new_pred), new_pred.ptr());
  probe.pk_changed = 2 == op_type && 0 != strcmp(probe.old_pred, probe.new_pred);
  bool any = 3 == op_type || 1 == op_type || probe.pk_changed;
  for (int i = 0; i < probe.ncol; ++i) {
    probe.cols[i].changed = 1 == op_type || 3 == op_type || probe.pk_changed ||
                            dep_changed(op, *schema, *ctdef, probe.cols[i].deps);
    any = any || probe.cols[i].changed;
  }
  if (!any) {
    return OB_SUCCESS;
  }
  if (3 != op_type && OB_FAIL(make_request_id(probe.request_id, sizeof(probe.request_id)))) {
    return ret;
  }
  void *mem = op.get_exec_ctx().get_allocator().alloc(sizeof(RowSnap));
  void *head_mem = nullptr;
  if (op.ai_column_capture_ == nullptr) {
    head_mem = op.get_exec_ctx().get_allocator().alloc(sizeof(CaptureHead));
  }
  if (OB_ISNULL(mem) || (op.ai_column_capture_ == nullptr && OB_ISNULL(head_mem))) {
    ret = OB_ALLOCATE_MEMORY_FAILED;
  } else {
    if (op.ai_column_capture_ == nullptr) {
      CaptureHead *head = new (head_mem) CaptureHead();
      head->head = nullptr;
      head->tail = nullptr;
      op.ai_column_capture_ = head;
    }
    RowSnap *snap = new (mem) RowSnap();
    *snap = probe;
    snap->next = nullptr;
    CaptureHead *head = static_cast<CaptureHead *>(op.ai_column_capture_);
    if (head->tail == nullptr) {
      head->head = snap;
      head->tail = snap;
    } else {
      head->tail->next = snap;
      head->tail = snap;
    }
  }
  return ret;
}

int ObAIColumnService::finish_dml(ObTableModifyOp &op)
{
  int ret = OB_SUCCESS;
  if (g_in_maintenance || op.ai_column_capture_ == nullptr) {
    return ret;
  }
  CaptureHead *head = static_cast<CaptureHead *>(op.ai_column_capture_);
  if (head->head == nullptr) {
    return ret;
  }
  AiTlsGuard guard(true);
  ObSEArray<ObDASTableLoc *, 8> locs;
  ObSEArray<uint8_t, 8> saved;
  DASTableLocList &list = op.get_exec_ctx().get_das_ctx().get_table_loc_list();
  for (DASTableLocList::iterator it = list.begin(); it != list.end(); ++it) {
    if (*it != nullptr && OB_SUCC(locs.push_back(*it)) && OB_SUCC(saved.push_back((*it)->is_writing_ ? 1 : 0))) {
      (*it)->is_writing_ = false;
    }
  }
  bool began = false;
  // begin_nested_session() bumps session nested_count_. Inner SQL is only exempt
  // from set_end_stmt() when the parent statement is marked nested (FK/PL/stats).
  // Reuse the FK marker so prompt SELECT and the invalidation UPDATE share the
  // user transaction without tripping OB_ERR_UNEXPECTED (-4016).
  ObDASCtx &das_ctx = op.get_exec_ctx().get_das_ctx();
  const bool saved_fk_cascading = das_ctx.is_fk_cascading_;
  if (OB_FAIL(op.open_inner_conn())) {
    LOG_WARN("open inner connection for ai column failed", K(ret));
  } else if (OB_FAIL(op.begin_nested_session(true))) {
  } else {
    began = true;
    das_ctx.is_fk_cascading_ = true;
  }
  for (RowSnap *row = head->head; OB_SUCC(ret) && row != nullptr; row = row->next) {
    if (3 == row->op) {
      if (OB_ISNULL(op.inner_conn_)) {
        ret = OB_NOT_INIT;
      } else if (OB_FAIL(delete_tasks(*op.inner_conn_, row->table_id, 0, row->old_pred))) {
      }
    } else {
      char prompts[AI_MAX_COLS][AI_PROMPT];
      memset(prompts, 0, sizeof(prompts));
      if (OB_FAIL(read_prompts(op, *row, prompts))) {
      } else if (OB_FAIL(invalidate_row(op, *row, prompts))) {
      }
    }
  }
  if (began) {
    das_ctx.is_fk_cascading_ = saved_fk_cascading;
    const int end_ret = op.end_nested_session();
    if (OB_SUCCESS == ret) {
      ret = end_ret;
    }
  }
  for (int64_t i = 0; i < locs.count() && i < saved.count(); ++i) {
    locs.at(i)->is_writing_ = saved.at(i) != 0;
  }
  head->head = nullptr;
  head->tail = nullptr;
  return ret;
}

int ObAIColumnService::check_strict_read(ObSQLSessionInfo &session, const ObString &sql)
{
  int ret = OB_SUCCESS;
  if (g_hidden_depth > 0 || session.is_nested_session() || sql.empty() ||
      0 == ATOMIC_LOAD(&g_meta_ready)) {
    return ret;
  }
  bool strict = false;
  if (OB_FAIL(session.get_sys_variable(SYS_VAR_AI_COLUMN_STRICT_READ, strict))) {
    LOG_WARN("read ai_column_strict_read failed", K(ret));
    return OB_SUCCESS;
  } else if (!strict) {
    return ret;
  }
  AiTlsGuard guard(false);
  ObMySQLProxy *proxy = GCTX.sql_proxy_;
  if (OB_ISNULL(proxy)) {
    return ret;
  }
  ObMySQLProxy::MySQLResult res;
  if (OB_FAIL(proxy->read(
          res, "SELECT table_id, column_id, column_name, state_column_name, database_name, table_name "
               "FROM oceanbase.__all_ai_column"))) {
    if (missing_table(ret)) {
      ret = OB_SUCCESS;
    }
    return ret;
  } else if (OB_ISNULL(res.get_result())) {
    return ret;
  }
  struct Item {
    uint64_t table_id;
    uint64_t column_id;
    char name[AI_NAME];
    char state[160];
    char db[AI_NAME];
    char table[AI_NAME];
  };
  Item items[64];
  int n = 0;
  while (OB_SUCC(ret) && n < 64 && OB_SUCC(res.get_result()->next())) {
    Item &item = items[n];
    memset(&item, 0, sizeof(item));
    if (OB_FAIL(res.get_result()->get_int(static_cast<int64_t>(0), reinterpret_cast<int64_t &>(item.table_id)))) {
    } else if (OB_FAIL(res.get_result()->get_int(static_cast<int64_t>(1), reinterpret_cast<int64_t &>(item.column_id)))) {
    } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 2, item.name, sizeof(item.name)))) {
    } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 3, item.state, sizeof(item.state)))) {
    } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 4, item.db, sizeof(item.db)))) {
    } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 5, item.table, sizeof(item.table)))) {
    } else {
      ++n;
    }
  }
  if (OB_ITER_END == ret) {
    ret = OB_SUCCESS;
  }
  for (int i = 0; OB_SUCC(ret) && i < n; ++i) {
    if (!contains_ident(sql.ptr(), sql.length(), items[i].name)) {
      continue;
    }
    ObSqlString check;
    ObMySQLProxy::MySQLResult hit;
    if (OB_FAIL(check.append("SELECT 1 FROM "))) {
    } else if (OB_FAIL(append_ident(check, items[i].db))) {
    } else if (OB_FAIL(check.append("."))) {
    } else if (OB_FAIL(append_ident(check, items[i].table))) {
    } else if (OB_FAIL(check.append(" WHERE "))) {
    } else if (OB_FAIL(append_ident(check, items[i].state))) {
    } else if (OB_FAIL(check.append(" IS NULL OR "))) {
    } else if (OB_FAIL(append_ident(check, items[i].state))) {
    } else if (OB_FAIL(check.append(" <> 'READY' LIMIT 1"))) {
    } else if (OB_FAIL(proxy->read(hit, check.ptr()))) {
    } else if (OB_ISNULL(hit.get_result())) {
      ret = OB_ERR_UNEXPECTED;
    } else if (OB_FAIL(hit.get_result()->next())) {
      if (OB_ITER_END == ret) {
        ret = OB_SUCCESS;
      }
    } else {
      LOG_USER_ERROR(OB_AI_COLUMN_NOT_READY, static_cast<int64_t>(items[i].table_id),
                     static_cast<int64_t>(items[i].column_id));
      ret = OB_AI_COLUMN_NOT_READY;
    }
  }
  return ret;
}

int ObAIColumnService::prepare_retry(ObAlterTableStmt &stmt, const ParseNode &action)
{
  int ret = OB_SUCCESS;
  const ParseNode *name = node_child(&action, 0);
  const ParseNode *where = node_child(&action, 1);
  char column[AI_NAME] = {0};
  node_text(name, column, sizeof(column));
  ObString column_name(column);
  ObString where_sql;
  if (column[0] == '\0') {
    ret = OB_ERR_UNEXPECTED;
  } else if (where != nullptr && where->str_value_ != nullptr) {
    where_sql.assign_ptr(where->str_value_, static_cast<int32_t>(where->str_len_));
  }
  if (OB_SUCC(ret)) {
    ret = stmt.set_ai_column_retry(column_name, where_sql);
  }
  return ret;
}

int ObAIColumnService::execute_retry(ObExecContext &ctx, ObAlterTableStmt &stmt)
{
  int ret = OB_SUCCESS;
  UNUSED(ctx);
  uint64_t table_id = stmt.get_alter_table_arg().table_id_;
  if (OB_INVALID_ID == table_id) {
    table_id = stmt.get_alter_table_arg().alter_table_schema_.get_table_id();
  }
  ObSchemaGetterGuard guard;
  const ObTableSchema *schema = nullptr;
  if (OB_FAIL(load_schema(table_id, 0, guard, schema)) || OB_ISNULL(schema)) {
    ret = OB_NOT_SUPPORTED;
    LOG_USER_ERROR(OB_NOT_SUPPORTED, "retrying a column that is not an AI column");
    return ret;
  }
  char db[AI_NAME] = {0};
  char table[AI_NAME] = {0};
  if (OB_FAIL(schema_names(guard, *schema, db, sizeof(db), table, sizeof(table)))) {
    return ret;
  }
  ObMySQLProxy *proxy = GCTX.sql_proxy_;
  if (OB_ISNULL(proxy)) {
    ret = OB_ERR_UNEXPECTED;
    return ret;
  }
  if (0 == ATOMIC_LOAD(&g_meta_ready) && OB_FAIL(ensure_meta_tables())) {
    return ret;
  }
  ObSqlString meta_sql;
  ObMySQLProxy::MySQLResult meta;
  char state_name[160] = {0};
  char request_name[160] = {0};
  char spec_name[160] = {0};
  char user_expr[AI_EXPR] = {0};
  char system_prompt[512] = {0};
  char model[AI_NAME] = {0};
  char kind[16] = {0};
  char result_kind[16] = {0};
  char column_name[AI_NAME] = {0};
  uint64_t column_id = 0;
  int64_t max_retries = 0;
  bool found = false;
  copy_str(column_name, sizeof(column_name), stmt.get_ai_retry_column_name());
  if (OB_FAIL(meta_sql.append_fmt("SELECT column_id, column_name, max_retries, kind, model_name, "
                                  "user_expr, system_prompt, result_kind, state_column_name, "
                                  "request_column_name, result_spec_column_name FROM "
                                  "oceanbase.__all_ai_column WHERE table_id = %lu", table_id))) {
  } else if (OB_FAIL(proxy->read(meta, meta_sql.ptr()))) {
  } else if (OB_ISNULL(meta.get_result())) {
    ret = OB_ERR_UNEXPECTED;
  }
  while (OB_SUCC(ret) && !found && OB_SUCC(meta.get_result()->next())) {
    char name[AI_NAME] = {0};
    if (OB_FAIL(copy_mysql_str(*meta.get_result(), 1, name, sizeof(name)))) {
    } else if (0 == strcasecmp(name, column_name)) {
      int64_t id = 0;
      if (OB_FAIL(meta.get_result()->get_int(static_cast<int64_t>(0), id))) {
      } else if (OB_FAIL(meta.get_result()->get_int(static_cast<int64_t>(2), max_retries))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 3, kind, sizeof(kind)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 4, model, sizeof(model)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 5, user_expr, sizeof(user_expr)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 6, system_prompt, sizeof(system_prompt)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 7, result_kind, sizeof(result_kind)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 8, state_name, sizeof(state_name)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 9, request_name, sizeof(request_name)))) {
      } else if (OB_FAIL(copy_mysql_str(*meta.get_result(), 10, spec_name, sizeof(spec_name)))) {
      } else {
        column_id = static_cast<uint64_t>(id);
        copy_cstr(column_name, sizeof(column_name), name);
        found = true;
      }
    }
  }
  if (OB_ITER_END == ret) {
    ret = OB_SUCCESS;
  }
  if (OB_SUCC(ret) && !found) {
    ret = OB_NOT_SUPPORTED;
    LOG_USER_ERROR(OB_NOT_SUPPORTED, "retrying a column that is not an AI column");
    return ret;
  }
  ObSqlString where;
  const ObString &raw_where = stmt.get_ai_retry_where();
  if (OB_FAIL(ret)) {
  } else if (!raw_where.empty() &&
             OB_FAIL(rewrite_status(raw_where.ptr(), raw_where.length(), state_name, where))) {
  }
  const ObRowkeyInfo &rk = schema->get_rowkey_info();
  ObSqlString select;
  if (OB_FAIL(ret)) {
  } else if (OB_FAIL(select.append("SELECT "))) {
  }
  for (int64_t i = 0; OB_SUCC(ret) && i < rk.get_size(); ++i) {
    uint64_t pk_id = 0;
    const ObColumnSchemaV2 *pk = nullptr;
    if (i > 0 && OB_FAIL(select.append(", "))) {
    } else if (OB_FAIL(rk.get_column_id(i, pk_id))) {
    } else if (OB_ISNULL(pk = schema->get_column_schema(pk_id))) {
      ret = OB_ERR_UNEXPECTED;
    } else if (OB_FAIL(select.append("CAST("))) {
    } else if (OB_FAIL(append_ident(select, pk->get_column_name()))) {
    } else if (OB_FAIL(select.append(" AS CHAR)"))) {
    }
  }
  if (OB_SUCC(ret) && OB_FAIL(select.append(", IFNULL(CAST(("))) {
  } else if (OB_SUCC(ret) && OB_FAIL(select.append(user_expr))) {
  } else if (OB_SUCC(ret) && OB_FAIL(select.append(") AS CHAR), '') FROM "))) {
  } else if (OB_SUCC(ret) && OB_FAIL(append_ident(select, db))) {
  } else if (OB_SUCC(ret) && OB_FAIL(select.append("."))) {
  } else if (OB_SUCC(ret) && OB_FAIL(append_ident(select, table))) {
  } else if (OB_SUCC(ret) && where.length() > 0) {
    if (OB_FAIL(select.append(" WHERE "))) {
    } else if (OB_FAIL(select.append(where.ptr()))) {
    }
  }
  struct Hit {
    char pred[1024];
    char prompt[AI_PROMPT];
  };
  Hit hits[128];
  int nhits = 0;
  if (OB_SUCC(ret)) {
    AiTlsGuard guard(true);
    ObMySQLProxy::MySQLResult rows;
    if (OB_FAIL(proxy->read(rows, select.ptr()))) {
      LOG_WARN("retry select failed", K(ret), K(select));
    } else if (OB_ISNULL(rows.get_result())) {
      ret = OB_ERR_UNEXPECTED;
    }
    while (OB_SUCC(ret) && nhits < 128 && OB_SUCC(rows.get_result()->next())) {
      ObSqlString pred;
      for (int64_t i = 0; OB_SUCC(ret) && i < rk.get_size(); ++i) {
        uint64_t pk_id = 0;
        const ObColumnSchemaV2 *pk = nullptr;
        char value[1024] = {0};
        if (OB_FAIL(rk.get_column_id(i, pk_id))) {
        } else if (OB_ISNULL(pk = schema->get_column_schema(pk_id))) {
          ret = OB_ERR_UNEXPECTED;
        } else if (OB_FAIL(copy_mysql_str(*rows.get_result(), i, value, sizeof(value)))) {
        } else if (i > 0 && OB_FAIL(pred.append(" AND "))) {
        } else if (OB_FAIL(append_ident(pred, pk->get_column_name()))) {
        } else if (OB_FAIL(pred.append(" = "))) {
        } else if (ob_is_string_tc(pk->get_data_type()) || ob_is_text_tc(pk->get_data_type())) {
          ret = append_quoted(pred, value);
        } else if (value[0] == '\0') {
          ret = pred.append("NULL");
        } else {
          ret = pred.append(value);
        }
      }
      if (OB_SUCC(ret)) {
        copy_cstr(hits[nhits].pred, sizeof(hits[nhits].pred), pred.ptr());
        ret = copy_mysql_str(*rows.get_result(), rk.get_size(), hits[nhits].prompt, AI_PROMPT);
        if (OB_SUCC(ret)) {
          ++nhits;
        }
      }
    }
    if (OB_ITER_END == ret) {
      ret = OB_SUCCESS;
    }
  }
  if (OB_FAIL(ret) || 0 == nhits) {
    return ret;
  }
  AiTlsGuard tls(true);
  ObMySQLTransaction trans;
  bool started = false;
  int64_t affected = 0;
  if (OB_FAIL(trans.start(proxy))) {
  } else {
    started = true;
  }
  for (int i = 0; OB_SUCC(ret) && i < nhits; ++i) {
    char request_id[80] = {0};
    ObSqlString upd;
    ObSqlString del;
    ObSqlString ins;
    if (OB_FAIL(make_request_id(request_id, sizeof(request_id)))) {
    } else if (OB_FAIL(upd.append("UPDATE "))) {
    } else if (OB_FAIL(append_ident(upd, db))) {
    } else if (OB_FAIL(upd.append("."))) {
    } else if (OB_FAIL(append_ident(upd, table))) {
    } else if (OB_FAIL(upd.append(" SET "))) {
    } else if (OB_FAIL(append_ident(upd, column_name))) {
    } else if (OB_FAIL(upd.append(" = NULL, "))) {
    } else if (OB_FAIL(append_ident(upd, state_name))) {
    } else if (OB_FAIL(upd.append(" = 'PENDING', "))) {
    } else if (OB_FAIL(append_ident(upd, request_name))) {
    } else if (OB_FAIL(upd.append(" = "))) {
    } else if (OB_FAIL(append_quoted(upd, request_id))) {
    } else if (OB_FAIL(upd.append(", "))) {
    } else if (OB_FAIL(append_ident(upd, spec_name))) {
    } else if (OB_FAIL(upd.append(" = "))) {
    } else if (OB_FAIL(append_quoted(upd, hits[i].prompt))) {
    } else if (OB_FAIL(upd.append(" WHERE "))) {
    } else if (OB_FAIL(upd.append(hits[i].pred))) {
    } else if (OB_FAIL(trans.write(upd.ptr(), affected))) {
    } else if (OB_FAIL(del.append_fmt(
                   "DELETE FROM oceanbase.__all_ai_column_task WHERE table_id = %lu AND column_id = %lu AND rowkey_predicate = ",
                   table_id, column_id))) {
    } else if (OB_FAIL(append_quoted(del, hits[i].pred))) {
    } else if (OB_FAIL(trans.write(del.ptr(), affected))) {
    } else if (OB_FAIL(ins.append(
                   "INSERT INTO oceanbase.__all_ai_column_task (table_id, column_id, rowkey_predicate, "
                   "request_id, eligible_ts, state, retry_count, prompt_text, system_prompt, model_name, "
                   "kind, max_retries, column_name, state_column_name, request_column_name, result_kind, "
                   "database_name, table_name) VALUES ("))) {
    } else if (OB_FAIL(ins.append_fmt("%lu, %lu, ", table_id, column_id))) {
    } else if (OB_FAIL(append_quoted(ins, hits[i].pred))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, request_id))) {
    } else if (OB_FAIL(ins.append(", NOW(6), 0, 0, "))) {
    } else if (OB_FAIL(append_quoted(ins, hits[i].prompt))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, system_prompt))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, model))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, kind))) {
    } else if (OB_FAIL(ins.append_fmt(", %ld, ", max_retries))) {
    } else if (OB_FAIL(append_quoted(ins, column_name))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, state_name))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, request_name))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, result_kind))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, db))) {
    } else if (OB_FAIL(ins.append(", "))) {
    } else if (OB_FAIL(append_quoted(ins, table))) {
    } else if (OB_FAIL(ins.append(")"))) {
    } else if (OB_FAIL(trans.write(ins.ptr(), affected))) {
    }
  }
  if (started) {
    const int end_ret = trans.end(OB_SUCCESS == ret);
    if (OB_SUCCESS == ret) {
      ret = end_ret;
    }
  }
  if (OB_SUCC(ret)) {
    ret = worker().ensure_started();
  }
  return ret;
}

int ObAIColumnService::process_tasks()
{
  int ret = OB_SUCCESS;
  if (0 == ATOMIC_LOAD(&g_meta_ready)) {
    return ret;
  }
  ObMySQLProxy *proxy = GCTX.sql_proxy_;
  if (OB_ISNULL(proxy)) {
    return ret;
  }
  AiTlsGuard guard(true);
  TaskRow *rows = static_cast<TaskRow *>(ob_malloc(sizeof(TaskRow) * 64, "AiColTask"));
  if (OB_ISNULL(rows)) {
    return OB_ALLOCATE_MEMORY_FAILED;
  }
  memset(rows, 0, sizeof(TaskRow) * 64);
  int64_t nrows = 0;
  {
    ObMySQLProxy::MySQLResult res;
    if (OB_FAIL(proxy->read(
            res, "SELECT table_id, column_id, rowkey_predicate, request_id, retry_count, "
                 "prompt_text, system_prompt, model_name, kind, max_retries, column_name, "
                 "state_column_name, request_column_name, result_kind, database_name, table_name "
                 "FROM oceanbase.__all_ai_column_task WHERE eligible_ts <= NOW(6) "
                 "ORDER BY table_id, rowkey_predicate, request_id, column_id LIMIT 64"))) {
      if (missing_table(ret)) {
        ret = OB_SUCCESS;
      }
    } else if (OB_NOT_NULL(res.get_result())) {
      while (OB_SUCC(ret) && nrows < 64 && OB_SUCC(res.get_result()->next())) {
        TaskRow &row = rows[nrows];
        int64_t table_id = 0;
        int64_t column_id = 0;
        if (OB_FAIL(res.get_result()->get_int(static_cast<int64_t>(0), table_id))) {
        } else if (OB_FAIL(res.get_result()->get_int(static_cast<int64_t>(1), column_id))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 2, row.predicate, sizeof(row.predicate)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 3, row.request_id, sizeof(row.request_id)))) {
        } else if (OB_FAIL(res.get_result()->get_int(static_cast<int64_t>(4), row.retry_count))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 5, row.prompt, sizeof(row.prompt)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 6, row.system_prompt, sizeof(row.system_prompt)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 7, row.model, sizeof(row.model)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 8, row.kind, sizeof(row.kind)))) {
        } else if (OB_FAIL(res.get_result()->get_int(static_cast<int64_t>(9), row.max_retries))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 10, row.column_name, sizeof(row.column_name)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 11, row.state_name, sizeof(row.state_name)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 12, row.request_name, sizeof(row.request_name)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 13, row.result_kind, sizeof(row.result_kind)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 14, row.db, sizeof(row.db)))) {
        } else if (OB_FAIL(copy_mysql_str(*res.get_result(), 15, row.table, sizeof(row.table)))) {
        } else {
          row.table_id = static_cast<uint64_t>(table_id);
          row.column_id = static_cast<uint64_t>(column_id);
          ++nrows;
        }
      }
      if (OB_ITER_END == ret) {
        ret = OB_SUCCESS;
      }
    }
  }
  for (int64_t i = 0; OB_SUCC(ret) && i < nrows;) {
    int64_t j = i + 1;
    while (j < nrows && rows[j].table_id == rows[i].table_id &&
           0 == strcmp(rows[j].predicate, rows[i].predicate) &&
           0 == strcmp(rows[j].request_id, rows[i].request_id)) {
      ++j;
    }
    bool ok = true;
    for (int64_t k = i; k < j; ++k) {
      const int http_ret = call_model(rows[k]);
      if (OB_SUCCESS != http_ret || !rows[k].http_ok) {
        ok = false;
        LOG_WARN("ai column http call failed", K(http_ret), K(rows[k].model), K(rows[k].kind));
      }
    }
    if (OB_FAIL(publish_group(*proxy, rows, i, j, ok))) {
      LOG_WARN("publish ai column group failed", K(ret));
      ret = OB_SUCCESS;
    }
    i = j;
  }
  ob_free(rows);
  return ret;
}

} // namespace sql
} // namespace oceanbase
