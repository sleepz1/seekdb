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

#include "sql/engine/basic/ob_values_table_access_op.h"
#include "sql/engine/basic/file_sql_scan.h"
#include "sql/engine/ob_exec_context.h"
#include "sql/engine/expr/ob_obj_cast_runtime.h"
#include "sql/resolver/ob_resolver_utils.h"
#include "common/timezone/ob_time_convert.h"

namespace oceanbase
{
using namespace common;
namespace sql
{
OB_SERIALIZE_MEMBER_INHERIT(ObValuesTableAccessSpec,
                            ObOpSpec,
                            access_type_,
                            column_exprs_,
                            value_exprs_,
                            start_param_idx_,
                            end_param_idx_,
                            obj_params_);

ObValuesTableAccessOp::ObValuesTableAccessOp(ObExecContext &exec_ctx,
                                             const ObOpSpec &spec,
                                             ObOpInput *input)
  : ObOperator(exec_ctx, spec, input), datum_caster_(), cm_(CM_NONE),
    row_idx_(0), row_cnt_(0), file_reader_(NULL) {}

int ObValuesTableAccessOp::inner_open()
{
  int ret = OB_SUCCESS;
  row_idx_ = 0;
  const int32_t result_flag = 0;
  ObPhysicalPlanCtx *plan_ctx = GET_PHY_PLAN_CTX(ctx_);
  if (OB_ISNULL(plan_ctx) || OB_ISNULL(ctx_.get_sql_ctx())) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_UNLIKELY(MY_SPEC.column_exprs_.empty())) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_FAIL(datum_caster_.init(eval_ctx_.exec_ctx_))) {
  } else if (OB_FAIL(ObSQLUtils::get_default_cast_mode(false, 0, GET_MY_SESSION(ctx_), cm_))) {
  } else {
    cm_ = cm_ | CM_COLUMN_CONVERT;
    if (OB_SUCC(ret)) {
      if (ObValuesTableDef::ACCESS_FILE == MY_SPEC.access_type_) {
        if (OB_FAIL(open_file_reader())) {
        } else {
          row_cnt_ = INT64_MAX;
        }
      } else if (ObValuesTableDef::FOLD_ACCESS_EXPR == MY_SPEC.access_type_) {
        if (OB_UNLIKELY(MY_SPEC.start_param_idx_ < 0 ||
                        MY_SPEC.start_param_idx_ > MY_SPEC.end_param_idx_ ||
                        MY_SPEC.end_param_idx_ >= plan_ctx->get_param_store().count())) {
          ret = OB_ERR_UNEXPECTED;
        } else {
          const ObObj &first_column_node = plan_ctx->get_param_store().at(MY_SPEC.start_param_idx_);
          if (OB_FAIL(first_column_node.get_real_param_count(row_cnt_))) {
          }
        }
      } else if (ObValuesTableDef::ACCESS_PARAM == MY_SPEC.access_type_) {
        int64_t column_cnt = MY_SPEC.column_exprs_.count();
        row_cnt_ = (MY_SPEC.end_param_idx_ - MY_SPEC.start_param_idx_ + 1) / column_cnt;
      } else if (ObValuesTableDef::ACCESS_OBJ == MY_SPEC.access_type_) {
        int64_t column_cnt = MY_SPEC.column_exprs_.count();
        row_cnt_ = MY_SPEC.obj_params_.count() / column_cnt;
      } else {
        int64_t column_cnt = MY_SPEC.column_exprs_.count();
        row_cnt_ = MY_SPEC.value_exprs_.count() / column_cnt;
      }
    }
  }
  return ret;
}

int ObValuesTableAccessOp::inner_rescan()
{
  int ret = OB_SUCCESS;
  if (OB_FAIL(ObOperator::inner_rescan())) {
  } else {
    row_idx_ = 0;
    if (ObValuesTableDef::ACCESS_FILE == MY_SPEC.access_type_ && OB_FAIL(rewind_file_reader())) {
    }
  }
  return ret;
}

int ObValuesTableAccessOp::switch_iterator()
{
  int ret = OB_SUCCESS;
  if (OB_FAIL(ObOperator::inner_switch_iterator())) {
  } else {
    row_idx_ = 0;
    if (ObValuesTableDef::ACCESS_FILE == MY_SPEC.access_type_ && OB_FAIL(rewind_file_reader())) {
    }
  }
  return ret;
}

