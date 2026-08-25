# 贡献指南

感谢改进 Brclio Mail。当前项目是 Preview；提交应优先提高正确性、安全性、可验证性和协议互操作，而不是扩大成熟度宣传。

## 开始前

- 安全漏洞不要创建公开 issue，按 [SECURITY.md](SECURITY.md)私下报告。
- 较大功能、数据库迁移、协议行为或 UI 改版先开 issue 说明用例、威胁、兼容性和测试计划。
- 不要在 issue、测试 fixture、截图、日志或 commit 中提交真实邮件、地址、token、证书、私钥或备份。
- 邮件协议行为应引用对应 RFC/官方文档，并区分 MUST/SHOULD 与本项目策略。

## 开发环境

CI 使用 Go `1.26.6`。本地还需要 Git；容器变更需要 Docker Engine 与 Docker Compose v2。

```bash
go mod download
make fmt-check
make test
make test-race
make vet
make vuln
make build
```

仅本地 Web 开发可使用：

```bash
make run-dev
```

开发模式允许降低 TLS 要求，只能绑定回环/隔离网络，不能暴露公网。邮件协议变更必须补充真实 listener 或协议级测试，不能只测 HTTP。

## 代码与测试要求

- 使用 `gofmt`，保持 `go vet ./...`、`go test ./...` 和 `go test -race ./...` 通过；
- 新行为应有正常、拒绝、边界和并发/事务测试；
- 数据库迁移必须可在现有库上原子执行，并包含备份/回滚说明；
- 不得削弱开放中继防护、From 所有权、TLS、归档原因门槛或审计写入；
- 错误和日志不得泄露密码、token、原始邮件正文、Bcc/envelope 元数据或完整远端响应；
- 新配置项要有安全默认值，并同步 `.env.example`、README 和相关文档；
- UI 必须保持键盘可用、清晰焦点、语义标签和合理窄屏行为；
- 不要提交生成的数据库、data、backup、secret 或本地测试输出。

执行容器相关核验：

```bash
docker compose config
docker build -t brclio-mail:contributor-test .
```

## 许可与素材来源

本仓库是文件级双许可：

- 后端、协议、基础设施和文档默认以 `AGPL-3.0-or-later` 提交；
- [NOTICE](NOTICE) 列出的 Esther 衍生 UI/设计文件以 `CC-BY-NC-SA-4.0` 提交。

提交贡献即表示你拥有授予目标文件相应许可所需的权利，并同意贡献按该许可分发。对 UI、图片、字体、图标或其他素材，PR 必须说明原创/生成/第三方来源、许可、作者署名和修改记录。不要提交许可不明素材；不要把 NC 素材移动进声称可商用的范围。

如果商业兼容 UI 是目标，应在独立目录/变更中提供完全独立创作的视觉层、完整来源证明和清晰许可映射，不能只改颜色或文件名后声称摆脱 Esther 衍生关系。

## Pull Request 清单

PR 描述请包含：

- 问题、用户影响和明确不在范围内的内容；
- 设计与安全权衡；
- 测试命令和结果；
- 数据库/配置/协议兼容影响；
- 文档与部署变化；
- UI 截图和键盘/窄屏核验（如适用）；
- 新依赖、素材来源与许可影响；
- 回滚步骤。

保持提交聚焦，不要夹带格式化整个仓库或无关重构。维护者可能要求把安全关键和视觉变更拆分。
