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

## 7. 后续轮次的改动（c482bd97 → 5610e69c → 本轮）

| 修改 | 文件 | 依据与影响 |
| --- | --- | --- |
| 只需要成员判断（`COUNT(*)`/`BOOL` 过滤，不算 BM25）的 MATCH 扫描只读倒排表的 doc_id 列 | `ob_tsc_cg_service.cpp`、`ob_text_retrieval_token_iter.*` | 计数查询的 profile 中解码 doc_length/word_count 等列占比明显；结果行集合不变 |
| 所有 TOKENIZE 调用点使用免锁的词表 map 类型；IK 词典/区间字典热路径小优化 | `ob_expr_tokenize.cpp`、`ob_ft_dat_dict.cpp`、`ob_ft_range_dict.cpp`、`ob_ik_arbitrator.h` | 分词每 token 少一次加锁与查找开销 |
| **top-k 穷举归并**：BMW top-k（`ORDER BY MATCH ... LIMIT k`）在 1～3 个查询词、估算倒排总长 ≤64K、查询词权重与字段权重均为 1.0 时，对 doc_id 做线性多路归并并逐文档算分，用与 BMW 相同的 top-k 堆与比较规则 | `ob_text_daat_iter.*`、`ob_text_retrieval_token_iter.h` | BMW 的块级跳跃在短倒排上收益小、维护开销大；结果的 ID、分数与顺序与 BMW 一致（语义用例 `topk-ids/scores/order`）。权重不为 1.0 时 BMW 的上界缩放与逐文档分数不一致，因此不走穷举 |
| 提前计算各查询词的估算文档数（原来在读第一条倒排时才惰性计算，导致上面的长度判断永远不成立） | `ob_text_retrieval_token_iter.h`、`ob_text_daat_iter.cpp` | 使穷举路径真正生效；CI 41230125090 中 `query_limit` 1.727ms，对照 2.452 / 2.573ms（−31%） |
| 异步系统包加载在每个包之间让出给正在执行的用户 DDL（`ObDDLScheduler` 任务数为 0 持续 500ms 才继续，单包最多等 60s，服务停止即退出），并在服务启动时立即开始而不是等第一次定时器 | `ob_system_package_load_task.*`、`ob_pl_package_manager.*`、`ob_ddl_scheduler.h` | CI 41224195571 中包加载与基准的首轮建索引重叠，build_total 约 11s（对照 7.3s）；加让出后 CI 41230125090 的建索窗口内没有包加载日志 |
| TOKENIZE 未指定 parser 属性时，默认属性的解析结果按线程缓存 | `ob_expr_tokenize.cpp` | 每次调用少一次 JSON 解析 |
| 排序比较直接访问 collation / 比较函数数组（一次性校验范围） | `ob_sort_op_impl.cpp` | 建索引排序每次比较少两次带边界检查的虚调用 |
| **本轮**：`ObStringDiffDecoder::batch_decode` 每批先把公共字节排成模板、把差异位置合并成区间，每行只拷贝一次模板再写差异字节（原实现每行两次遍历差异描述符、每个描述符一次 memcpy）；十六进制打包和超过 256 字节的串仍走原路径 | `ob_string_diff_decoder.*` | 倒排表 doc_id 列（16 字节，string-diff 编码）的解码在 `query_cn` profile 中自身占 19%、`query_limit` 中 14%。输出字节与原实现逐字节相同（本地 200 万组随机描述符对比 0 差异） |
| **本轮**：`ObDocIdExt::from_datum` 内联到头文件（16 字节定长拷贝）；倒排游标与批量保存改为直接数组访问；16 字节 doc_id 比较用两个大端 64 位字比较（与 memcmp 的符号一致，本地 2000 万组随机对比 0 差异）；投影时 evaluated 标志用 `set_all(count)` 一次置位 | `ob_das_id_protocol.h`、`ob_text_retrieval_token_iter.*`、`ob_text_daat_iter.*`、`ob_sparse_daat_iter.cpp` | 这些函数在计数查询 profile 中合计约 12%（`from_datum` 4.2%、`ObBitVector::set` 3.4%、数组 `at()` 3.3% 等） |