int ObValuesTableAccessOp::inner_get_next_row()
{
  int ret = OB_SUCCESS;
  if (OB_FAIL(try_check_status())) {
  } else {
    do {
      clear_evaluated_flag();
      if (OB_FAIL(calc_next_row())) {
        if(OB_ITER_END != ret) {
        }
      } else {
        LOG_DEBUG("output row", "row", ROWEXPR2STR(eval_ctx_, MY_SPEC.output_));
      }
      break;
    } while (OB_SUCC(ret));
  }
  return ret;
}

void ObValuesTableAccessOp::update_src_meta(const ObObjMeta &src_obj_meta,
                                            const ObAccuracy &src_obj_acc,
                                            ObDatumMeta &src_meta)
{
  src_meta.type_ = src_obj_meta.get_type();
  src_meta.cs_type_ = src_obj_meta.get_collation_type();
  src_meta.scale_ = src_obj_acc.get_scale();
  src_meta.precision_ = src_obj_acc.get_precision();
}

int ObValuesTableAccessOp::get_real_src_obj_type(const int64_t row_idx,
                                                 ObExpr &src_expr,
                                                 ObDatumMeta &src_meta,
                                                 ObObjMeta &src_obj_meta)
{
  int ret = OB_SUCCESS;
  if (T_QUESTIONMARK == src_expr.type_) {
    int64_t param_idx = src_expr.extra_;
    ObPhysicalPlanCtx *plan_ctx = GET_PHY_PLAN_CTX(ctx_);
    const ObSqlArrayObj *array_obj = NULL;
    if (OB_UNLIKELY(param_idx < 0 || param_idx >= plan_ctx->get_param_store().count())) {
      ret = OB_INVALID_ARGUMENT;
    } else if (plan_ctx->get_param_store().at(param_idx).is_ext_sql_array()) {
      // If the parameter is is_ext_sql_array
      if (OB_ISNULL(array_obj = reinterpret_cast<const ObSqlArrayObj*>(
                                            plan_ctx->get_param_store().at(param_idx).get_ext()))) {
        ret = OB_ERR_UNEXPECTED;
      } else if (array_obj->count_ <= row_idx) {
        ret = OB_ERR_UNEXPECTED;
      } else {
        src_obj_meta = array_obj->data_[row_idx].meta_;
        const ObAccuracy &src_obj_acc = array_obj->data_[row_idx].get_accuracy();
        update_src_meta(src_obj_meta, src_obj_acc, src_meta);
      }
    } else if (src_expr.frame_idx_ < spec_.plan_->get_expr_frame_info().const_frame_.count() +
                                     spec_.plan_->get_expr_frame_info().param_frame_.count()) {
      src_obj_meta = plan_ctx->get_param_store().at(param_idx).meta_;
      const ObAccuracy &src_obj_acc = plan_ctx->get_param_store().at(param_idx).get_accuracy();
      update_src_meta(src_obj_meta, src_obj_acc, src_meta);
    }
  }
  return ret;
}

