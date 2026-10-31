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

int ObTextDaaTIter::do_one_merge_round(int64_t &count)
{
  return use_linear_merge_ ? linear_merge_round(count) : ObSRDaaTIterImpl::do_one_merge_round(count);
}

int ObTextDaaTIter::linear_merge_round(int64_t &count)
{
  int ret = OB_SUCCESS;
  const int64_t dim_cnt = dim_iters_->count();
  // 1. advance the cursors consumed by the previous round (all cursors on the first round)
  for (int64_t i = 0; OB_SUCC(ret) && i < next_round_cnt_; ++i) {
    const int64_t iter_idx = next_round_iter_idxes_[i];
    ObTextRetrievalDaaTTokenIter *dim_iter = static_cast<ObTextRetrievalDaaTTokenIter *>(dim_iters_->at(iter_idx));
    if (OB_ISNULL(dim_iter)) {
      ret = OB_ERR_UNEXPECTED;
    } else if (OB_FAIL(dim_iter->next_id(iter_domain_ids_[iter_idx]))) {
      if (OB_ITER_END == ret) {
        ret = OB_SUCCESS;
        iter_domain_ids_[iter_idx] = nullptr;
      } else {
        LOG_WARN("fail to get next row from dimension iter", K(ret), K(iter_idx));
      }
    }
  }
  // 2. pick the smallest doc id and every cursor positioned on it
  const ObDatum *min_id = nullptr;
  if (OB_SUCC(ret)) {
    next_round_cnt_ = 0;
    for (int64_t i = 0; OB_SUCC(ret) && i < dim_cnt; ++i) {
      const ObDatum *id = iter_domain_ids_[i];
      int cmp_ret = 0;
      if (nullptr == id) {
      } else if (nullptr == min_id) {
        min_id = id;
        next_round_iter_idxes_[next_round_cnt_++] = i;
      } else if (OB_FAIL(id_cmp_func_(*id, *min_id, cmp_ret, nullptr))) {
        LOG_WARN("fail to compare doc id", K(ret));
      } else if (cmp_ret < 0) {
        min_id = id;
        next_round_cnt_ = 0;
        next_round_iter_idxes_[next_round_cnt_++] = i;
      } else if (0 == cmp_ret) {
        next_round_iter_idxes_[next_round_cnt_++] = i;
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
    for (int64_t i = 0; OB_SUCC(ret) && i < next_round_cnt_; ++i) {
      const int64_t iter_idx = next_round_iter_idxes_[i];
      double score = static_cast<ObTextRetrievalDaaTTokenIter *>(dim_iters_->at(iter_idx))->curr_score();
      if (OB_NOT_NULL(iter_param_->dim_weights_)) {
        score = score * iter_param_->field_boost_ * iter_param_->dim_weights_->at(iter_idx);
      }
      if (OB_FAIL(relevance_collector_->collect_one_dim(iter_idx, score))) {
        LOG_WARN("fail to collect dimension", K(ret), K(iter_idx));
      }
    }
    if (OB_FAIL(ret)) {
    } else if (OB_FAIL(relevance_collector_->get_result(relevance, need_project))) {
      LOG_WARN("fail to get collected relevance", K(ret));
    } else if (need_project && OB_FAIL(process_collected_row(*min_id, relevance))) {
    } else if (need_project && OB_FAIL(filter_on_demand(count, relevance, need_project))) {
    } else if (need_project && OB_FAIL(cache_result(count, *min_id, relevance))) {
      if (OB_UNLIKELY(OB_ITER_END != ret)) {
        LOG_WARN("fail to cache result", K(ret));
      }
    }
  }
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
