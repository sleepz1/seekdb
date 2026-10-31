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

#ifndef OB_TEXT_DAAT_ITER_H_
#define OB_TEXT_DAAT_ITER_H_

#include "query/ob_sql_define.h"
#include "ob_inv_idx_param_estimator.h"
#include "ob_sparse_bmw_iter.h"
#include "ob_sparse_daat_iter.h"
#include "ob_text_retrieval_token_iter.h"

namespace oceanbase
{
namespace sql
{
  class ObDASScanIter;
}
namespace storage
{

struct ObTextDaaTParam
{
  ObTextDaaTParam()
    : dim_iters_(nullptr), base_param_(nullptr), allocator_(nullptr), relevance_collector_(nullptr),
      bm25_param_est_ctx_(), mode_flag_(sql::ObMatchAgainstMode::NATURAL_LANGUAGE_MODE),
      function_lookup_mode_(false) {}
  ~ObTextDaaTParam() {}
  TO_STRING_KV(K_(base_param), KP_(dim_iters), K_(bm25_param_est_ctx),  K_(mode_flag), K_(function_lookup_mode));
  ObIArray<ObISRDaaTDimIter *> *dim_iters_;
  ObSparseRetrievalMergeParam *base_param_;
  common::ObArenaAllocator *allocator_;
  ObSRDaaTRelevanceCollector *relevance_collector_;
  ObBM25ParamEstCtx bm25_param_est_ctx_;
  sql::ObMatchAgainstMode mode_flag_;
  bool function_lookup_mode_;
};

class ObTextDaaTIter final : public ObSRDaaTIterImpl
{
public:
  ObTextDaaTIter() : ObSRDaaTIterImpl(),
      bm25_param_estimator_(),
      mode_flag_(sql::ObMatchAgainstMode::NATURAL_LANGUAGE_MODE),
      function_lookup_mode_(false),
      use_linear_merge_(false),
      id_cmp_func_(nullptr),
      linear_dim_cnt_(0),
      linear_pending_cnt_(0),
      linear_bool_filter_(false),
      linear_binary_id_(false),
      linear_sum_relevance_(false),
      linear_should_match_(0),
      linear_capacity_(0) {}
  virtual ~ObTextDaaTIter() { reset(); }

  int init(const ObTextDaaTParam &param);
  virtual void reuse(const bool switch_tablet = false) override;
  virtual void reset() override;
  virtual int get_next_rows(const int64_t capacity, int64_t &count) override;
protected:
  virtual int pre_process() override;
  virtual int do_one_merge_round(int64_t &count) override;
private:
  // For a handful of query tokens without BM25 scoring, merging the sorted posting
  // cursors by a linear minimum scan is cheaper than maintaining the merge heap.
  static const int64_t LINEAR_MERGE_MAX_DIM_CNT = 8;
  bool can_use_linear_merge() const;
  void prepare_linear_merge();
  int linear_merge_round(int64_t &count);
  OB_INLINE int linear_cmp_id(const ObDatum &l, const ObDatum &r, int &cmp_ret) const
  {
    int ret = common::OB_SUCCESS;
    if (linear_binary_id_ && OB_LIKELY(!l.is_null() && !r.is_null())) {
      // same result as the binary collation compare function (memcmp, then length)
      const uint32_t min_len = l.len_ < r.len_ ? l.len_ : r.len_;
      const int mem_ret = 0 == min_len ? 0 : MEMCMP(l.ptr_, r.ptr_, min_len);
      cmp_ret = 0 != mem_ret ? mem_ret : (l.len_ > r.len_ ? 1 : (l.len_ < r.len_ ? -1 : 0));
    } else {
      ret = id_cmp_func_(l, r, cmp_ret, nullptr);
    }
    return ret;
  }
protected:
  ObBM25ParamEstimator bm25_param_estimator_;
  sql::ObMatchAgainstMode mode_flag_;
  bool function_lookup_mode_;
  bool use_linear_merge_;
  common::ObDatumCmpFuncType id_cmp_func_;
  // plain cursor state of the linear merge (valid when use_linear_merge_)
  int64_t linear_dim_cnt_;
  int64_t linear_pending_cnt_;
  // the pushed down filter is BOOL(relevance) on the projected double relevance
  bool linear_bool_filter_;
  // doc id is a binary-collation string: compare inline
  bool linear_binary_id_;
  // the relevance collector is the plain inner product one: sum the scores inline
  bool linear_sum_relevance_;
  int64_t linear_should_match_;
  // rows wanted by the current get_next_rows() call
  int64_t linear_capacity_;
  ObTextRetrievalDaaTTokenIter *linear_dims_[LINEAR_MERGE_MAX_DIM_CNT];
  const ObDatum *linear_ids_[LINEAR_MERGE_MAX_DIM_CNT];
  int64_t linear_pending_[LINEAR_MERGE_MAX_DIM_CNT];
private:
  DISALLOW_COPY_AND_ASSIGN(ObTextDaaTIter);
};


class ObTextBMWIter final : public ObSRBMWIterImpl
{
public:
  ObTextBMWIter();
  virtual ~ObTextBMWIter() { reset(); }
  virtual void reuse(const bool switch_tablet = false) override;
  virtual void reset() override;
  int init(const ObTextDaaTParam &param);
protected:
  virtual int get_next_rows(const int64_t capacity, int64_t &count) override;
  virtual int init_before_wand_process() override;
protected:
  ObBM25ParamEstimator bm25_param_estimator_;
private:
  DISALLOW_COPY_AND_ASSIGN(ObTextBMWIter);
};

} // namespace storage
} // namespace oceanbase

#endif
