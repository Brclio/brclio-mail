# 文档索引

Brclio Mail 当前是 **Preview**。先按实际服务器选择一篇独立教程：

1. [宝塔快速部署（推荐首次使用）](tutorial-baota-quick.md)：7 步完成安装、中继、启动、域名和客户端。
2. [宝塔完整版](tutorial-baota.md)：高级证书、排错、计划备份、升级与卸载。
3. [命令行与一键部署](tutorial-command-line.md)：SSH 一次粘贴安装与完整手动流程。
4. [Docker Compose 部署](tutorial-docker.md)：可选的单机容器路线、volume、备份与升级。

然后按顺序阅读通用参考：

1. [部署与 TLS](deployment.md)：systemd 安装、ACME/静态证书、防火墙、端口与生命周期。
2. [DNS 配置](dns.md)：A/AAAA、MX、PTR、SPF、DKIM、DMARC、TLS-RPT、SRV 与核验命令。
3. [第三方客户端](clients.md)：IMAPS 与 SMTP Submission 参数。
4. [运维、备份与恢复](operations.md)：日志、健康检查、一致性备份、恢复和升级/回滚。
5. [架构](architecture.md)与[威胁模型](threat-model.md)：数据边界、删除语义、安全能力和责任边界。
6. [限制与路线图](limitations-roadmap.md)：上线前必须了解的缺口。

若文档和程序行为不一致，请把它视为缺陷并按[贡献指南](../CONTRIBUTING.md)报告，不要以更乐观的文档描述代替实际核验。
