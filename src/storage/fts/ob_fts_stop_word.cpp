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

#include "object/ob_object.h"
#define USING_LOG_PREFIX STORAGE_FTS

#include "share/rc/ob_server_runtime.h"
#include "storage/fts/ob_fts_stop_word.h"
#include "storage/fts/ob_fts_parser_helper.h"
#include "storage/fts/ob_fts_parser_property.h"
#include "share/datum/ob_datum_funcs.h"
#include "storage/ob_storage_util.h"

namespace oceanbase
{
namespace storage
{
namespace
{
inline bool is_ascii_casedown_noop(const char *str, const int64_t len)
{
  for (int64_t i = 0; i < len; ++i) {
    const unsigned char c = static_cast<unsigned char>(str[i]);
    if (c >= 0x80 || (c >= 'A' && c <= 'Z')) {
      return false;
    }
  }
  return true;
}
} // namespace

////////////////////////////////////////////////////////////////////////////////
// class ObStopTokenChecker
int ObStopTokenChecker::init(const ObCollationType coll, ObStopTokenTable *stop_token_hash_table)
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(is_inited_)) {
    ret = OB_INIT_TWICE;
  } else if (OB_UNLIKELY(coll == CS_TYPE_INVALID ||
                         coll >= CS_TYPE_PINYIN_BEGIN_MARK ||
                         nullptr == stop_token_hash_table)) {
    ret = OB_INVALID_ARGUMENT;
  } else {
    collation_type_ = coll;
    stop_token_hash_table_ = stop_token_hash_table;
    is_inited_ = true;
  }
  return ret;
}

int ObStopTokenChecker::check_is_stop_token(const ObFTWord &token, bool &is_stop_token) const
{
  int ret = OB_SUCCESS;
  is_stop_token = false;
  if (OB_UNLIKELY(!is_inited_)) {
    ret = OB_NOT_INIT;
  } else {
    ret = stop_token_hash_table_->exist_refactored(token);
    if (OB_HASH_NOT_EXIST == ret) {
      is_stop_token = false;
      ret = OB_SUCCESS;
    } else if (OB_HASH_EXIST == ret) {
      is_stop_token = true;
      ret = OB_SUCCESS;
    } else {
      LOG_WARN("fail to exist refactored", K(ret), K(token));
    }
  }
  return ret;
}

////////////////////////////////////////////////////////////////////////////////
// class ObStopTokenCheckerGen
void ObStopTokenCheckerGen::reset()
{
  common::TCWLockGuard w_guard(lock_);
  is_inited_ = false;
  if (OB_LIKELY(stop_token_hash_tables_.created())) {
    StopTokenHashMap::iterator it;
    for (it = stop_token_hash_tables_.begin(); it != stop_token_hash_tables_.end(); ++it) {
      if (OB_LIKELY(nullptr != it->second)) {
        (void)it->second->destroy();
        OB_DELETE(ObStopTokenTable, &allocator_, it->second);
        it->second = nullptr;
      }
    }
    (void)stop_token_hash_tables_.destroy();
  }
  allocator_.reset();
}

int ObStopTokenCheckerGen::init()
{
  int ret = OB_SUCCESS;
  common::TCWLockGuard w_guard(lock_);
  if (OB_UNLIKELY(is_inited_)) {
    ret = OB_INIT_TWICE;
  } else {
    allocator_.set_attr(ObMemAttr("tokenCheckerGen"));
    ObCollationType general_coll_types[] = { CS_TYPE_UTF8MB4_GENERAL_CI,
                                             CS_TYPE_UTF8MB4_BIN };
    const int64_t general_coll_type_count = sizeof(general_coll_types) / sizeof(general_coll_types[0]);
    if (OB_LIKELY(!stop_token_hash_tables_.created()) &&
        OB_FAIL(stop_token_hash_tables_.create(ObCharset::VALID_COLLATION_TYPES,
                                               "st_hash_tables",
                                               "st_hash_tables"))) {
      LOG_WARN("fail to create stop token hash tables", K(ret));
    }
    for (int64_t i = 0; OB_SUCC(ret) && i < general_coll_type_count; ++i) {
      if (OB_FAIL(generate_stop_token_hash_table_by_coll(general_coll_types[i]))) {
        LOG_WARN("fail to initialize stop token table by collation", K(ret), K(general_coll_types[i]));
      }
    }
    if (OB_SUCC(ret)) {
      is_inited_ = true;
    }
  }
  return ret;
}

