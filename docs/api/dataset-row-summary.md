# 按当前筛选条件统计数据集样本总行数

## 请求

```http
GET /api/v1/datasets/row-summary
Authorization: Bearer <access_token>
```

此接口统计**当前用户可见且满足筛选条件的全部数据集**，不只统计列表当前页。
不传令牌时仅统计公开仓库；登录后的私有仓库范围与数据集列表使用同一权限规则。
用户身份来自认证上下文，不能通过 `current_user` 查询参数冒充其他用户。

支持以下列表筛选参数：

| 参数 | 类型 | 含义 |
| --- | --- | --- |
| `search` | string | 搜索文本，与列表使用相同的匹配规则 |
| `source` | string | 来源：`opencsg`、`huggingface`、`local`；省略表示不限 |
| `tag_category` | repeated string | 标签类别，如 `task`、`language`、`license` |
| `tag_name` | repeated string | 标签名称，与 `tag_category` 按位置配对 |
| `tag_group` | repeated string | 标签分组，与标签参数按位置配对 |
| `task_tag`、`framework_tag`、`license_tag`、`language_tag` | string | 列表已有的标签筛选写法 |
| `compliance_status` | string | `compliant`、`pending_review`、`non_compliant` |
| `dataset_type` | string | `commercial`、`normal` |
| `user_purchased` | boolean | `true` 时沿用列表的已购买筛选规则 |
| `xnet_migration_status` | string | `pending`、`running`、`completed`、`failed` |
| `repo_size_min`、`repo_size_max` | int64 | 仓库大小的字节数范围，必须非负且最小值不大于最大值 |

`page`、`per` 不影响统计。`sort` 不影响统计结果，若传入仍按列表允许值校验。
多标签采用列表现有的组合规则；重复匹配的同一个数据集只累计一次。
统计不受列表分页计数上限或搜索结果展示上限限制。排序与推荐排名不作为统计筛选条件。

```bash
curl --get 'http://127.0.0.1/api/v1/datasets/row-summary' \
  --header "Authorization: Bearer $CSGHUB_TOKEN" \
  --data-urlencode 'search=chat' \
  --data-urlencode 'source=local' \
  --data-urlencode 'tag_category=language' \
  --data-urlencode 'tag_name=zh'
```

## 响应

```json
{
  "msg": "OK",
  "data": {
    "total_rows": "123456789",
    "total_datasets": 12,
    "counted_datasets": 10,
    "uncounted_datasets": 2,
    "complete": false
  }
}
```

| 字段 | 含义 |
| --- | --- |
| `total_rows` | 已知样本行数之和。为避免浏览器丢失 int64 精度，使用**十进制字符串**返回 |
| `total_datasets` | 满足筛选及权限条件的全部数据集数量 |
| `counted_datasets` | 当前 DataViewer 已完成，且有有效非负 `rows_num` 的数据集数量 |
| `uncounted_datasets` | 尚无可靠行数的数据集数量，包括未解析、处理中、失败或元数据异常 |
| `complete` | 所有匹配数据集是否都有可用统计 |

没有匹配项时返回 `total_rows: "0"`，三个数据集计数均为 `0`，`complete: true`。
有效的零行数据集属于已统计数据集。未统计的数据集不能理解为零行；
`complete: false` 时，`total_rows` 只是可用统计的合计，尚不是所有匹配数据的完整行数。

## 统计口径和刷新

- 数据来自 DataViewer 当前完成任务保存的 `card_data.rows_num`，覆盖其解析的所有 subset/split。
- DataViewer 的扫描与转换限制仍生效：默认最多扫描 5000 个文件，转换范围上限为 5 GiB。`complete` 仅说明匹配数据集都有可用统计，不保证源仓库的全部文件均已解析；它不是全量覆盖率标志。解析失败恢复及配置见 [DataViewer 行数统计恢复](../zh-CN/dataviewer-recovery.md)。
- 不下载仓库文件，不遍历样本，不在每次筛选时重新运行解析任务；数据库一次查询筛选出的元数据，再逐条累计。
- 按数据集累计，不对不同仓库之间相同的样本做去重；同一数据集的 subset/split 如果内容重叠，沿用 DataViewer 的行数口径。
- 文件发生变化后，须待 DataViewer 更新完成，新的行数才会进入统计。因此“实时”指筛选变化后立即重新请求当前已持久化的统计，不是实时扫描文件。
- `/datasets` 页面在标题右侧、搜索框左侧显示“当前数据集总行数为：…”；使用与列表相同的搜索、标签和来源条件。搜索框按 Enter 应用搜索后刷新统计。
- 翻页不重新计算总数；快速切换条件时忽略过期响应。加载中显示“统计中…”，失败显示“统计失败”和重试按钮，不保留上一组条件的数字。
- 未统计的数据集数量会在总数旁明确提示。

## 错误

非法来源、排序或仓库大小范围返回 `400`；权限范围解析、数据库查询或累计溢出失败返回 `500`，不会回退成跨权限统计或伪装成零。
鉴权错误沿用服务端既有中间件响应。