## 8. 正确性验证（续）

* 语义用例扩展到 413 项：新增 `content` 列（`fti_content_ik`）上的 `倒排索引 tokenizer`、`数据库` 等查询，
  以及 top-k 的 ID 集合、分数、顺序三种形态；CI 41230125090 中基线 c482、候选、基线复跑三者 413 项完全一致。
* 600 万组 utf8mb4 比较等价测试、`tokenize_function` / `parser_properties` mysqltest 每轮均通过。

## 9. 官方评测与同机对比（续）

| 提交 | 官方分数 | 说明 |
| --- | --- | --- |
| 5ed1fb52 | 12.767 | 词典共享、比较快路径、dop=2、DDL 轮询 |
| b65f9490 | 23.498 | + 线性 DaaT 归并 |
| 11125bf9 | 17.310 | + 分词热路径；本次 IK 正文建索出现 7.791s 的单次停顿（同机未复现） |
| c482bd97 | 30.609 | + doc_id-only 扫描、免锁词表 |
| 5610e69c | 32.409 | + top-k 穷举归并、包加载让出、TOKENIZE 属性缓存、排序比较；`query_limit` 1.4311→0.9625ms（−33%），另外三条查询 +6%～7%，建索/分词持平 |
| d5565d28 | 45.202 | + string-diff 模板解码、16 字节 doc_id 内联拷贝/比较；官方四条查询 0.351/0.490/0.377/0.782ms，比 5610e69c 快 19%～34%，建索/分词在波动范围内 |

CI 41230125090（同一 runner，基线 c482 跑两遍）：

| 指标 | 基线 c482 base / base2 | 候选 5610e69 | 说明 |
| --- | --- | --- | --- |
| build_ik_all_sec（单次） | 8.666 / 5.947 | 6.216 | 首轮建索受启动期后台任务影响大；3 次重建均值 4.42 / 4.35 vs 4.31s |
| build_ik_content_sec | 4.555 / 4.738 | 4.553 | 持平 |
| build_beng_en_sec | 2.418 / 2.334 | 2.280 | 持平 |
| tokenize_ik / beng (ms) | 0.1836 / 0.1841，0.1402 / 0.1388 | 0.1816，0.1371 | 持平 |
| query_cn / beng / mixed (ms) | 1.040 / 1.021，1.441 / 1.440，1.083 / 1.074 | 1.079，1.513，1.115 | +3%～5%，在该 runner 的波动范围内 |
| query_limit (ms) | 2.452 / 2.573 | 1.727 | −31%（穷举归并生效） |

**噪声说明**：首轮建索引（尤其 `build_ik_all`）与服务启动期的后台任务重叠——tablet GC 的首次
`ObTabletChangeTask` 会持久化约 365 个 bootstrap tablet（一次 MDS mini merge 风暴，约 1.2 万行日志与大量
slog/IO），在 CI 中落在第一个建索引开始后约 1s；官方结果中单项建索引偶发多出 2～3.5s 也与此类启动期
任务有关，并非改动导致。`sdb07_claude_bench.sh` 现在会额外输出 `startup-timeline.txt`（ready 时刻、
建索窗口，以及建索结束前的 tablet change / 包加载 / 启动里程碑日志）用于定位。

## 10. 第 17 轮：查询解码热路径的同机结果（CI run 41266737225，基线 5610e69 跑两遍）