int ObValuesTableAccessOp::calc_datum_from_expr(const int64_t col_idx,
                                                const int64_t row_idx,
                                                ObExpr *src_expr,
                                                ObExpr *dst_expr)
{
  int ret = OB_SUCCESS;
  ObDatumMeta src_meta = src_expr->datum_meta_;
  ObObjMeta src_obj_meta = src_expr->obj_meta_;
  ObDatum *datum = NULL;
  bool need_adjust_decimal_int = false;
  if (src_expr == dst_expr) {
    /* actually should not happened */
  } else if (T_QUESTIONMARK == src_expr->type_ &&
             OB_FAIL(get_real_src_obj_type(row_idx, *src_expr, src_meta, src_obj_meta))) {
    LOG_WARN("failed to get real obj type");
  } else {
    need_adjust_decimal_int = src_meta.type_ == ObDecimalIntType &&
                              dst_expr->datum_meta_.type_ == ObDecimalIntType &&
                              ObDatumCast::need_scale_decimalint(src_meta.scale_,
                                                                 src_meta.precision_,
                                                                 dst_expr->datum_meta_.scale_,
                                                                 dst_expr->datum_meta_.precision_);
  }
  if (OB_FAIL(ret)) {
  } else if (src_meta.type_ == dst_expr->datum_meta_.type_ &&
             src_meta.cs_type_ == dst_expr->datum_meta_.cs_type_ &&
             !need_adjust_decimal_int) {
    // when type and collation_type is same. src has the same type to dst;
    if (OB_FAIL(src_expr->eval(eval_ctx_, datum))) {
    } else {
      dst_expr->locate_datum_for_write(eval_ctx_) = *datum;
      dst_expr->set_evaluated_projected(eval_ctx_);
    }
  } else {
    if (OB_FAIL(ObCharset::check_valid_implicit_convert(src_meta.cs_type_,
                                                        dst_expr->datum_meta_.cs_type_))) {
    } else {
      ObExpr real_src_expr = *src_expr;
      real_src_expr.datum_meta_ = src_meta;
      real_src_expr.obj_meta_ = src_obj_meta;
      real_src_expr.obj_datum_map_ = ObDatum::get_obj_datum_map_type(src_meta.type_);
      if (OB_FAIL(datum_caster_.to_type(dst_expr->datum_meta_, real_src_expr, cm_, datum))) {
      } else {
        ObDatum &dst_datum = dst_expr->locate_datum_for_write(eval_ctx_);
        if (ObObjDatumMapType::OBJ_DATUM_STRING == dst_expr->obj_datum_map_) {
          ObExprStrResAlloc res_alloc(*dst_expr, eval_ctx_);
          if (OB_FAIL(dst_datum.deep_copy(*datum, res_alloc))) {
          }
        } else {
          ObDataBuffer res_alloc(const_cast<char*>(dst_datum.ptr_), dst_expr->res_buf_len_);
          if (OB_FAIL(dst_datum.deep_copy(*datum, res_alloc))) {
          }
        }
        dst_expr->set_evaluated_projected(eval_ctx_);
      }
    }
  }
  return ret;
}

int ObValuesTableAccessOp::calc_datum_from_param(const ObObj &src_obj, ObExpr *dst_expr)
{
  int ret = OB_SUCCESS;
  ObDatum &dst_datum = dst_expr->locate_datum_for_write(eval_ctx_);
  const ObObjType &src_type = src_obj.get_type();
  const ObObjType &dst_type =  dst_expr->obj_meta_.get_type();
  const ObObjParam src_obj_param = static_cast<const ObObjParam>(src_obj);
  bool need_adjust_decimal_int = ob_is_decimal_int_tc(src_type)
                                 && ob_is_decimal_int_tc(dst_expr->datum_meta_.type_)
                                 && ObDatumCast::need_scale_decimalint(src_obj_param.get_scale(),
                                                                       src_obj_param.get_precision(),
                                                                       dst_expr->datum_meta_.scale_,
                                                                       dst_expr->datum_meta_.precision_);
  if (ob_is_decimal_int_tc(src_type) && (ob_is_number_tc(dst_type) || need_adjust_decimal_int)) {
    ObObj dst_obj;
    const ObDataTypeCastParams dtc_params = ObBasicSessionInfo::create_dtc_params(GET_MY_SESSION(ctx_));
    ObAccuracy dst_accuracy = ObAccuracy::DDL_DEFAULT_ACCURACY2[0][dst_type];
    if (ob_is_decimal_int_tc(dst_type)) {
      dst_accuracy.set_scale(dst_expr->datum_meta_.scale_);
      dst_accuracy.set_precision(dst_expr->datum_meta_.precision_);
    }
    ObCastCtx cast_ctx(&eval_ctx_.exec_ctx_.get_allocator(), &dtc_params, cm_, dst_expr->obj_meta_.get_collation_type(), &dst_accuracy);
    ObSqlObjCastRuntime cast_runtime(&eval_ctx_.exec_ctx_);
    cast_ctx.runtime_ = &cast_runtime;
    if (OB_FAIL(ObObjCaster::to_type(dst_type, dst_expr->obj_meta_.get_collation_type(), cast_ctx,
                                     src_obj, dst_obj))) {
    } else if (OB_FAIL(dst_datum.from_obj(dst_obj))) {
    } else {
      dst_expr->set_evaluated_projected(eval_ctx_);
    }
  } else if (OB_FAIL(dst_datum.from_obj(src_obj))) {
  } else {
    dst_expr->set_evaluated_projected(eval_ctx_);
  }
  return ret;
}

