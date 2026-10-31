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

#define USING_LOG_PREFIX STORAGE

#include "ob_text_daat_iter.h"

namespace oceanbase
{
namespace storage
{
int ObTextDaaTIter::init(const ObTextDaaTParam &param)
{
  int ret = OB_SUCCESS;
  if (OB_ISNULL(param.base_param_) || OB_ISNULL(param.dim_iters_) || OB_ISNULL(param.allocator_)
      || OB_ISNULL(param.relevance_collector_)) {
    ret = OB_INVALID_ARGUMENT;
  } else if (OB_FAIL(ObSRDaaTIterImpl::init(*param.base_param_, *param.dim_iters_,
                                            *param.allocator_, *param.relevance_collector_))) {
  } else if (nullptr != param.base_param_->relevance_expr_
             && OB_FAIL(bm25_param_estimator_.init(param.bm25_param_est_ctx_))) {
  } else {
    mode_flag_ = param.mode_flag_;
    function_lookup_mode_ = param.function_lookup_mode_;
    const sql::ObDatumMeta &id_meta = iter_param_->id_proj_expr_->datum_meta_;
    common::ObDatumBasicFuncs *basic_funcs = ObDatumFuncs::get_basic_func(id_meta.type_, id_meta.cs_type_);
    id_cmp_func_ = nullptr == basic_funcs ? nullptr : basic_funcs->null_first_cmp_;
    use_linear_merge_ = can_use_linear_merge();
    prepare_linear_merge();
  }
  return ret;
}

bool ObTextDaaTIter::can_use_linear_merge() const
{
  // Only used when no BM25 relevance is computed: every matched dimension then contributes
  // 1.0 * field_boost * weight. When each such term is an exactly representable integer the
  // collected relevance does not depend on the order the dimensions are visited, so the
  // linear merge produces exactly the same rows, in the same doc id order, with the same
  // relevance as the heap based merge.
  bool can_use = !function_lookup_mode_
      && nullptr != id_cmp_func_
      && nullptr == iter_param_->relevance_expr_
      && OB_NOT_NULL(dim_iters_)
      && dim_iters_->count() > 0
      && dim_iters_->count() <= LINEAR_MERGE_MAX_DIM_CNT;
  if (can_use && OB_NOT_NULL(iter_param_->dim_weights_)) {
    static const double MAX_EXACT_TERM = 1099511627776.0; // 2^40
    for (int64_t i = 0; can_use && i < iter_param_->dim_weights_->count(); ++i) {
      const double term = 1.0 * iter_param_->field_boost_ * iter_param_->dim_weights_->at(i);
      can_use = term >= 0.0 && term <= MAX_EXACT_TERM && term == static_cast<double>(static_cast<int64_t>(term));
    }
  }
  return can_use;
}

void ObTextDaaTIter::prepare_linear_merge()
{
  linear_dim_cnt_ = 0;
  linear_pending_cnt_ = 0;
  linear_binary_id_ = false;
  linear_sum_relevance_ = false;
  linear_should_match_ = 0;
  if (use_linear_merge_) {
    linear_dim_cnt_ = dim_iters_->count();
    for (int64_t i = 0; i < linear_dim_cnt_; ++i) {
      linear_dims_[i] = static_cast<ObTextRetrievalDaaTTokenIter *>(dim_iters_->at(i));
      linear_ids_[i] = nullptr;
      linear_pending_[i] = i;
      if (OB_ISNULL(linear_dims_[i])) {
        use_linear_merge_ = false;
      }
    }
    linear_pending_cnt_ = linear_dim_cnt_;
    const sql::ObDatumMeta &id_meta = iter_param_->id_proj_expr_->datum_meta_;
    linear_binary_id_ = ObStringTC == ob_obj_type_class(id_meta.type_) && CS_TYPE_BINARY == id_meta.cs_type_;
    linear_should_match_ = 0;
    linear_sum_relevance_ = OB_NOT_NULL(relevance_collector_)
        && relevance_collector_->is_plain_inner_product(linear_should_match_);
    const sql::ObExpr *filter_expr = iter_param_->filter_expr_;
    const sql::ObExpr *relevance_proj_expr = iter_param_->relevance_proj_expr_;
    linear_bool_filter_ = OB_NOT_NULL(filter_expr) && OB_NOT_NULL(relevance_proj_expr)
        && OB_NOT_NULL(iter_param_->eval_ctx_) && iter_param_->eval_ctx_->is_vectorized()
        && T_OP_BOOL == filter_expr->type_ && 1 == filter_expr->arg_cnt_
        && OB_NOT_NULL(filter_expr->args_) && relevance_proj_expr == filter_expr->args_[0]
        && ObDoubleType == relevance_proj_expr->datum_meta_.type_
        && iter_param_->need_project_relevance();
  }
}

int ObTextDaaTIter::get_next_rows(const int64_t capacity, int64_t &count)
{
  // remember how many rows the base loop wants, so that the linear merge can fill the whole
  // batch in one do_one_merge_round() call (the base loop then stops at the same row)
  linear_capacity_ = (is_inited_ && OB_NOT_NULL(iter_param_)) ? MIN(capacity, iter_param_->max_batch_size_) : 0;
  return ObSRDaaTIterImpl::get_next_rows(capacity, count);
}

int ObTextDaaTIter::do_one_merge_round(int64_t &count)
{
  return use_linear_merge_ ? linear_merge_round(count) : ObSRDaaTIterImpl::do_one_merge_round(count);
}

int ObTextDaaTIter::linear_merge_round(int64_t &count)
{
  int ret = OB_SUCCESS;
  const ObIArray<double> *dim_weights = iter_param_->dim_weights_;
  const double field_boost = iter_param_->field_boost_;
  const bool no_limit = !iter_param_->limit_param_->is_valid();
  const int64_t buffer_cnt = MIN(buffered_domain_ids_.count(), buffered_relevances_.count());
  ObDocIdExt *buffered_ids = buffered_domain_ids_.get_data();
  double *buffered_relevances = buffered_relevances_.get_data();
  // Each iteration is exactly one round of the original one-row-per-call merge; iterating
  // until the requested batch is full only saves the per-row virtual dispatch.
  do {
    // 1. advance the cursors consumed by the previous round (all cursors on the first round)
    for (int64_t i = 0; OB_SUCC(ret) && i < linear_pending_cnt_; ++i) {
      const int64_t iter_idx = linear_pending_[i];
      if (OB_FAIL(linear_dims_[iter_idx]->next_id(linear_ids_[iter_idx]))) {
        if (OB_ITER_END == ret) {
          ret = OB_SUCCESS;
          linear_ids_[iter_idx] = nullptr;
        } else {
          LOG_WARN("fail to get next row from dimension iter", K(ret), K(iter_idx));
        }
      }
    }
    // 2. pick the smallest doc id and every cursor positioned on it
    const ObDatum *min_id = nullptr;
    if (OB_SUCC(ret)) {
      linear_pending_cnt_ = 0;
      for (int64_t i = 0; OB_SUCC(ret) && i < linear_dim_cnt_; ++i) {
        const ObDatum *id = linear_ids_[i];
        int cmp_ret = 0;
        if (nullptr == id) {
        } else if (nullptr == min_id) {
          min_id = id;
          linear_pending_[linear_pending_cnt_++] = i;
        } else if (OB_FAIL(linear_cmp_id(*id, *min_id, cmp_ret))) {
          LOG_WARN("fail to compare doc id", K(ret));
        } else if (cmp_ret < 0) {
          min_id = id;
          linear_pending_cnt_ = 0;
          linear_pending_[linear_pending_cnt_++] = i;
        } else if (0 == cmp_ret) {
          linear_pending_[linear_pending_cnt_++] = i;
        }
      }
      if (OB_SUCC(ret) && nullptr == min_id) {
        ret = OB_ITER_END;
      }
    }
    // 3. collect relevance, filter and buffer the row exactly as the heap based merge does
    if (OB_SUCC(ret)) {
      double relevance = 0.0;
      bool need_project = false;
      if (linear_sum_relevance_) {
        // ObSRDaaTInnerProductRelevanceCollector: sum in the same order, valid iff enough dims
        for (int64_t i = 0; i < linear_pending_cnt_; ++i) {
          const int64_t iter_idx = linear_pending_[i];
          double score = linear_dims_[iter_idx]->curr_score();
          if (OB_NOT_NULL(dim_weights)) {
            score = score * field_boost * dim_weights->at(iter_idx);
          }
          relevance += score;
        }
        need_project = linear_pending_cnt_ >= linear_should_match_;
      } else {
        for (int64_t i = 0; OB_SUCC(ret) && i < linear_pending_cnt_; ++i) {
          const int64_t iter_idx = linear_pending_[i];
          double score = linear_dims_[iter_idx]->curr_score();
          if (OB_NOT_NULL(dim_weights)) {
            score = score * field_boost * dim_weights->at(iter_idx);
          }
          if (OB_FAIL(relevance_collector_->collect_one_dim(iter_idx, score))) {
            LOG_WARN("fail to collect dimension", K(ret), K(iter_idx));
          }
        }
        if (OB_FAIL(ret)) {
        } else if (OB_FAIL(relevance_collector_->get_result(relevance, need_project))) {
          LOG_WARN("fail to get collected relevance", K(ret));
        }
      }
      if (OB_FAIL(ret)) {
      } else if (need_project && OB_FAIL(process_collected_row(*min_id, relevance))) {
      } else if (!need_project) {
      } else if (linear_bool_filter_) {
        // BOOL(<non-null double relevance>) is exactly (relevance != 0); the relevance of every
        // buffered row is projected by project_results(), so nothing has to be evaluated here.
        need_project = 0 != relevance;
      } else if (OB_FAIL(filter_on_demand(count, relevance, need_project))) {
      }
      if (OB_FAIL(ret) || !need_project) {
      } else if (no_limit && OB_LIKELY(count < buffer_cnt)) {
        // same as cache_result() without limit/offset
        ++input_row_cnt_;
        buffered_ids[count].from_datum(*min_id);
        buffered_relevances[count] = relevance;
        ++count;
        ++output_row_cnt_;
      } else if (OB_FAIL(cache_result(count, *min_id, relevance))) {
        if (OB_UNLIKELY(OB_ITER_END != ret)) {
          LOG_WARN("fail to cache result", K(ret));
        }
      }
    }
  } while (OB_SUCC(ret) && count < linear_capacity_);
  return ret;
}

void ObTextDaaTIter::reset()
{
  bm25_param_estimator_.reset();
  // this class can know the type of dim_iters_, but ObSRDaaTIterImpl maybe not
  if (OB_NOT_NULL(dim_iters_)) {
    for (int64_t i = 0; i < dim_iters_->count(); ++i) {
      static_cast<ObTextRetrievalDaaTTokenIter *>(dim_iters_->at(i))->reset();
    }
  }
  ObSRDaaTIterImpl::reset();
}

void ObTextDaaTIter::reuse(const bool switch_tablet)
{
  if ((OB_NOT_NULL(dim_iters_) && 0 == dim_iters_->count())) {
    // do nothing
  } else {
    bm25_param_estimator_.reuse(switch_tablet);
  }
  if (OB_NOT_NULL(dim_iters_)) {
    for (int64_t i = 0; i < dim_iters_->count(); ++i) {
      static_cast<ObTextRetrievalDaaTTokenIter *>(dim_iters_->at(i))->reuse();
    }
  }
  ObSRDaaTIterImpl::reuse(switch_tablet);
  use_linear_merge_ = is_inited_ && can_use_linear_merge();
  prepare_linear_merge();
}

int ObTextDaaTIter::pre_process()
{
  int ret = OB_SUCCESS;
  if (dim_iters_->count() == 0) {
    ret = OB_ITER_END;
  } else if (nullptr != iter_param_->relevance_expr_) {
    if (OB_FAIL(bm25_param_estimator_.do_estimation(*iter_param_->eval_ctx_))) {
    }
  }
  return ret;
}

ObTextBMWIter::ObTextBMWIter()
  : ObSRBMWIterImpl(),
    bm25_param_estimator_() {}

void ObTextBMWIter::reuse(const bool switch_tablet)
{
  bm25_param_estimator_.reuse(switch_tablet);
  if (OB_NOT_NULL(dim_iters_)) {
    for (int64_t i = 0; i < dim_iters_->count(); ++i) {
      static_cast<ObTextRetrievalBlockMaxIter *>(dim_iters_->at(i))->reuse();
    }
  }
  ObSRBMWIterImpl::reuse(switch_tablet);
}

void ObTextBMWIter::reset()
{
  bm25_param_estimator_.reset();
  if (OB_NOT_NULL(dim_iters_)) {
    for (int64_t i = 0; i < dim_iters_->count(); ++i) {
      static_cast<ObTextRetrievalBlockMaxIter *>(dim_iters_->at(i))->reset();
    }
  }
  ObSRBMWIterImpl::reset();
}

int ObTextBMWIter::init(const ObTextDaaTParam &param)
{
  int ret = OB_SUCCESS;
  if (OB_ISNULL(param.base_param_) || OB_ISNULL(param.dim_iters_) || OB_ISNULL(param.allocator_)
      || OB_ISNULL(param.relevance_collector_)) {
    ret = OB_INVALID_ARGUMENT;
  } else if (OB_FAIL(ObSRBMWIterImpl::init(*param.base_param_, *param.dim_iters_,
                                           *param.allocator_, *param.relevance_collector_))) {
  } else if (nullptr != param.base_param_->relevance_expr_
             && OB_FAIL(bm25_param_estimator_.init(param.bm25_param_est_ctx_))) {
  }
  return ret;
}

int ObTextBMWIter::get_next_rows(const int64_t capacity, int64_t &count)
{
  int ret = OB_SUCCESS;
  if (IS_NOT_INIT) {
    ret = OB_NOT_INIT;
  } else if (dim_iters_->count() == 0) {
    ret = OB_ITER_END;
  } else if (OB_FAIL(bm25_param_estimator_.do_estimation(*iter_param_->eval_ctx_))) {
  }

  if (FAILEDx(ObSRBMWIterImpl::get_next_rows(capacity, count))) {
    if (OB_UNLIKELY(OB_ITER_END != ret)) {
    }
  }
  return ret;
}

int ObTextBMWIter::init_before_wand_process()
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(!bm25_param_estimator_.is_estimated())) {
    ret = OB_ERR_UNEXPECTED;
  }

  for (int64_t i = 0; OB_SUCC(ret) && i < dim_iters_->count(); ++i) {
    ObTextRetrievalBlockMaxIter *block_max_iter = static_cast<ObTextRetrievalBlockMaxIter *>(dim_iters_->at(i));
    if (OB_FAIL(block_max_iter->init_block_max_iter(
        bm25_param_estimator_.get_total_doc_cnt(), bm25_param_estimator_.get_avg_doc_token_cnt()))) {
    }
  }
  return ret;
}

} // namespace storage
} // namespace oceanbase