| 指标 | 基线 base / base2 | 候选 d5565d28 | 变化 |
| --- | --- | --- | --- |
| query_cn (ms) | 1.0957 / 1.0510 | 0.6656 | −38% |
| query_beng (ms) | 1.5096 / 1.4615 | 0.8544 | −43% |
| query_mixed (ms) | 1.1398 / 1.1418 | 0.7134 | −37% |
| query_limit (ms) | 1.7038 / 1.7040 | 1.2985 | −24% |
| tokenize_ik / beng (ms) | 0.1850 / 0.1849，0.1377 / 0.1366 | 0.1806，0.1383 | 持平 |
| 建索重建均值 ik_all / ik_content / beng (ms) | 4339 / 4461，3676 / 3745，1912 / 1844 | 4464，3672，1951 | 持平（噪声内） |

413 项语义三者一致，600 万组比较等价 0 差异，两项 mysqltest 通过。

## 11. 本轮改动（d5565d28 之后）

| 修改 | 文件 | 依据与影响 |
| --- | --- | --- |
| **回退**包加载“让出给用户 DDL / 启动即加载”（恢复 c482bd97 的行为） | `ob_system_package_load_task.*`、`ob_pl_package_manager.*`、`ob_ddl_scheduler.h` | 官方 5610e69c 中建索引没有变快（5.187/3.935/2.067s vs c482 的 5.247/3.924/2.041s），三条计数查询反而慢 6%～7%；让出会把后台包加载推迟到 TOKENIZE/MATCH 阶段。收益未得到证实，改回官方已测过的行为 |
| **请求队列短自旋**：刚处理完一个请求的工作线程在阻塞到 futex 之前，最多 100µs 轮询请求队列（每 32 次 `PAUSE` 让出一次 CPU，同一时刻最多一个线程自旋） | `ob_server_runtime.*`、`ob_th_worker.cpp` | TOKENIZE 的 profile 中约 10% 是 futex 唤醒/上下文切换；单连接逐条执行时，下一条请求通常在几十微秒内到达。与 SQL 语义无关，对所有短请求（含 SELECT 1）同样生效。实验 CI 41267131098：tokenize_ik 0.2137/0.2124→0.1944ms（−9%），tokenize_beng 0.1629/0.1642→0.1391ms（−15%），3 个采样组全部低于基线；查询持平；413 项语义一致 |
| **DDL 等待期的健康检查降频**：索引构建子任务等待已提交的补数据作业、FTS 父任务等待子任务时，`check_health` 中重读任务记录（`SELECT … FOR UPDATE` 事务）以及父任务对子任务错误信息表的查询，从每 10ms 一次改为最多每 100ms 一次；作业/子任务完成仍在每轮的内存检查中发现（`check_local_build`、`on_child_task_finish`） | `ob_ddl_task.*`、`ob_index_build_task.cpp`、`ob_fts_index_build_task.*` | 建索引 profile 中 DDL 调度线程占全部 CPU 采样约 5%（`check_health`、`get_ddl_error_message`、`select_for_update`），与 PX 建索线程争用 CPU；完成检测延迟不变，只有外部取消的发现可能晚至 100ms |

CI 41271498726（同一 runner，基线 d5565d28 跑两遍）验证上表三项：413 项语义完全一致；tokenize_ik / beng
0.0659 / 0.0657、0.0440 / 0.0481 → 0.0615、0.0397ms（−7% / −14%），select1 0.033→0.0255ms；四条查询持平或略快；
建索 3 次重建均值持平（健康检查降频为中性）。本次 base2 与候选的首轮 ik_all 都出现约 9.5s 的启动期停顿（base 为 4.9s），
与代码无关。

## 12. TOKENIZE 结果打包与解析器属性（f39b4553 之后）

TOKENIZE 的 profile（`tokenize_ik`，CI 41271498726 候选）中：JSON 结果打印占 8.15% 采样（其中
`ObJsonBin::get_array_element` 4.22%、`create_new_binary` 2.53%——打印二进制数组时每个元素都复制一份容器和
节点栈），`ObFTParseHelper::init` 3.09%（每次调用都重新解析同一个属性 JSON），`pack_json_res` 3.47%。

