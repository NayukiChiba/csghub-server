# DataViewer 行数统计恢复

`/api/v1/datasets/row-summary` 汇总当前预览任务保存的 `card_data.rows_num`。
没有这个字段不代表零行；数据库中的 `Done` 也可能只是一次无可转换文件的扫描。

## 已修复的解析路径

- 转换在独立临时目录中执行，成功后替换旧 Parquet 分片。重试不复用半成品，不把残留分片重复计数；转换失败时保留上一份输出。
- JSON/CSV 多文件按列名合并。JSON 单对象读取上限默认 128 MiB，可通过 `OPENCSG_DATAVIEWER_MAX_JSON_OBJECT_SIZE` 调整；仍严格报告解析错误，不跳过坏记录来凑出行数。
- UTF-8 BOM 在下载的 JSON/JSONL/CSV 中移除。按行复制支持超过 64 KiB 的记录，并向上传播读取错误。
- 普通 Git 中的 Parquet 文件先以内容哈希存入对象存储，预览分支使用 LFS 指针；原分支和原数据不变。已有 LFS 文件继续复用原对象。
- 下载 LFS 文件优先使用实际 SHA-256 生成对象路径，并检查 HTTP 状态；404 响应不会作为数据写入缓存。
- Git 文件更新与创建一样分块传输，避免一次发送超过 gRPC 限制。
- 工作流在会话创建失败、会话取消或活动失败后，使用独立状态回写上下文；数据库写入错误会反馈给活动重试。

## 恢复顺序

1. 先备份解析状态元数据和应用配置，部署新二进制，保留旧镜像用于回滚。
2. 根据最新任务而非仅根据 `dataviewers.workflow_id` 分类，排除无本地仓库、无可转换文件的项。
3. 通过已有 DataViewer callback API 定向重触发任务，先验证小规模 CSV、JSONL 和 Parquet 仓库。callback 会向 Temporal 核对旧任务；仅当旧工作流已关闭或不存在时，旧 pending/running 行才会被标记失效。
4. 限制恢复并发，观察新任务、磁盘空间和接口统计。不要直接修改 `rows_num`，不要把失败任务标记为成功。
5. 对仍然失败的项保留原始原因，分别处理缺失对象、损坏格式和额外格式支持。

现有扫描文件数和转换字节数限制仍生效；此接口口径是 DataViewer 已解析的样本，不能据此声称绕过限制完成了整个源仓库的全量扫描。

DuckDB 参数参考：[JSON 读取](https://duckdb.org/docs/1.4/data/json/loading_json)、[COPY](https://duckdb.org/docs/1.4/sql/statements/copy)。本次以项目实际依赖的 Go DuckDB 驱动执行回归测试，而非仅依赖在线文档版本。