OB_INLINE int ObValuesTableAccessOp::calc_next_row()
{
  int ret = OB_SUCCESS;
  if (ObValuesTableDef::ACCESS_FILE == MY_SPEC.access_type_) {
    return calc_next_file_row();
  }
  int64_t col_num = MY_SPEC.column_exprs_.count();
  int64_t col_idx = 0;
  ObPhysicalPlanCtx *plan_ctx = GET_PHY_PLAN_CTX(ctx_);
  if (row_idx_ == row_cnt_) {
    ret = OB_ITER_END;
  } else if (ObValuesTableDef::FOLD_ACCESS_EXPR == MY_SPEC.access_type_ &&
             OB_FAIL(plan_ctx->replace_batch_param_datum(row_idx_,
                                          MY_SPEC.start_param_idx_,
                                          MY_SPEC.end_param_idx_ - MY_SPEC.start_param_idx_ + 1))) {
  } else {
    while (OB_SUCC(ret) && col_idx < col_num) {
      ObExpr *col_expr = MY_SPEC.column_exprs_.at(col_idx);
      switch (MY_SPEC.access_type_)
      {
        case ObValuesTableDef::ACCESS_EXPR : {
          int64_t idx = row_idx_ * col_num + col_idx;
          ObExpr *src_expr = MY_SPEC.value_exprs_.at(idx);
          if (OB_FAIL(calc_datum_from_expr(col_idx, row_idx_, src_expr, col_expr))) {
          }
          break;
        }
        case ObValuesTableDef::FOLD_ACCESS_EXPR : {
          ObExpr *src_expr = MY_SPEC.value_exprs_.at(col_idx);
          if (OB_FAIL(calc_datum_from_expr(col_idx, row_idx_, src_expr, col_expr))) {
          }
          break;
        }
        case ObValuesTableDef::ACCESS_PARAM: {
          int64_t param_idx = MY_SPEC.start_param_idx_ + col_idx + row_idx_ * col_num;
          const ObObjParam &param = plan_ctx->get_param_store().at(param_idx);
          if (OB_FAIL(calc_datum_from_param(param, col_expr))) {
          }
          break;
        }
        case ObValuesTableDef::ACCESS_OBJ: {
          int64_t idx = row_idx_ * col_num + col_idx;
          const ObObj &param = MY_SPEC.obj_params_.at(idx);
          if (OB_FAIL(calc_datum_from_param(param, col_expr))) {
          }
          break;
        }
        case ObValuesTableDef::ACCESS_FILE: {
          ret = OB_ERR_UNEXPECTED;
          break;
        }
      };
      col_idx++;
    } // while end

    for (int64_t i = 0; OB_SUCC(ret) && i < MY_SPEC.output_.count(); i++) {
      ObExpr *output = MY_SPEC.output_.at(i);
      if (i < col_num && output == MY_SPEC.column_exprs_.at(i)) {
        /* need do nothing */
      } else {
        ObDatum *dst_datum = NULL;
        MY_SPEC.output_.at(i)->eval(eval_ctx_, dst_datum);
      }
    }
    if (OB_SUCC(ret)) {
      ++row_idx_;
    }
  }
  return ret;
}

static int file_sql_status_to_ret(const filesql::Status &st)
{
  int ret = OB_SUCCESS;
  if (st.ok()) {
  } else if (filesql::ST_UNSUPPORTED == st.code) {
    ret = OB_NOT_SUPPORTED;
    LOG_USER_ERROR(OB_NOT_SUPPORTED, st.message.empty() ? "file sql" : st.message.c_str());
  } else if (filesql::ST_CHANGED == st.code || st.message == "FILE_CHANGED") {
    ret = OB_ERR_UNEXPECTED;
    LOG_USER_ERROR(OB_ERR_UNEXPECTED, "FILE_CHANGED");
  } else if (st.message.find("secure_file_priv") != std::string::npos ||
             st.message.find("..") != std::string::npos) {
    ret = OB_ERR_NO_PRIVILEGE;
    LOG_USER_ERROR(OB_ERR_NO_PRIVILEGE, "FILE");
  } else if (filesql::ST_IO == st.code &&
             st.message.find("not a regular") == std::string::npos &&
             st.message.find("not a directory") == std::string::npos) {
    ret = OB_FILE_NOT_EXIST;
  } else {
    ret = OB_ERR_UNEXPECTED;
    LOG_USER_ERROR(OB_ERR_UNEXPECTED, st.message.empty() ? "file sql" : st.message.c_str());
  }
  return ret;
}