| 修改 | 文件 | 依据与影响 |
| --- | --- | --- |
| **JSON 打印复用子节点**：`ObIJsonBase::print_array/print_object` 遇到二进制容器时，用一个复用的 `ObJsonBin` 子对象依次定位到每个元素（新增 `ObJsonBin::get_child_for_read`，内部即原有的 `get_value`），不再为每个元素克隆容器与节点栈；非二进制（tree）路径不变 | `ob_json_bin.*`、`ob_json_base.cpp` | 输出字节与原实现一致（413 项语义含 TOKENIZE 的默认/`detail`/对象模式输出，全部一致；`tokenize_function` mysqltest 通过） |
| **解析器属性解析结果按线程缓存**：`ObFTParserProperty::parse_for_parser_helper` 以（解析器类型，属性串 ≤1KB）为键缓存上一次的解析结果，命中时直接赋值解析出的字段；未命中或属性串过长时走原实现 `parse_for_parser_helper_uncached` | `ob_fts_parser_property.*` | 缓存的是属性配置的解析结果（与输入文本无关），不缓存任何分词结果；属性非法时照常报错且不写缓存；`parser_properties` mysqltest 通过 |

实验 CI 41272766847（同一 runner，基线 f39b4553 跑两遍；该 runner 整体比上一轮慢约 3 倍）：

| 指标 | 基线 base / base2 | 候选 | 变化 |
| --- | --- | --- | --- |
| tokenize_ik (ms) | 0.1956 / 0.1964 | 0.1729 | −12% |
| tokenize_beng (ms) | 0.1420 / 0.1487 | 0.1390 | −2%～−7% |
| query_cn / beng / mixed / limit (ms) | 0.667 / 0.651，0.859 / 0.893，0.723 / 0.674，1.266 / 1.240 | 0.713，0.827，0.675，1.250 | 噪声内（不涉及查询路径） |
| 建索重建均值 ik_all / ik_content / beng (ms) | 4517 / 3767 / 1915 | 4225 / 3819 / 1902 | 持平 |

413 项语义三者一致，600 万组比较等价 0 差异，两项 mysqltest 通过。

### 官方评测：020a1afe（含第 11、12 节全部改动）= 41.007

| 指标 | d5565d28（45.202） | 020a1afe（41.007） | 变化 |
| --- | --- | --- | --- |
| build ik_all / ik_content / beng (s) | 5.608 / 4.416 / 2.221 | 5.175 / 3.923 / 2.081 | −8% / −11% / −6% |
| tokenize_ik / beng (ms) | 0.0615 / 0.0399 | 0.0516 / 0.0329 | −16% / −18% |
| query cn / beng / mixed / limit (ms) | 0.351 / 0.490 / 0.377 / 0.782 | 0.321 / 0.452 / 0.348 / 0.717 | −8%～−9% |
| select1 (ms) | 0.0268 | 0.0223 | −17% |

全部 9 项计分指标（以及 SELECT 1）的绝对耗时都比 d5565d28 低，但 `fts_mean_improvement` 由 0.452 降到 0.410，说明官方每次评测的基线是在
当次环境下重新测得的（本次环境整体更快，基线提升得更多）；不同提交之间的分数差异包含较大的环境波动，同一次评测内的
绝对指标比较更能反映改动本身。

## 13. TOKENIZE 结果打印：字符串元素原地输出（020a1afe 之后）

020a1afe 的 `tokenize_ik` profile（CI 41272766847 候选）中，结果集编码 `ObSMUtils::build_cell_value → ObIJsonBase::print`
仍占 19.5% 采样，与分词本身（`ObFTParseHelper::segment` 约 18%）相当：TOKENIZE 的输出是由大量短字符串组成的 JSON 数组，
每个元素都要先定位成子节点（`get_child_for_read`，解析元素头、建节点状态），再走通用 `print` 分派，最后
`add_double_quote` 逐字符检查转义。