int ObStopTokenCheckerGen::convert_charset(
    const ObString &src_string,
    const ObCollationType from_collation,
    const ObCollationType to_collation,
    ObString &converted_string)
{
  int ret = OB_SUCCESS;
  converted_string.reset();
  if (CHARSET_UTF8MB4 == ObCharset::charset_type_by_coll(to_collation)) {
    converted_string = src_string;
  } else if (OB_FAIL(common::ObCharset::charset_convert(allocator_,
                                                        src_string,
                                                        from_collation,
                                                        to_collation,
                                                        converted_string))) {
    LOG_WARN("fail to convert charset",
        K(ret), K(src_string), K(from_collation), K(to_collation));
  }
  return ret;
}

int ObStopTokenCheckerGen::generate_stop_token_hash_table_by_coll(const ObCollationType coll)
{
  int ret = OB_SUCCESS;
  ObStopTokenTable *stop_token_hash_table = OB_NEWx(ObStopTokenTable, &allocator_);
  if (OB_UNLIKELY(nullptr == stop_token_hash_table)) {
    ret = OB_ALLOCATE_MEMORY_FAILED;
    LOG_WARN("fail to allocate memory for stop token table", K(ret));
  } else if (OB_FAIL(stop_token_hash_table->create(DEFAULT_STOP_TOKEN_TABLE_CAPACITY,
                                                   "stop_token_tab", "stop_token_tab"))) {
    LOG_WARN("fail to create stop token hash set", K(ret));
  } else {
    ObObjMeta meta;
    meta.set_varchar();
    meta.set_collation_type(coll);
    const int64_t stop_token_count = sizeof(ob_stop_word_list) / sizeof(ob_stop_word_list[0]);

    common::ObDatumBasicFuncs *basic_funcs = ObDatumFuncs::get_basic_func(meta.get_type(),
                                                                          meta.get_collation_type());
    ObDatumCmpFuncType cmp_func = get_datum_cmp_func(meta, meta);

    if (OB_UNLIKELY(nullptr == basic_funcs || nullptr == cmp_func)) {
      ret = OB_ERR_UNEXPECTED;
      LOG_WARN("fail to get basic funcs or cmp func",
          K(ret), K(meta), KP(basic_funcs), KP(cmp_func));
    } else if (OB_UNLIKELY(nullptr == basic_funcs->default_hash_)) {
      ret = OB_ERR_UNEXPECTED;
      LOG_WARN("the default hash is null", K(ret));
    }
    for (int64_t i = 0; OB_SUCC(ret) && i < stop_token_count; ++i) {
      ObFTWord stop_token;
      ObString converted_string;
      if (OB_FAIL(convert_charset(ob_stop_word_list[i],
                                  CS_TYPE_UTF8MB4_GENERAL_CI,
                                  coll,
                                  converted_string))) {
        LOG_WARN("fail to convert charset", K(ret), K(coll));
      } else if (OB_FAIL(stop_token.init(converted_string.ptr(),
                                         converted_string.length(),
                                         meta,
                                         basic_funcs->default_hash_,
                                         cmp_func))) {
        LOG_WARN("fail to initialize stop token",
            K(ret), K(converted_string), K(meta));
      } else {
        uint64_t hash_val = 0;
        if (OB_FAIL(stop_token.hash(hash_val))) {
          LOG_WARN("fail to calc stop token hash value", K(ret), K(stop_token));
        } else if (OB_FAIL(stop_token_hash_table->set_refactored(stop_token))) {
          LOG_WARN("fail to set stop token", K(ret), K(stop_token));
        }
      }
    }
    if (OB_SUCC(ret)) {
      if (OB_FAIL(stop_token_hash_tables_.set_refactored(coll, stop_token_hash_table))) {
        LOG_WARN("fail to set stop token table", K(ret), K(coll));
      }
    }
  }
  if (OB_FAIL(ret)) {
    if (nullptr != stop_token_hash_table) {
      (void)stop_token_hash_table->destroy();
      OB_DELETE(ObStopTokenTable, &allocator_, stop_token_hash_table);
      stop_token_hash_table = nullptr;
    }
  }
  return ret;
}