int ObValuesTableAccessOp::inner_close()
{
  int ret = OB_SUCCESS;
  row_idx_ = 0;
  if (NULL != file_reader_) {
    filesql::Status st = file_reader_->check_unchanged();
    delete file_reader_;
    file_reader_ = NULL;
    if (!st.ok()) {
      ret = file_sql_status_to_ret(st);
    }
  }
  int caster_ret = datum_caster_.destroy();
  if (OB_SUCC(ret)) {
    ret = caster_ret;
  }
  return ret;
}

void ObValuesTableAccessOp::destroy()
{
  if (NULL != file_reader_) {
    delete file_reader_;
    file_reader_ = NULL;
  }
  ObOperator::destroy();
}

int ObValuesTableAccessOp::column_is_needed(const ObExpr *col, bool &needed)
{
  int ret = OB_SUCCESS;
  needed = false;
  if (OB_ISNULL(col)) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    const ExprFixedArray *lists[] = {
      &MY_SPEC.output_, &MY_SPEC.filters_, &MY_SPEC.startup_filters_, &MY_SPEC.calc_exprs_
    };
    for (int64_t li = 0; OB_SUCC(ret) && !needed && li < 4; ++li) {
      const ExprFixedArray &list = *lists[li];
      for (int64_t i = 0; OB_SUCC(ret) && !needed && i < list.count(); ++i) {
        bool found = false;
        if (OB_ISNULL(list.at(i))) {
        } else if (OB_FAIL(list.at(i)->contain_expr(col, found))) {
        } else {
          needed = found;
        }
      }
    }
  }
  return ret;
}