| 修改 | 文件 | 依据与影响 |
| --- | --- | --- |
| **字符串元素原地打印**：新增 `ObJsonBin::print_plain_string_element(index, buf, handled)`，读取数组/对象第 index 个值的类型；若是 `J_STRING_V0`，直接按二进制布局（类型字节 + 变长长度 + 字节）取出字符串并加引号输出，`handled=true`；其他类型 `handled=false`，回退到原有 `get_child_for_read` + `print` 路径 | `ob_json_bin.*`、`ob_json_base.cpp`（`print_array` / `print_object`） | 只改变取值路径，不改变输出：空串仍输出 `""`，转义规则与原 `add_double_quote` 相同 |
| **`add_double_quote` 快速路径**：先扫描是否含需要转义的字符；不含时一次 `reserve` 后直接写入 引号 + memcpy + 引号；含转义字符时走原逐字符实现 | `ob_json_base.cpp` | 输出字节完全一致 |

实验 CI 41292608225（同一 runner，基线 d4f8911c 跑两遍）：

| 指标 | 基线 base / base2 | 候选 | 变化 |
| --- | --- | --- | --- |
| tokenize_ik (ms) | 0.0559 / 0.0606 | 0.0551 | −1%～−9% |
| tokenize_beng (ms) | 0.0411 / 0.0419 | 0.0384 | −7%～−8% |
| select1 (ms) | 0.0352 / 0.0280 | 0.0301 | 噪声内 |
| query_cn / beng / mixed / limit (ms) | 0.296 / 0.326，0.420 / 0.423，0.316 / 0.318，0.584 / 0.667 | 0.289，0.400，0.312，0.649 | 持平（不涉及查询路径） |
| 建索重建均值 ik_all / ik_content / beng (ms) | 2374 / 1998 / 1146，2521 / 2090 / 1191 | 2377 / 2097 / 1131 | 持平 |

候选的 `tokenize_ik` profile 中 `build_cell_value` 由 19.5% 降到 2.9%（其中新路径 `print_plain_string_element` 1.8%），
剩余热点为分词本身（`segment` 32%，词典查找 `ObArrayHashMap::find` 6.5%）与 JSON 树→二进制打包（`pack_json_res` 3.6%）。
413 项语义（含 TOKENIZE 默认 / `detail` / 对象模式输出）三者完全一致，600 万组比较等价 0 差异，`tokenize_function`、
`parser_properties` 两项 mysqltest 通过。

## 14. 首次建索偶发 +3s：系统包加载逐个取 id（c7d8bd80 之后）

官方评测 c7d8bd80 得 30.522：与 020a1afe（41.007）相比，TOKENIZE 与 MATCH 各项持平或更好，
差距全部来自 `build_beng_en` 5.002s（正常约 2.1s，多出约 3s）。历次官方结果里这类 +3s 的首轮建索停顿
在 ik_all / ik_content / beng 三项上都出现过，位置不固定。

用 CI 41292608225 的完整日志定位到原因（cand 的 `build_ik_all` 8.2s，而重建只需 2.4s）：

1. 服务启动约 3s 后，后台任务 `ObSystemPackageLoadTask` 加载 13 个系统包（只在新集群首次启动时执行一次）。每条
   `CREATE PACKAGE` 都通过进程级的 Root Service 串行锁（`serialize_root_service_call`）执行，并在 DDL 事务里持有
   运行时 DDL 锁。
2. 包里每个 package / routine 都要分配一个 sys PL object id，`OB_MAX_USED_SYS_PL_OBJECT_ID_TYPE` 不在
   `ObMaxIdCache` 的缓存类型里，每个 id 都是一次独立的内部表事务提交。启动后的 MDS mini merge、slog 写入
   让磁盘很忙，每次提交约 55ms；`dbms_stats` 的 spec 一条语句就取了 50 个 id，从 42.94 到 45.62，持锁 2.7s。
