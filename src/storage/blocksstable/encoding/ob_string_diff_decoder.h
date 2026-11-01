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

#ifndef OCEANBASE_ENCODING_OB_STRING_DIFF_DECODER_H_
#define OCEANBASE_ENCODING_OB_STRING_DIFF_DECODER_H_

#include "ob_icolumn_decoder.h"
#include "ob_encoding_util.h"
#include "storage/blocksstable/ob_data_buffer.h"
#include "ob_string_diff_encoder.h"

namespace oceanbase
{
namespace blocksstable
{

struct ObColumnHeader;
struct ObStringDiffHeader;

class ObStringDiffDecoder : public ObIColumnDecoder
{
public:
  static const ObColumnHeader::Type type_ = ObColumnHeader::STRING_DIFF;

  ObStringDiffDecoder();
  virtual ~ObStringDiffDecoder();

  OB_INLINE int init(
      const ObMicroBlockHeader &micro_block_header,
      const ObColumnHeader &column_header,
      const char *meta);

  virtual int decode(const ObColumnDecoderCtx &ctx, common::ObDatum &datum, const int64_t row_id,
      const ObBitStream &bs, const char *data, const int64_t len) const override;

  virtual int update_pointer(const char *old_block, const char *cur_block) override;

  virtual int batch_decode(
      const ObColumnDecoderCtx &ctx,
      const ObIRowIndex* row_index,
      const int32_t *row_ids,
      const char **cell_datas,
      const int64_t row_cap,
      common::ObDatum *datums) const override;


  void reset() { this->~ObStringDiffDecoder(); new (this) ObStringDiffDecoder(); }
  OB_INLINE void reuse();
  virtual ObColumnHeader::Type get_type() const { return type_; }

  bool is_inited() const { return NULL != header_; }
private:
  // batch_decode fast path: strings up to this size are rebuilt from a per-batch template
  static const int64_t MAX_TEMPLATE_STRING_SIZE = 256;
  static const int64_t MAX_DIFF_DESC_CNT = 256; // diff_desc_cnt_ is an uint8_t
  struct DiffRange
  {
    uint16_t fpos_; // position in the full string
    uint16_t ppos_; // position in the row's stored diff bytes
    uint16_t cnt_;
  };
  static OB_INLINE void fill_from_template(const char *tmpl, const uint16_t string_size,
      const DiffRange *ranges, const int64_t range_cnt, const char *cell_data, char *str)
  {
    if (16 == string_size) {
      MEMCPY(str, tmpl, 16);
    } else {
      MEMCPY(str, tmpl, string_size);
    }
    for (int64_t r = 0; r < range_cnt; ++r) {
      const DiffRange &range = ranges[r];
      if (range.cnt_ <= 8) {
        for (int64_t k = 0; k < range.cnt_; ++k) {
          str[range.fpos_ + k] = cell_data[range.ppos_ + k];
        }
      } else {
        MEMCPY(str + range.fpos_, cell_data + range.ppos_, range.cnt_);
      }
    }
  }

private:
  const ObStringDiffHeader *header_;
};

OB_INLINE int ObStringDiffDecoder::init(
    const ObMicroBlockHeader &micro_block_header,
    const ObColumnHeader &column_header,
    const char *meta)
{
  UNUSEDx(micro_block_header, column_header);
  // performance critical, don't check params, already checked upper layer
  int ret = common::OB_SUCCESS;
  if (is_inited()) {
    ret = common::OB_INIT_TWICE;
    STORAGE_LOG(WARN, "init twice", K(ret));
  } else {
    meta += column_header.offset_;
    header_ = reinterpret_cast<const ObStringDiffHeader *>(meta);
  }
  return ret;
}

OB_INLINE void ObStringDiffDecoder::reuse()
{
  header_ = NULL;
  /*
  obj_meta_.reset();
  micro_block_header_ = NULL;
  col_header_ = NULL;
  header_ = NULL;
  */
}
} // end namespace blocksstable
} // end namespace oceanbase
#endif // OCEANBASE_ENCODING_OB_STRING_DIFF_DECODER_H_
