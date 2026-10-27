# CI 构建与排错

## Swagger 是编译前置条件

API 启动代码会导入 `opencsg.com/csghub-server/docs`。该包由 Swagger 工具生成，
`docs/docs.go`、`docs/swagger.json` 和 `docs/swagger.yaml` 不提交到 Git。

`make build`、`make test`、`make lint` 和 `make cover` 会先执行 `make swag`。
默认通过 `go run` 调用与 `go.mod` 对应的 `swag v1.16.6`，不要求预先安装全局 `swag`。
第一次运行需要下载工具依赖；如已有固定版本工具，也可传入 `SWAG=/path/to/swag`。

```sh
make build
make test
make lint  # 需要安装 golangci-lint；CI 固定使用 v2.15.0
```

直接执行 `go test ./...`、`go build` 或 `golangci-lint run` 时，须自行先运行 `make swag`。
测试使用 Docker 启动隔离数据库和 Redis，需要本机 Docker 可访问。

GitHub Actions 的每个 job 使用独立工作目录。因此独立的 Swagger 检查成功，
并不意味着 Test、Lint job 中已有生成文件。这些 job 必须各自准备编译依赖。
三个 Dockerfile（CE、EE、SaaS）也会在编译前生成 Swagger。

遇到 `no required module provides package opencsg.com/csghub-server/docs` 时，
应检查生成步骤，而不是尝试 `go get` 当前仓库的 `docs` 包。

## 可选评论审核

`Auto Moderation for Issues and Comments` 依赖仓库 Secret `LLM_API_KEY`。
未配置、空值或只有空白字符时，工作流会输出明确的跳过提示，
不安装 Python 依赖、不调用模型，也不修改 Issue 或评论。
配置有效值后，保留原来的审核流程；配置检查不会打印密钥。

`issues` 和 `issue_comment` 事件使用仓库默认分支中的工作流。
如果默认分支仍是 `main`，需将该工作流修复合入 `main` 才会对新事件生效；
只更新 `github-dev` 或 `develop` 不会改变这些事件的执行版本。