3. 基准的 `ALTER TABLE ... ADD FULLTEXT INDEX` 在 42.81 发出，排在同一把串行锁后面，到 45.78 才开始执行；
   之后 DDL 任务的 `create_aux_index` 又与后续包语句争运行时锁，等 2.1s 后锁超时（-4012），重试一次才继续。
   基线另一轮里包加载失败重试了两次（03.0 → 14.3），`ik_content` 的 ALTER 在串行锁上又等了 0.9s。
   包加载如果碰巧在建索前完成（base，21.21 → 21.43，共 0.22s），就不会出现停顿。

修改：`ObMaxIdCache` 增加 sys PL object id 缓存项，每次批量预留 128 个 id（其他类型仍为 1024；
sys PL id 只有 31 万到 50 万这一段，批量取小一些），`check_use_max_id_cache_` 把这一类型列为走缓存。
同一进程内分配到的 id 序列不变（仍从 310001 起连续）；没用完的 id 在重启后跳过，不会重复使用；缓存失败时
仍回退到原来的逐个取 id。这样一次包加载的 117 次 id 事务变成 1 次，每条包语句持有串行锁的时间
从“id 个数 × 一次提交”降为一两次提交。这是引擎通用路径的改动，不涉及全文索引的语义，
也不改变包加载的时机：包加载仍按原样在后台异步执行。

CI 41302360111（base / base2 = c7d8bd80，cand = 0845d6f；本次 runner 整体比之前慢约 3 倍，tokenize 0.18ms）：
语义对比 413 条全部与父提交一致，mysqltest `tokenize_function` / `parser_properties` 三个变体均 PASS。
日志确认 sys PL id 走缓存（从 310001 起一次预留），cand 的 ALTER 发出后立刻开始执行，不再排在串行锁后面。

| 指标 | base | cand | base2 |
| --- | --- | --- | --- |
| build ik_all (s) | 5.653 | 8.806 | 8.047 |
| build ik_content (s) | 4.483 | 5.005 | 4.766 |
| build beng (s) | 2.457 | 2.320 | 2.401 |
| tokenize ik / beng (ms) | 0.1832 / 0.1307 | 0.1776 / 0.1412 | 0.1747 / 0.1293 |
| query cn / beng / mixed / limit (ms) | 0.670 / 0.858 / 0.703 / 1.327 | 0.657 / 0.871 / 0.688 / 1.246 | 0.654 / 0.858 / 0.689 / 1.244 |

首轮 ik_all 在 cand 与 base2 上仍然偏慢，重建耗时三者一致。cand 日志显示剩下的停顿来自另一处：
包加载的 `dbms_ischeduler` 语句在等运行时 DDL 锁之后返回 -4023 并整体失败，3s 后从第一个包重新加载；
同时基准 DDL 任务的 `create_aux_index` 也因为包语句提交了新的 schema 版本而收到 -4023，重试两次（44.61 → 45.26）。
见第 15 节。

## 15. 系统包语句遇到并发 DDL 时原地重试（0845d6f 之后）

`ObDDLSQLTransaction::start` 在拿到运行时 DDL 锁后会复核 schema 版本：如果等锁期间别的 DDL 已经提交，
就返回 OB_EAGAIN（语句尚未修改任何东西）。用户会话里的 DDL 会被重试，但系统包加载走内部 SQL，
`read_and_exec_package_sql` 直接把 -4023 返回给 `load_sys_package_list`，整个包列表失败；
`ObSystemPackageLoadTask` 3s 后从第一个包开始全部重新执行 `CREATE OR REPLACE`。基准建索期间正好有大量 DDL
提交，因此这一轮又一轮的重试会把包加载拖得更长，每一轮都在和建索的 DDL 任务争同一把锁、相互触发 -4023。

修改：`ObPLPackageManager::exec_package_sql_with_retry_` 在单条包语句返回 OB_EAGAIN 时刷新 schema，
10ms 后原地重新执行同一条语句（最多 20 次，其他错误码与原来一样直接返回）。语句内容和执行顺序不变，
包加载仍按原来的时机在后台执行，只是不再因为一次并发冲突重做前面已经成功的包，减少了它与建索争锁的总时长。
