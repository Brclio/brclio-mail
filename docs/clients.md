# 第三方邮件客户端

Brclio Mail 支持标准 IMAP4rev1 与 SMTP Submission。当前不支持 POP3、JMAP、Exchange ActiveSync 或 CalDAV/CardDAV。

## 推荐参数

假设服务器是 `mail.example.com`，用户是 `alice@example.com`：

| 用途 | 主机 | 端口 | 连接安全 | 身份验证 |
| --- | --- | ---: | --- | --- |
| 收信 IMAP | `mail.example.com` | 993 | SSL/TLS（隐式 TLS） | 普通密码 / PLAIN over TLS |
| 发信 SMTP | `mail.example.com` | 465 | SSL/TLS（隐式 TLS） | 需要身份验证 |
| 发信 SMTP 备选 | `mail.example.com` | 587 | STARTTLS，必须升级 TLS | 需要身份验证 |

- 用户名始终使用完整邮箱地址，如 `alice@example.com`；
- 推荐在网页邮箱“应用专用密码”中为每台设备创建独立密码；
- 服务器目前也接受主密码，但不建议在第三方客户端复用；
- 不要选择“无加密”“可选 STARTTLS”或端口 25 发信；25 是服务器间 SMTP，不提供客户端 AUTH；
- 证书主机名必须与客户端填写的 `mail.example.com` 完全匹配。

隐式 TLS 端口 465/993 的使用建议见 [RFC 8314](https://www.rfc-editor.org/rfc/rfc8314.html)，Submission 587 见 [RFC 6409](https://www.rfc-editor.org/rfc/rfc6409.html)。当前实现是 IMAP4rev1，协议基线见 [RFC 3501](https://www.rfc-editor.org/rfc/rfc3501.html)；尚未实现 RFC 9051 的完整 IMAP4rev2 能力。

## 创建与撤销应用专用密码

1. 登录网页邮箱。
2. 打开“应用专用密码”。
3. 使用可识别的名称，如“MacBook Thunderbird”或“iPhone Mail”。
4. 立即保存生成的密码；界面只在创建时显示明文。
5. 设备遗失或不再使用时，在网页端撤销对应密码，不必更改其他设备。

每台设备、每个客户端分别创建一个密码，便于审计和最小化泄露影响。管理员账号不应配置到日常邮件客户端。

## 常见客户端

Thunderbird、Apple Mail、Outlook 等都可选择“手动设置”并填写上表参数。DNS 中的 `_imaps._tcp` 和 `_submission._tcp` SRV 记录可辅助某些客户端发现，但并非所有客户端都会读取，因此必须核对最终端口和 TLS 选项。

客户端可能把“删除”分成标记 `\\Deleted`、移动到 Trash 和 EXPUNGE。对于已经收发或以非草稿导入的邮件，无论用户界面如何呈现，用户删除只移除自己的邮箱可见副本；规范化原始 MIME 仍保留在管理员归档中。尚未发送的草稿不进入管理员归档，最后一份草稿副本删除后会物理清理。部署者必须在为用户开户前披露这两种不同语义。

## 连通性排错

从客户端所在网络检查：

```bash
openssl s_client -connect mail.example.com:993 -servername mail.example.com -crlf
openssl s_client -connect mail.example.com:465 -servername mail.example.com -crlf
openssl s_client -starttls smtp -connect mail.example.com:587 -servername mail.example.com -crlf
```

检查输出中的证书链、有效期、主机名和 `Verify return code: 0 (ok)`。常见问题：

- **连接超时**：云安全组、本机防火墙、家庭/公司网络或运营商阻断端口；
- **证书不可信/主机名错误**：客户端填了邮件域而不是证书覆盖的邮件主机名，或静态证书链不完整；
- **535/认证失败**：用户名未使用完整邮箱、应用密码抄错或已撤销；
- **587 无法认证**：客户端没有先启用 STARTTLS；
- **发信进入队列但不离开**：未配置 smarthost、relay 参数错误，且 direct delivery 默认关闭；在管理员队列及 systemd journal（或可选 Docker logs）中查看脱敏错误。

不要通过关闭证书校验来“修复”客户端连接。
