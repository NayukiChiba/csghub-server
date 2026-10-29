# DataViewer 行数统计恢复

`/api/v1/datasets/row-summary` 汇总当前预览任务保存的 `card_data.rows_num`。
没有这个字段不代表零行；数据库中的 `Done` 也可能只是一次无可转换文件的扫描。

## 已修复的解析路径

- 转换在独立临时目录中执行，成功后替换旧 Parquet 分片。重试不复用半成品，不把残留分片重复计数；转换失败时保留上一份输出。
- 小型 JSON 输入按列名合并，扫描缓存范围内的全部记录来判断类型，避免后半段类型变化导致失败；日期和时间仅按固定格式推断，其他表示保留为字符串。CSV 按文件分别识别分隔符并转换，预览按列名合并；保留原始文本值，避免不同文件同名列的日期、整数等类型推断冲突。CSV 默认首行为表头，`OPENCSG_DATAVIEWER_CSV_HEADER=false` 可配置无表头文件，`auto` 保留 DuckDB 自动猜测行为；同为纯文本的表头与首条数据无法可靠自动区分，应按数据源约定选择。JSON 单对象读取上限默认 128 MiB，可通过 `OPENCSG_DATAVIEWER_MAX_JSON_OBJECT_SIZE` 调整；仍严格报告解析错误，不跳过坏记录来凑出行数。
- 为避免全量类型推断耗尽内存，只有总大小不超过 `OPENCSG_DATAVIEWER_JSON_INFERENCE_MAX_BYTES`（默认 64 MiB）的本地输入采用完整类型推断。更大或大小未知的远程 JSON 输入流式保留每条完整原始记录，输出单列 `record`，不做类型推断；JSON 数组和 JSONL 仍按实际记录计数，字段和值不丢弃，但预览列布局会改变。
- 大型本地 JSON 由 Go 解码器逐条校验并直接写入 Parquet，不再经过 DuckDB 的向量化 JSON 批次。原始记录列每累计约 8 MiB 或达到 2048 行刷新；单条较大的记录写完立即刷新，不会将几千条大记录同时保留在内存中。小文件类型推断遇到内存不足时，也回退到完整原始记录列；语法错误仍然报错。
- DuckDB 缓冲内存预算由 `OPENCSG_DATAVIEWER_CONVERSION_MEMORY_LIMIT` 控制，默认 2 GiB。此配置不是整个进程的 RSS 硬上限，也不限制 Go 写入器的所有分配；解析器、单条记录及压缩缓冲仍需监控。Go 路径在读取和解码时执行单对象大小上限，保留最多 64 KiB 分隔符/预读余量；记录大小仍严格受 `OPENCSG_DATAVIEWER_MAX_JSON_OBJECT_SIZE` 限制。对确实含大记录的数据可明确提高该值，不应直接取消上限。
- UTF-8 BOM 在下载的 JSON/JSONL/CSV 中移除。按行复制支持超过 64 KiB 的记录，并向上传播读取错误。
- 单文件压缩表格支持 `.json.gz`、`.jsonl.gz`、`.csv.gz` 及相应 `.zst` 后缀（大小写不敏感）。识别真实数据格式后，在本地缓存流式解压，再复用现有转换、上传和计数流程；不把 `.tar.gz` 或任意压缩包当成表格。
- 压缩来源必须完整下载并通过解码校验，不能按压缩字节截断。跨所有 subset/split 累计解压后的转换字节数，仍受 `OPENCSG_DATAVIEWER_CONVERT_LIMIT_SIZE` 限制；超限、损坏、取消时明确失败，不提交解压半成品。Zstandard 流式窗口上限为 128 MiB，使用现有依赖，没有升级版本。压缩 LFS 数据要求启用 `OPENCSG_DATAVIEWER_DOWNLOAD_LFS_FILE`。
- 扫描时排除 macOS 的 `._*`、`.DS_Store` 和 `__MACOSX` 附带文件；这些资源元数据可能沿用 `.jsonl`、`.parquet` 等后缀，但不属于数据样本，也不占用样本扫描字节预算。
- 普通 Git 中的 Parquet 文件先以内容哈希存入对象存储，预览分支使用 LFS 指针；原分支和原数据不变。已有 LFS 文件继续复用原对象。
- 下载 LFS 文件优先使用实际 SHA-256 生成对象路径，并检查 HTTP 状态；404 响应不会作为数据写入缓存。
- Git 文件更新与创建一样分块传输，避免一次发送超过 gRPC 限制。
- Parquet 仅计数请求直接读取文件元数据，不初始化行读取器；避免 NULL 逻辑列触发字典解码异常，也不为计数读取样本内容。实际样本预览仍受底层解码器支持范围限制。
- Parquet 嵌套字段通过文件 schema 判断类型，不把数字或含符号的合法 JSON 键转换为 Go 结构体字段名，避免反射 panic。
- 转换分片使用上传器要求的纯数字文件名；上传遇到异常分片名立即报错，不再静默忽略后返回零行。来源未变但旧结果缺少分片元数据时也会重建，避免长期复用错误的零行结果。
- 工作流在会话创建失败、会话取消或活动失败后，使用独立状态回写上下文；数据库写入错误会反馈给活动重试。

## 恢复顺序

1. 先备份解析状态元数据和应用配置，部署新二进制，保留旧镜像用于回滚。
2. 根据最新任务而非仅根据 `dataviewers.workflow_id` 分类，排除无本地仓库、无可转换文件的项。
3. 通过已有 DataViewer callback API 定向重触发任务，先验证小规模 CSV、JSONL 和 Parquet 仓库。callback 会向 Temporal 核对旧任务；仅当旧工作流已关闭或不存在时，旧 pending/running 行才会被标记失效。
4. 限制恢复并发，观察新任务、磁盘空间和接口统计。不要直接修改 `rows_num`，不要把失败任务标记为成功。
5. 对仍然失败的项保留原始原因，分别处理缺失对象、损坏格式和额外格式支持。

现有扫描文件数和转换字节数限制仍生效；此接口口径是 DataViewer 已解析的样本，不能据此声称绕过限制完成了整个源仓库的全量扫描。

DuckDB 参数参考：[JSON 读取](https://duckdb.org/docs/1.4/data/json/loading_json)、[COPY](https://duckdb.org/docs/1.4/sql/statements/copy)。本次以项目实际依赖的 Go DuckDB 驱动执行回归测试，而非仅依赖在线文档版本。

JSON 完整类型扫描仅用于大小受限的本地输入；大文件使用原始记录流式路径，不启用 `ignore_errors`。混合时间格式问题也可见 [DuckDB 上游问题说明](https://github.com/duckdb/duckdb/issues/14004)，实际行为以本仓库固定版本的回归测试为准。
