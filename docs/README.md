# 文档索引

Brclio Mail 当前是 **Preview**。建议按以下顺序阅读和执行：

1. [部署与 TLS](deployment.md)：Ubuntu/Debian、RHEL 系 systemd 首选部署，ACME/静态证书、防火墙、端口与可选 Docker Compose。
2. [DNS 配置](dns.md)：A/AAAA、MX、PTR、SPF、DKIM、DMARC、TLS-RPT、SRV 与核验命令。
3. [第三方客户端](clients.md)：IMAPS 与 SMTP Submission 参数。
4. [运维、备份与恢复](operations.md)：systemd 日志、健康检查、一致性备份、恢复、成对升级/回滚和 Docker 对照。
5. [架构](architecture.md)与[威胁模型](threat-model.md)：数据边界、删除语义、安全能力和责任边界。
6. [限制与路线图](limitations-roadmap.md)：上线前必须了解的缺口。

若文档和程序行为不一致，请把它视为缺陷并按[贡献指南](../CONTRIBUTING.md)报告，不要以更乐观的文档描述代替实际核验。
