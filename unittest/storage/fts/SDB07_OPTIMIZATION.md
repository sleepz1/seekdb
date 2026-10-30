# SDB-07 全文索引调优：瓶颈定位、修改说明与性能对比

基线：`5d7919353d73cbbd0012c777b9beb22abf5692eb`（此前官方最高分版本）。
本说明对应分支 `contest/sdb07-claude`；内核改动全部位于 `src/`，未修改
`tools/benchmark/fts_large_bench*`、测试数据、SQL、计时逻辑、评分脚本或基线。

## 1. 瓶颈定位依据

在 GitHub 同一 runner 上对基线与候选分别编译 Release、运行官方
`fts_large_bench.sh`，并在候选上对建索引、TOKENIZE 与 MATCH 查询采集
`perf record -g`（`sdb07_claude_bench.sh`，证据分支 `evidence/sdb07-claude-<run_id>`）。

* **建索引是 CPU 密集型**：DDL 调度固定开销约 150ms；两个 PX 子任务（word-doc 与
  doc-word 辅助表）各自 dop=1 并发执行。CPU 分布约为：分词 20%、排序 30%、宏块编码写入
  17%、排序落盘/压缩其余。排序中的字符串比较占比最高：
  `ObNullSafeDatumStrCmp<utf8mb4_general_ci>` + `ob_strnncollsp_utf8mb4` + `ob_mb_wc_utf8mb4`
  + `ObCharset::get_charset/is_argument_valid` 合计约 15%，`binary` 比较约 2%。
* **IK 分词**：每个解析器实例重复加载/构建主词典、量词词典与停用词结构；
  `casedown` 对每个 token 走完整的 unicase 转换，即使 token 已全是小写 ASCII 或汉字。
* **查询**：`COUNT(*) ... WHERE MATCH()` 的计划把 `BOOL(relevance)` 作为下推过滤，
  DaaT 迭代器对每个候选文档做一次通用表达式求值。

## 2. 主要修改及影响

| 修改 | 文件 | 影响 |
| --- | --- | --- |
| IK 词典（主词典、量词词典等）在进程内共享，解析器实例只持有只读引用；退出时在词典缓存销毁前释放 | `ob_ik_ft_parser.*`、`ob_fts_parser_helper.cpp`、`ob_server.cpp` | 建索引和 TOKENIZE 不再为每个解析器实例重复准备词典 |
| 停用词检查快路径：内置停用词均为短 ASCII 词，纯 ASCII token 做大小写无关的精确比较，含 3/4 字节字符的 token 直接判非停用词，其余仍走原 hash 集合 | `ob_fts_stop_word.cpp` | 分词每 token 少一次字符集转换与 hash 查找 |
| 分组计数单次 hash 探测（`set_or_update` 代替 get + 覆盖写）；命中时同时用最新 token 覆盖键，保持原覆盖写“同 collation 等价的不同拼写保留最后一次出现”的语义（如希腊词尾 σ/ς） | `ob_fts_stop_word.cpp` | 每 token 少一次 hash 与比较 |
| 已是小写的 ASCII 与格式正确的 U+4000–U+9FFF 汉字跳过 casedown（这些码位在 `ob_unicase_default` 中无大小写映射，结果逐字节不变） | `ob_fts_stop_word.cpp` | 分词与建索引减少逐字符转换 |
| `utf8mb4_general_ci` 比较内联前缀快路径：ASCII 与 3 字节汉字直接查 collation 自身的 sort 表，遇到其他字符或较短串结束时把剩余后缀交回 `ObCharset::strcmpsp`；`binary` 直接 memcmp；字节完全相同的串直接判等 | `ob_datum_cmp_func_def.h`、`ob_charset.cpp` | 建索引排序比较大幅减负 |
| `BOOL(relevance)` 过滤在 DaaT 中直接判断 relevance 是否非零，与表达式求值结果相同 | `ob_sparse_daat_iter.cpp` | 计数类查询每文档少一次表达式求值 |

**评估后未采用**：提高 FTS 辅助表构建 dop。它会改变索引块布局，使 BMW top-k 在同分
文档上的返回顺序与基线不同（语义对比出现 DIFF），因此保持 `decide_parallelism` 不变。

## 3. 正确性验证

* `test_contest_semantics.py`：基线与候选在同一 runner 上分别抓取完整输出逐项比对，
  覆盖 4 种分词器的 TOKENIZE（含明细输出）、自然语言/布尔模式 MATCH 的计数、ID、分数、
  阈值、LIMIT、计划复用与多种 SQL 形态；并针对本次改动补充：
  * `cmp/*`：utf8mb4_general_ci 与 binary 列上的排序、降序、GROUP BY、DISTINCT 与两两
    比较（大小写、全角、重音、ß/ẞ、希腊/土耳其字母、emoji、增补平面汉字、尾部空格/制表符/全角空格、长前缀等）；
  * `cmp-fts/*`：在上述数据上用 ik/beng/ngram/space 建全文索引并查询（覆盖建索排序路径）；
  * `casedown/*`：大小写混合的 ASCII/汉字/全角/特殊大小写字母 TOKENIZE；
  * `boolfilter/*`：MATCH 与 OR/NOT/AND/IS TRUE/布尔负词/LIMIT 组合。
  * 回归发现：`casedown/{beng,ngram,space}/6`（`ΣΊΣΥΦΟΣ σίσυφος`）曾暴露分组键保留首次拼写的差异，
    已修复为与基线一致（保留最后一次拼写）。
* `sdb07_utf8mb4_cmp_equiv.cpp`：600 万组随机串（含非法 UTF-8、截断多字节、代理区、
  增补平面字符）对比内联前缀算法与原 `ob_strnncollsp_utf8mb4`，两种尾空格模式，要求 0 差异。
* 原生 mysqltest：`fts_index/tokenize_function`、`fts_index/parser_properties` 均 PASS。

## 4. 优化前后性能对比（同一 runner，CI run 41034676026）

| 指标 | 基线 5d791935 | 候选 | 变化 |
| --- | --- | --- | --- |
| build_ik_all_sec（单次 / 3 次重建均值） | 8.699 / 5.476 | 5.590 / 4.476 | −36% / −18% |
| build_ik_content_sec | 6.460 / 4.673 | 4.751 / 3.837 | −26% / −18% |
| build_beng_en_sec | 3.040 / 2.431 | 2.546 / 1.930 | −16% / −21% |
| tokenize_ik_avg_ms | 0.1887 | 0.1761 | −6.7% |
| tokenize_beng_avg_ms | 0.1247 | 0.1227 | −1.6% |
| query_cn_avg_ms | 1.9065 | 1.7431 | −8.6% |
| query_beng_avg_ms | 2.9038 | 2.4417 | −15.9% |
| query_mixed_avg_ms | 2.2359 | 1.8499 | −17.3% |
| query_limit_avg_ms | 2.7897 | 2.4437 | −12.4% |

查询命中数（8001 / 11000 / 7332 / 20）与基线一致。单次建索引受 runner 噪声影响较大，
因此额外记录了 3 次稳态重建均值。

## 5. 官方评测观察

官方明细显示，多次提测中会随机出现某一项建索引多出约 3～3.5s 的情况
（如 44c0ff5 的 BENG 6.279s、opt3 的 IK 全列 9.868s、483f1bbc 的 BENG 6.233s），
与改动内容无关；同机 CI 未复现。