int ObStopTokenCheckerGen::get_stop_token_checker_by_coll(
    const ObCollationType coll,
    ObStopTokenChecker &stop_token_checker)
{
  int ret = OB_SUCCESS;
  ObStopTokenTable *stop_token_hash_table = nullptr;
  stop_token_checker.reset();
  if (OB_UNLIKELY(!is_inited_)) {
    ret = OB_NOT_INIT;
  } else if (OB_UNLIKELY(CS_TYPE_INVALID == coll || coll >= CS_TYPE_PINYIN_BEGIN_MARK)) {
    ret = OB_INVALID_ARGUMENT;
  } else {
    {
      common::TCRLockGuard r_guard(lock_);
      if (OB_FAIL(stop_token_hash_tables_.get_refactored(coll, stop_token_hash_table))) {
        if (OB_LIKELY(OB_HASH_NOT_EXIST == ret)) {
          ret = OB_SUCCESS;
        } else {
          LOG_WARN("fail to get stop token table", K(ret), K(coll));
        }
      } else if (OB_FAIL(stop_token_checker.init(coll, stop_token_hash_table))) {
        LOG_WARN("fail to initialize stop token checker", K(ret), K(coll));
      }
    }

    if (OB_SUCC(ret) && nullptr == stop_token_hash_table) {
      common::TCWLockGuard write_guard(lock_);
      if (OB_FAIL(stop_token_hash_tables_.get_refactored(coll, stop_token_hash_table))) {
        if (OB_LIKELY(OB_HASH_NOT_EXIST == ret)) {
          ret = OB_SUCCESS;
          if (OB_FAIL(generate_stop_token_hash_table_by_coll(coll))) {
            LOG_WARN("fail to generate stop token table by collation", K(ret), K(coll));
          } else if (OB_FAIL(stop_token_hash_tables_.get_refactored(coll, stop_token_hash_table))) {
            LOG_WARN("fail to get stop token table after creation", K(ret), K(coll));
          }
        } else {
          LOG_WARN("fail to get stop token table", K(ret), K(coll));
        }
      }
      if (FAILEDx(stop_token_checker.init(coll, stop_token_hash_table))) {
        LOG_WARN("fail to initialize stop token checker", K(ret), K(coll));
      }
    }
  }
  return ret;
}

////////////////////////////////////////////////////////////////////////////////
// class ObStopWordChecker (facade)
ObStopWordChecker::~ObStopWordChecker()
{
  destroy();
}

int ObStopWordChecker::init()
{
  int ret = OB_SUCCESS;
  if (inited_) {
    ret = OB_INIT_TWICE;
  } else if (OB_FAIL(gen_.init())) {
  } else {
    inited_ = true;
  }
  return ret;
}

void ObStopWordChecker::destroy()
{
  if (inited_) {
    gen_.reset();
    inited_ = false;
  }
}

int ObStopWordChecker::get_stop_token_checker(const ObCollationType coll,
                                              ObStopTokenChecker &checker)
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(!inited_)) {
    ret = OB_NOT_INIT;
  } else if (OB_FAIL(gen_.get_stop_token_checker_by_coll(coll, checker))) {
  }
  return ret;
}

int ObStopWordChecker::check_stopword(const ObFTWord &word, bool &is_stopword)
{
  int ret = OB_SUCCESS;
  ObStopTokenChecker checker;
  if (OB_UNLIKELY(!inited_)) {
    ret = OB_NOT_INIT;
  } else if (OB_UNLIKELY(word.empty())) {
    ret = OB_INVALID_ARGUMENT;
  } else if (OB_FAIL(gen_.get_stop_token_checker_by_coll(word.get_collation_type(), checker))) {
  } else if (OB_FAIL(checker.check_is_stop_token(word, is_stopword))) {
  }
  return ret;
}

////////////////////////////////////////////////////////////////////////////////
// class ObAddWord
ObAddWord::ObAddWord(
    const ObFTParserProperty &property,
    const ObObjMeta &meta,
    const ObAddWordFlag &flag,
    common::ObIAllocator &allocator,
    ObFTWordMap &word_map)
  : word_meta_(meta),
    allocator_(allocator),
    word_map_(word_map),
    min_max_word_cnt_(0),
    non_stopword_cnt_(0),
    stopword_cnt_(0),
    min_token_size_(property.min_token_size_),
    max_token_size_(property.max_token_size_),
    flag_(flag),
    hash_func_(nullptr),
    cmp_func_(nullptr),
    stop_token_checker_(),
    stop_checker_ready_(false)
{
  common::ObDatumBasicFuncs *basic_funcs =
      ObDatumFuncs::get_basic_func(meta.get_type(), meta.get_collation_type());
  if (OB_NOT_NULL(basic_funcs)) {
    hash_func_ = basic_funcs->default_hash_;
  }
  cmp_func_ = get_datum_cmp_func(meta, meta);
  if (flag_.stopword()) {
    ObStopWordChecker *stop_word_checker = ObFTParseData::instance().stop_word_checker();
    if (OB_NOT_NULL(stop_word_checker) &&
        OB_SUCCESS == stop_word_checker->get_stop_token_checker(meta.get_collation_type(),
                                                                stop_token_checker_)) {
      stop_checker_ready_ = true;
    }
  }
}

