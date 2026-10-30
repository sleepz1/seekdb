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

#ifndef OB_FTS_STOP_WORD_H_
#define OB_FTS_STOP_WORD_H_

#include "lib/container/ob_iarray.h"
#include "lib/hash/ob_hashset.h"
#include "lib/hash/ob_hashmap.h"
#include "lib/lock/ob_tc_rwlock.h"
#include "object/ob_object.h"
#include "data_plane/fts/ob_fts_struct.h"

namespace oceanbase
{
namespace storage
{

class ObFTParserProperty;

#define FTS_STOP_WORD_MAX_LENGTH 10

// utf8 source table; per-collation hash sets are generated lazily / at init.
static const char ob_stop_word_list[][FTS_STOP_WORD_MAX_LENGTH] = {
  "a",
  "about",
  "an",
  "are",
  "as",
  "at",
  "be",
  "by",
  "com",
  "de",
  "en",
  "for",
  "from",
  "how",
  "i",
  "in",
  "is",
  "it",
  "la",
  "of",
  "on",
  "or",
  "that",
  "the",
  "this",
  "to",
  "was",
  "what",
  "when",
  "where",
  "who",
  "will",
  "with",
  "und",
  "www"
};

typedef common::hash::ObHashSet<
    storage::ObFTWord,
    common::hash::NoPthreadDefendMode> ObStopTokenTable;

// Lightweight per-lookup handle pointing at a process-global collation table.
class ObStopTokenChecker final
{
public:
  ObStopTokenChecker()
    : is_inited_(false), collation_type_(CS_TYPE_INVALID), stop_token_hash_table_(nullptr)
  {}
  ~ObStopTokenChecker() { reset(); }
  int init(const ObCollationType coll, ObStopTokenTable *stop_token_hash_table);
  void reset()
  {
    is_inited_ = false;
    collation_type_ = CS_TYPE_INVALID;
    stop_token_hash_table_ = nullptr;
  }
  int check_is_stop_token(const ObFTWord &token, bool &is_stop_token) const;

private:
  bool is_inited_;
  ObCollationType collation_type_;
  ObStopTokenTable *stop_token_hash_table_;
};

// Process-global generator: one lock-free hash set per collation, built once.
class ObStopTokenCheckerGen final
{
public:
  ObStopTokenCheckerGen()
    : is_inited_(false),
      allocator_(),
      lock_(),
      stop_token_hash_tables_()
  {}
  ~ObStopTokenCheckerGen() { reset(); }

  int init();
  void reset();
  int get_stop_token_checker_by_coll(const ObCollationType collation_type,
                                     ObStopTokenChecker &stop_token_checker);

private:
  int generate_stop_token_hash_table_by_coll(const ObCollationType coll);
  int convert_charset(const ObString &src_string,
                      const ObCollationType from_collation,
                      const ObCollationType to_collation,
                      ObString &converted_string);

private:
  static const int64_t DEFAULT_STOP_TOKEN_NUMBERS = 36L;
  static const int64_t DEFAULT_STOP_TOKEN_TABLE_CAPACITY =
      static_cast<int64_t>(1) << (64 - __builtin_clzll(DEFAULT_STOP_TOKEN_NUMBERS));

  typedef common::hash::ObHashMap<
      ObCollationType,
      ObStopTokenTable *,
      common::hash::NoPthreadDefendMode,
      common::hash::hash_func<int64_t>,
      common::hash::equal_to<ObCollationType>> StopTokenHashMap;

private:
  bool is_inited_;
  ObArenaAllocator allocator_;
  common::TCRWLock lock_;
  StopTokenHashMap stop_token_hash_tables_;

  static_assert(sizeof(ob_stop_word_list) / sizeof(ob_stop_word_list[0]) <= DEFAULT_STOP_TOKEN_NUMBERS,
      "ob_stop_word_list's number shouldn't be greater than DEFAULT_STOP_TOKEN_NUMBERS");
};

// Compatibility facade used by ObFTParseData / ObAddWord.
class ObStopWordChecker final
{
public:
  ObStopWordChecker() = default;
  ~ObStopWordChecker();

  int init();
  void destroy();
  int check_stopword(const ObFTWord &word, bool &is_stopword);
  int get_stop_token_checker(const ObCollationType coll, ObStopTokenChecker &checker);

private:
  ObStopTokenCheckerGen gen_;
  bool inited_ = false;
};

class ObAddWord final
{
public:
  ObAddWord(
      const ObFTParserProperty &property,
      const ObObjMeta &meta,
      const ObAddWordFlag &flag,
      common::ObIAllocator &allocator,
      ObFTWordMap &word_map);
  ~ObAddWord() = default;
  int process_word(
      const char *word,
      const int64_t word_len,
      const int64_t char_cnt,
      const int64_t word_freq);
  void reuse()
  {
    min_max_word_cnt_ = 0;
    non_stopword_cnt_ = 0;
    stopword_cnt_ = 0;
  }
  virtual int64_t get_add_word_count() const { return non_stopword_cnt_; }
  VIRTUAL_TO_STRING_KV(
      K_(word_meta),
      K_(min_max_word_cnt),
      K_(non_stopword_cnt),
      K_(stopword_cnt),
      K_(min_token_size),
      K_(max_token_size),
      K(word_map_.size()));

private:
  static constexpr int64_t MAX_CHAR_COUNT_PER_TOKEN = 1024;
  class UpdateWordCountCallBack final
  {
  public:
    explicit UpdateWordCountCallBack(const int64_t word_freq) : word_freq_(word_freq) {}
    // ObHashMap::set_or_update passes the pair (hashtable default preproc).
    int operator()(common::hash::HashMapPair<ObFTWord, int64_t> &pair)
    {
      pair.second += word_freq_;
      return OB_SUCCESS;
    }
  private:
    int64_t word_freq_;
  };

private:
  bool is_min_max_word(const int64_t c_len) const;
  int casedown_word(const ObFTWord &src, ObFTWord &dst);
  int check_stopword(const ObFTWord &word, bool &is_stopword);
  int groupby_word(const ObFTWord &word, const int64_t word_cnt);
private:
  ObObjMeta word_meta_;
  common::ObIAllocator &allocator_;
  ObFTWordMap &word_map_;
  int64_t min_max_word_cnt_;
  int64_t non_stopword_cnt_;
  int64_t stopword_cnt_;
  int64_t min_token_size_;
  int64_t max_token_size_;
  ObAddWordFlag flag_;
  common::ObDatumHashFuncType hash_func_;
  common::ObDatumCmpFuncType cmp_func_;
  ObStopTokenChecker stop_token_checker_;
  bool stop_checker_ready_;
};

} // end namespace storage
} // end namespace oceanbase

#endif // OB_FTS_STOP_WORD_H_