int ObValuesTableAccessOp::open_file_reader()
{
  int ret = OB_SUCCESS;
  if (NULL != file_reader_) {
    delete file_reader_;
    file_reader_ = NULL;
  }
  ObSQLSessionInfo *session = GET_MY_SESSION(ctx_);
  if (MY_SPEC.obj_params_.count() < filesql::META_SLOTS) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_ISNULL(session)) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    const ObObj &path_obj = MY_SPEC.obj_params_.at(filesql::META_PATH);
    ObString path = path_obj.get_varchar();
    filesql::Kind kind = static_cast<filesql::Kind>(MY_SPEC.obj_params_.at(filesql::META_KIND).get_int());
    filesql::Options opt;
    opt.header = 0 != MY_SPEC.obj_params_.at(filesql::META_HEADER).get_int();
    opt.sample_size = MY_SPEC.obj_params_.at(filesql::META_SAMPLE_SIZE).get_int();
    opt.all_varchar = 0 != MY_SPEC.obj_params_.at(filesql::META_ALL_VARCHAR).get_int();
    ObString delim = MY_SPEC.obj_params_.at(filesql::META_DELIM).get_varchar();
    ObString nullstr = MY_SPEC.obj_params_.at(filesql::META_NULLSTR).get_varchar();
    ObString quote = MY_SPEC.obj_params_.at(filesql::META_QUOTE).get_varchar();
    ObString format = MY_SPEC.obj_params_.at(filesql::META_FORMAT).get_varchar();
    if (1 == delim.length() && NULL != delim.ptr()) {
      opt.delim = delim.ptr()[0];
    }
    if (1 == quote.length() && NULL != quote.ptr()) {
      opt.quote = quote.ptr()[0];
    }
    if (nullstr.length() > 0 && NULL != nullstr.ptr()) {
      opt.nullstr.assign(nullstr.ptr(), static_cast<size_t>(nullstr.length()));
    }
    if (format.length() > 0 && NULL != format.ptr()) {
      opt.format.assign(format.ptr(), static_cast<size_t>(format.length()));
    }
    ObString secure;
    if (OB_FAIL(session->get_secure_file_priv(secure))) {
    } else if (secure.empty() || 0 == secure.case_compare("NULL")) {
      ret = OB_ERR_NO_PRIVILEGE;
      LOG_USER_ERROR(OB_ERR_NO_PRIVILEGE, "FILE");
    } else if (OB_FAIL(ObResolverUtils::check_secure_path(secure, path))) {
      LOG_USER_ERROR(OB_ERR_NO_PRIVILEGE, "FILE");
    } else if (path.empty() || NULL == path.ptr()) {
      ret = OB_ERR_UNEXPECTED;
    } else {
      opt.secure_root.assign(secure.ptr(), static_cast<size_t>(secure.length()));
      std::string path_s(path.ptr(), static_cast<size_t>(path.length()));
      file_reader_ = new (std::nothrow) filesql::FileSqlReader();
      if (NULL == file_reader_) {
        ret = OB_ALLOCATE_MEMORY_FAILED;
      } else {
        filesql::Status st = file_reader_->open(kind, path_s, opt);
        if (!st.ok()) {
          ret = file_sql_status_to_ret(st);
        } else {
          const filesql::Fingerprint &fp = file_reader_->schema().fp;
          uint64_t dev = static_cast<uint64_t>(MY_SPEC.obj_params_.at(filesql::META_DEV).get_int());
          uint64_t ino = static_cast<uint64_t>(MY_SPEC.obj_params_.at(filesql::META_INO).get_int());
          uint64_t size = static_cast<uint64_t>(MY_SPEC.obj_params_.at(filesql::META_SIZE).get_int());
          int64_t mtime = MY_SPEC.obj_params_.at(filesql::META_MTIME_NS).get_int();
          if (fp.dev != dev || fp.ino != ino || fp.size != size || fp.mtime_ns != mtime) {
            ret = OB_ERR_UNEXPECTED;
            LOG_USER_ERROR(OB_ERR_UNEXPECTED, "FILE_CHANGED");
          }
        }
      }
    }
    if (OB_SUCC(ret)) {
      std::vector<char> needed(static_cast<size_t>(MY_SPEC.column_exprs_.count()), 0);
      for (int64_t i = 0; OB_SUCC(ret) && i < MY_SPEC.column_exprs_.count(); ++i) {
        bool use = false;
        if (OB_FAIL(column_is_needed(MY_SPEC.column_exprs_.at(i), use))) {
        } else {
          needed[static_cast<size_t>(i)] = use ? 1 : 0;
        }
      }
      if (OB_SUCC(ret) && NULL != file_reader_) {
        file_reader_->set_needed(needed);
      }
    }
    if (OB_FAIL(ret) && NULL != file_reader_) {
      delete file_reader_;
      file_reader_ = NULL;
    }
  }
  return ret;
}

int ObValuesTableAccessOp::rewind_file_reader()
{
  int ret = OB_SUCCESS;
  if (NULL == file_reader_) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    filesql::Status st = file_reader_->check_unchanged();
    if (!st.ok()) {
      ret = file_sql_status_to_ret(st);
    } else {
      st = file_reader_->rewind();
      if (!st.ok()) {
        ret = file_sql_status_to_ret(st);
      }
    }
  }
  return ret;
}