int ObAddWord::process_word(
    const char *word,
    const int64_t word_len,
    const int64_t char_cnt,
    const int64_t word_freq)
{
  int ret = OB_SUCCESS;
  bool is_stopword = false;
  ObFTWord src_word;
  ObFTWord dst_word;
  if (OB_ISNULL(word) || OB_UNLIKELY(0 >= word_len || 0 >= char_cnt || 0 >= word_freq)) {
    ret = OB_INVALID_ARGUMENT;
  } else if (OB_UNLIKELY(is_min_max_word(char_cnt))) {
    // Early short-circuit: min/max filter before any allocation / hash work.
    ++min_max_word_cnt_;
  } else {
    if (OB_NOT_NULL(hash_func_) && OB_NOT_NULL(cmp_func_)) {
      if (OB_FAIL(src_word.init(word, word_len, word_meta_, hash_func_, cmp_func_))) {
      }
    } else {
      src_word = ObFTWord(word_len, word, word_meta_);
    }
    if (OB_FAIL(ret)) {
    } else if (OB_FAIL(casedown_word(src_word, dst_word))) {
    } else if (OB_FAIL(check_stopword(dst_word, is_stopword))) {
    } else if (OB_UNLIKELY(is_stopword)) {
      ++stopword_cnt_;
    } else if (OB_FAIL(groupby_word(dst_word, word_freq))) {
    } else {
      non_stopword_cnt_ += word_freq;
    }
  }
  return ret;
}

bool ObAddWord::is_min_max_word(const int64_t c_len) const
{
  return (c_len > MAX_CHAR_COUNT_PER_TOKEN) ||
         (flag_.min_max_word() && (c_len < min_token_size_ || c_len > max_token_size_));
}

int ObAddWord::casedown_word(const ObFTWord &src, ObFTWord &dst)
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(src.empty())) {
    ret = OB_INVALID_ARGUMENT;
  } else if (flag_.casedown()) {
    const ObString src_str = src.get_word().get_string();
    if (is_ascii_casedown_noop(src_str.ptr(), src_str.length())) {
      dst = src;
    } else {
      ObString dst_str;
      if (OB_FAIL(ObCharset::tolower(
                      word_meta_.get_collation_type(),
                      src_str,
                      dst_str,
                      allocator_))) {
      } else if (OB_NOT_NULL(hash_func_) && OB_NOT_NULL(cmp_func_)) {
        if (OB_FAIL(dst.init(dst_str.ptr(), dst_str.length(), word_meta_, hash_func_, cmp_func_))) {
        }
      } else {
        dst = ObFTWord(dst_str.length(), dst_str.ptr(), word_meta_);
      }
    }
  } else {
    dst = src;
  }
  return ret;
}

int ObAddWord::check_stopword(const ObFTWord &ft_word, bool &is_stopword)
{
  int ret = OB_SUCCESS;
  is_stopword = false;
  if (OB_UNLIKELY(ft_word.empty())) {
    ret = OB_INVALID_ARGUMENT;
  } else if (flag_.stopword()) {
    if (stop_checker_ready_) {
      if (OB_FAIL(stop_token_checker_.check_is_stop_token(ft_word, is_stopword))) {
      }
    } else {
      ObStopWordChecker *stop_word_checker = ObFTParseData::instance().stop_word_checker();
      if (OB_ISNULL(stop_word_checker)) {
        ret = OB_ERR_UNEXPECTED;
      } else if (OB_FAIL(stop_word_checker->check_stopword(ft_word, is_stopword))) {
      }
    }
  }
  return ret;
}

int ObAddWord::groupby_word(const ObFTWord &word, const int64_t word_freq)
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(word.empty() || word_freq <= 0)) {
    ret = OB_INVALID_ARGUMENT;
  } else if (!flag_.groupby_word()) {
    if (OB_FAIL(word_map_.set_refactored(word, 1/*word count*/))) {
    }
  } else {
    UpdateWordCountCallBack cb(word_freq);
    // Single hash path: insert or in-place increment.
    if (OB_FAIL(word_map_.set_or_update(word, word_freq, cb))) {
    }
  }
  return ret;
}

} // end namespace storage
} // end namespace oceanbase
