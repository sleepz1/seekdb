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

#pragma once

#include "lib/container/ob_iarray.h"
#include "lib/string/ob_string.h"
#include "share/schema/ob_column_schema.h"
#include "share/schema/ob_table_schema.h"
#include "sql/parser/parse_node.h"

namespace oceanbase
{
namespace sql
{

class ObAlterTableStmt;
class ObExecContext;
class ObSQLSessionInfo;
class ObDMLStmt;
class ObPhysicalPlan;
class ObTableModifyOp;

class ObAIColumnService
{
public:
  static int define_column(share::schema::ObColumnSchemaV2 &column, const ParseNode *node);
  static int fill_hidden_column(share::schema::ObColumnSchemaV2 &column,
                                const common::ObString &ai_name,
                                const char *prefix,
                                int32_t data_length);
  static bool is_ai_column(const share::schema::ObColumnSchemaV2 &column);
  static bool is_aux_column_name(const common::ObString &name);
  static bool allow_hidden_access();
  static int check_create_dependencies(
      const common::ObIArray<share::schema::ObColumnSchemaV2> &columns);
  static int check_alter_dependency(const share::schema::ObTableSchema &existing,
                                    const share::schema::ObTableSchema &added,
                                    const share::schema::ObColumnSchemaV2 &column);
  static int register_after_ddl(uint64_t table_id, int64_t schema_version);
  static int capture_row(ObTableModifyOp &op);
  static int finish_dml(ObTableModifyOp &op);
  static int collect_referenced_columns(const ObDMLStmt &stmt, ObPhysicalPlan &plan);
  static int check_strict_read(ObSQLSessionInfo &session, const ObPhysicalPlan *plan);
  static int prepare_retry(ObAlterTableStmt &stmt, const ParseNode &action);
  static int execute_retry(ObExecContext &ctx, ObAlterTableStmt &stmt);
  static int process_tasks();
};

} // namespace sql
} // namespace oceanbase