static int fill_file_cell(ObEvalCtx &eval_ctx,
                          ObSQLSessionInfo *session,
                          const filesql::Cell &cell,
                          ObExpr *expr)
{
  int ret = OB_SUCCESS;
  if (OB_ISNULL(expr) || OB_ISNULL(session)) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    ObDatum &datum = expr->locate_datum_for_write(eval_ctx);
    if (cell.is_null) {
      datum.set_null();
    } else {
      switch (expr->datum_meta_.type_) {
        case ObIntType: {
          datum.set_int(cell.i64);
          break;
        }
        case ObTinyIntType: {
          datum.set_bool(0 != cell.i64);
          break;
        }
        case ObDoubleType: {
          datum.set_double(cell.f64);
          break;
        }
        case ObFloatType: {
          datum.set_float(static_cast<float>(cell.f64));
          break;
        }
        case ObDateType: {
          int32_t value = 0;
          ObString text(static_cast<int32_t>(cell.text.size()), cell.text.data());
          if (OB_FAIL(ObTimeConverter::str_to_date(text, value))) {
          } else {
            datum.set_date(value);
          }
          break;
        }
        case ObMySQLDateType: {
          ObMySQLDate value;
          ObString text(static_cast<int32_t>(cell.text.size()), cell.text.data());
          if (OB_FAIL(ObTimeConverter::str_to_mdate(text, value))) {
          } else {
            datum.set_mysql_date(value);
          }
          break;
        }
        case ObDateTimeType: {
          int64_t value = 0;
          ObString text(static_cast<int32_t>(cell.text.size()), cell.text.data());
          ObTimeConvertCtx cvrt_ctx(session->get_timezone_info(), false);
          if (OB_FAIL(ObTimeConverter::str_to_datetime(text, cvrt_ctx, value))) {
          } else {
            datum.set_datetime(value);
          }
          break;
        }
        case ObMySQLDateTimeType: {
          ObMySQLDateTime value;
          ObString text(static_cast<int32_t>(cell.text.size()), cell.text.data());
          ObTimeConvertCtx cvrt_ctx(session->get_timezone_info(), false);
          if (OB_FAIL(ObTimeConverter::str_to_mdatetime(text, cvrt_ctx, value))) {
          } else {
            datum.set_mysql_datetime(value);
          }
          break;
        }
        default: {
          int64_t len = static_cast<int64_t>(cell.text.size());
          char *buf = expr->get_str_res_mem(eval_ctx, len);
          if (len > 0 && OB_ISNULL(buf)) {
            ret = OB_ALLOCATE_MEMORY_FAILED;
          } else {
            if (len > 0) {
              MEMCPY(buf, cell.text.data(), static_cast<size_t>(len));
            }
            datum.set_string(buf, static_cast<uint32_t>(len));
          }
          break;
        }
      }
    }
    if (OB_SUCC(ret)) {
      expr->set_evaluated_projected(eval_ctx);
    }
  }
  return ret;
}

int ObValuesTableAccessOp::calc_next_file_row()
{
  int ret = OB_SUCCESS;
  if (NULL == file_reader_) {
    ret = OB_ERR_UNEXPECTED;
  } else {
    filesql::Status st = file_reader_->check_unchanged();
    std::vector<filesql::Cell> row;
    if (!st.ok()) {
      ret = file_sql_status_to_ret(st);
    } else {
      st = file_reader_->next(row);
      if (st.end()) {
        filesql::Status tail = file_reader_->check_unchanged();
        ret = tail.ok() ? OB_ITER_END : file_sql_status_to_ret(tail);
      } else if (!st.ok()) {
        ret = file_sql_status_to_ret(st);
      } else if (row.size() != static_cast<size_t>(MY_SPEC.column_exprs_.count())) {
        ret = OB_ERR_UNEXPECTED;
        LOG_USER_ERROR(OB_ERR_UNEXPECTED, "file sql column count mismatch");
      } else {
        ObSQLSessionInfo *session = GET_MY_SESSION(ctx_);
        for (int64_t col_idx = 0; OB_SUCC(ret) && col_idx < MY_SPEC.column_exprs_.count(); ++col_idx) {
          if (OB_FAIL(fill_file_cell(eval_ctx_, session, row.at(static_cast<size_t>(col_idx)),
                                     MY_SPEC.column_exprs_.at(col_idx)))) {
          }
        }
        for (int64_t i = 0; OB_SUCC(ret) && i < MY_SPEC.output_.count(); ++i) {
          ObExpr *output = MY_SPEC.output_.at(i);
          bool is_column = false;
          for (int64_t c = 0; c < MY_SPEC.column_exprs_.count(); ++c) {
            if (output == MY_SPEC.column_exprs_.at(c)) {
              is_column = true;
              break;
            }
          }
          if (!is_column) {
            ObDatum *dst_datum = NULL;
            if (OB_FAIL(output->eval(eval_ctx_, dst_datum))) {
            }
          }
        }
        if (OB_SUCC(ret)) {
          ++row_idx_;
        }
      }
    }
  }
  return ret;
}
} // end namespace sql
} // end namespace oceanbase
