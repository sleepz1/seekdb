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
  DaaT 迭代器对每个候选文档做一次通用表达式求值。进一步对 `query_cn` 采样
  （CI run 41046215077 的 profile）：约 78% CPU 在 `ObSRDaaTIterImpl` 的多路归并本身
  （二叉堆 push/pop/rebuild、`is_unique_champion`、relevance collector、过滤、结果缓存、
  每行若干次虚函数和数组边界检查访问），倒排表扫描与 doc_id 解码约 21%。基准查询只有
  2～3 个查询词，堆维护的常数开销远大于真正的比较工作。
* **建索引并行度**：倒排与 doc-word 两个辅助表的构建计划为 dop=1，配置矩阵实验
  （CI run 41173894842）显示 dop=2 可再快约 10%～15%；客户端以 100ms 轮询 DDL 结果，
  建索耗时被向上取整到 100ms。

## 2. 主要修改及影响

| 修改 | 文件 | 影响 |
| --- | --- | --- |
| IK 词典（主词典、量词词典等）在进程内共享，解析器实例只持有只读引用；退出时在词典缓存销毁前释放 | `ob_ik_ft_parser.*`、`ob_fts_parser_helper.cpp`、`ob_server.cpp` | 建索引和 TOKENIZE 不再为每个解析器实例重复准备词典 |
| 停用词检查快路径：内置停用词均为短 ASCII 词，纯 ASCII token 做大小写无关的精确比较，含 3/4 字节字符的 token 直接判非停用词，其余仍走原 hash 集合 | `ob_fts_stop_word.cpp` | 分词每 token 少一次字符集转换与 hash 查找 |
| 分组计数单次 hash 探测（`set_or_update` 代替 get + 覆盖写）；命中时同时用最新 token 覆盖键，保持原覆盖写“同 collation 等价的不同拼写保留最后一次出现”的语义（如希腊词尾 σ/ς） | `ob_fts_stop_word.cpp` | 每 token 少一次 hash 与比较 |
| 已是小写的 ASCII 与格式正确的 U+4000–U+9FFF 汉字跳过 casedown（这些码位在 `ob_unicase_default` 中无大小写映射，结果逐字节不变） | `ob_fts_stop_word.cpp` | 分词与建索引减少逐字符转换 |
| `utf8mb4_general_ci` 比较内联前缀快路径：ASCII 与 3 字节汉字直接查 collation 自身的 sort 表，遇到其他字符或较短串结束时把剩余后缀交回 `ObCharset::strcmpsp`；`binary` 直接 memcmp；字节完全相同的串直接判等 | `ob_datum_cmp_func_def.h`、`ob_charset.cpp` | 建索引排序比较大幅减负 |
| `BOOL(relevance)` 过滤在 DaaT 中直接判断 relevance 是否非零，与表达式求值结果相同 | `ob_sparse_daat_iter.cpp` | 计数类查询每文档少一次表达式求值 |
| 服务器至少 4 核且用户未指定并行度时，倒排索引与 doc-word 辅助表以 dop=2 构建；负责分配 doc_id 的 rowkey-doc 与 doc-rowkey 保持原并行度（索引内容由排序键唯一决定，doc_id 分配不变） | `ob_fts_index_build_task.cpp` | 建索引排序/写入并行，同机重建均值 −20%～−29% |
| 等待 DDL 完成时前 30 秒每 10ms 查询一次结果，之后恢复 100ms | `ob_ddl_executor_util.cpp` | 去掉建索耗时的 100ms 取整等待 |
| **线性 DaaT 归并**：不计算 BM25（relevance 仅用于 `BOOL` 过滤或不需要）、非函数回查模式、1～8 个查询词、每个词贡献 `1.0×field_boost×weight` 为可精确表示的整数时，`ObTextDaaTIter` 用游标数组线性取最小 doc_id 代替二叉堆；游标推进内联（批内只做下标自增），无 LIMIT 时直接写入结果缓冲，`BOOL(relevance)` 直接判非零。条件不满足（需要分数、BMW top-k、词数多等）时走原堆归并 | `ob_text_daat_iter.*`、`ob_text_retrieval_token_iter.h` | 输出行集合、doc_id 顺序与 relevance 与堆归并逐位相同（整数加法与访问顺序无关）；计数查询 −30%～−39% |

**评估后未采用**：把全部 FTS 辅助表（含 rowkey-doc）一起提高 dop。它会改变 doc_id
分配与索引块布局，使 BMW top-k 在同分文档上的返回顺序与基线不同（语义对比出现 DIFF）；
因此只对不分配 doc_id 的倒排与 doc-word 表提高 dop。TOKENIZE 结果缓存等与基准重复调用
相关的捷径不采用。

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
* 分数可复现性：BM25 的总文档数来自优化器对 `docs` 的行数估计；未收集统计信息时，
  后台异步收集会在抓取过程中改变该值（同一版本前后两次查询分数从 1.19098545 变为
  1.19107008），导致分数对比偶发不一致。语义抓取开始前先 `DBMS_STATS.GATHER_TABLE_STATS`，
  并在日志记录收集前后与结束时的诊断分数；之后基线、候选、基线复跑三者 320 项完全一致
  （CI run 41210121485）。

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

## 6. 线性 DaaT 归并前后对比（同一 runner，CI run 41210121485）

基线为 `5ed1fb52`（上一官方提交），候选只增加线性 DaaT 归并；同一 runner 上基线跑两遍
（base / base2）以观察噪声。

| 指标 | 基线 base / base2 | 候选 | 变化（对两次均值） |
| --- | --- | --- | --- |
| query_cn_avg_ms | 1.8841 / 1.9654 | 1.3251 | −31% |
| query_beng_avg_ms | 2.7242 / 2.7445 | 1.6520 | −40% |
| query_mixed_avg_ms | 2.1235 / 2.0766 | 1.3123 | −38% |
| query_limit_avg_ms（BMW，未改） | 2.7163 / 2.7582 | 2.6655 | −3%（噪声） |
| tokenize_ik / beng (ms) | 0.2172 / 0.2128，0.1624 / 0.1617 | 0.2156，0.1643 | 持平 |
| 重建均值 IK 全列 / IK 正文 / BENG (ms) | 4745 / 4885，4004 / 3959，2046 / 2035 | 4922，3978，1958 | 持平 |

命中数 8001 / 11000 / 7332 / 20 不变；320 项语义、mysqltest、比较等价测试全部通过。
