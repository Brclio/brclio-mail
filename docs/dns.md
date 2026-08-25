# DNS、PTR 与发信信誉

以下示例假设邮件域是 `example.com`，服务器主机名是 `mail.example.com`。把它们完整替换为自己的值。先以较低 TTL（例如 300 秒）上线，稳定后再提高。

## 必需与推荐记录

| 类型 | 名称 | 示例值 | 说明 |
| --- | --- | --- | --- |
| A | `mail.example.com` | `203.0.113.10` | 固定公网 IPv4 |
| AAAA | `mail.example.com` | `2001:db8::10` | 只有 IPv6 双向可达时才发布 |
| MX | `example.com` | `10 mail.example.com.` | 入站 SMTP 目的地 |
| PTR | `203.0.113.10` | `mail.example.com.` | 在云/VPS 控制台设置，不在普通 DNS 区域设置 |
| TXT | `_brclio-mail.example.com` | 管理台提供的 token | 域名所有权验证 |
| TXT | `example.com` | SPF，见下文 | 授权合法出口 |
| TXT | `brclio._domainkey.example.com` | 管理台提供的 `v=DKIM1; ...` | 出站 DKIM 公钥；selector 可能不同 |
| TXT | `_dmarc.example.com` | `v=DMARC1; p=none; rua=mailto:dmarc@example.com; adkim=r; aspf=r` | 先监控，不阻断 |
| TXT | `_smtp._tls.example.com` | `v=TLSRPTv1; rua=mailto:tlsrpt@example.com` | SMTP TLS 汇总报告 |
| SRV | `_submission._tcp.example.com` | `0 1 587 mail.example.com.` | Submission 自动发现 |
| SRV | `_imaps._tcp.example.com` | `0 1 993 mail.example.com.` | IMAPS 自动发现 |

MX 目标必须是有 A/AAAA 的主机名，不能是裸 IP；不要让 MX 目标本身成为 CNAME。若发布 AAAA，但主机的 25/443/465/587/993 在 IPv6 上不可达，远端可能优先走失败的 IPv6，因此应先修复连通性或删除 AAAA。

## 先完成域名所有权验证

创建域名后，管理员控制台会给出随机 token：

```text
_brclio-mail.example.com. TXT "<管理台提供的 token>"
```

发布并等待权威 DNS 生效，然后在后台对该域点击检查。状态必须从 `pending` 变为 `verified`。管理员可以在 pending 阶段预先创建用户/别名，但系统会拒绝该域通过公网 SMTP 收信或通过 Submission 发信，也会拒绝从 Web 向外域发信/进入外发队列。验证记录不能用 DKIM、SPF 或任意其他 TXT 代替。当前验证是一次性状态提升：系统不会定期复核，也不会在 TXT 后续被删除时自动降回 pending，verified 域的 UI 也不提供再次检查；部署者必须外部监控记录持续存在。

## PTR / 正反向一致

PTR 由公网 IP 的所有者（通常是 VPS/云服务商）设置：

```text
203.0.113.10  -> PTR -> mail.example.com
mail.example.com -> A -> 203.0.113.10
```

服务器 SMTP banner 使用 `BRCLIO_HOSTNAME`，它应与 PTR 和证书主机名一致。多个邮件域可以共用同一个服务器主机名，但一个 IP 通常只应选一个稳定的 PTR。反向 DNS 的操作建议可参考 [RFC 1912](https://www.rfc-editor.org/rfc/rfc1912.html)。

## SPF 必须匹配实际出口

一个域只能发布一条 `v=spf1` TXT 记录。不要同时创建两条 SPF，也不要把邮件管理台的通用提示当成 smarthost 的最终配置。

若这台 Brclio Mail 主机直接投递（当前不推荐）：

```text
example.com. TXT "v=spf1 mx -all"
```

若所有外发经过 smarthost，应使用提供商官方给出的 `include` 或 IP 机制。例如结构可能是：

```text
example.com. TXT "v=spf1 include:_spf.provider.example -all"
```

`_spf.provider.example` 只是占位符，不能直接发布。混合出口时，合并到同一条记录，例如 `v=spf1 mx include:<provider-record> -all`。SPF DNS 查询机制有上限，详见 [RFC 7208](https://www.rfc-editor.org/rfc/rfc7208.html)。

## DKIM

每个域创建时生成 2048 位 RSA DKIM 密钥，默认 selector 是 `brclio`。从管理员控制台复制完整公钥记录；DNS 控制台可能把长 TXT 拆成多个引号片段，这是合法的，但查询结果拼接后必须保持内容不变。

```text
brclio._domainkey.example.com. TXT "v=DKIM1; k=rsa; p=<管理台公钥>"
```

私钥保存在 SQLite 中，备份等同于持有发信身份。当前版本尚无自动 selector 轮换流程；轮换是生产路线图项目。规范见 [RFC 6376](https://www.rfc-editor.org/rfc/rfc6376.html) 与密钥建议 [RFC 8301](https://www.rfc-editor.org/rfc/rfc8301.html)。

## DMARC：先监控

首发策略保持 `p=none`：

```text
_dmarc.example.com. TXT "v=DMARC1; p=none; rua=mailto:dmarc@example.com; adkim=r; aspf=r"
```

先收集并分析足够的聚合报告，确认合法来源都通过 SPF 或 DKIM 对齐，再考虑 `quarantine` 或 `reject`。Brclio Mail 目前只把报告作为普通邮件接收，不解析报表，也不会在入站阶段执行 DMARC；当前 DMARC 规范见 [RFC 9989](https://www.rfc-editor.org/rfc/rfc9989.html)，聚合报告格式见 [RFC 9990](https://www.rfc-editor.org/rfc/rfc9990.html)。

## TLS-RPT 与 MTA-STS 的区别

可以发布 TLS-RPT：

```text
_smtp._tls.example.com. TXT "v=TLSRPTv1; rua=mailto:tlsrpt@example.com"
```

它请求远端发送 TLS 汇总报告；Brclio Mail 当前只接收这些邮件，不自动分析，规范见 [RFC 8460](https://www.rfc-editor.org/rfc/rfc8460.html)。

**当前版本没有实现 MTA-STS。** 它既不托管 `https://mta-sts.example.com/.well-known/mta-sts.txt`，也不在直接投递时下载/执行远端策略。不要在没有独立、正确的策略托管和运维能力时发布 `_mta-sts` 记录；错误策略可能造成收信故障。MTA-STS 规范见 [RFC 8461](https://www.rfc-editor.org/rfc/rfc8461.html)。

TLS-RPT/MTA-STS 不能替代反垃圾。当前入站没有 SPF、DKIM、DMARC 验证、内容垃圾评分或恶意软件扫描。

## SRV 与角色地址

客户端发现记录基于 [RFC 6186](https://www.rfc-editor.org/rfc/rfc6186.html)：

```text
_submission._tcp.example.com. SRV 0 1 587 mail.example.com.
_imaps._tcp.example.com.      SRV 0 1 993 mail.example.com.
```

SRV 只是发现提示，并非所有客户端都采用。仍应发布人工配置文档。建议把以下地址做成受监控的用户或别名：`postmaster@`、`abuse@`、`security@`、`dmarc@`、`tlsrpt@`；常见角色地址见 [RFC 2142](https://www.rfc-editor.org/rfc/rfc2142.html)。

## DNS 验收命令

等待权威 DNS 生效后执行：

```bash
dig +short A mail.example.com
dig +short AAAA mail.example.com
dig +short MX example.com
dig +short -x 203.0.113.10
dig +short TXT example.com
dig +short TXT _brclio-mail.example.com
dig +short TXT brclio._domainkey.example.com
dig +short TXT _dmarc.example.com
dig +short TXT _smtp._tls.example.com
dig +short SRV _submission._tcp.example.com
dig +short SRV _imaps._tcp.example.com
```

再从不同网络检查端口和 TLS：

```bash
nc -vz mail.example.com 25
openssl s_client -starttls smtp -connect mail.example.com:25 -servername mail.example.com </dev/null
openssl s_client -connect mail.example.com:465 -servername mail.example.com </dev/null
openssl s_client -starttls smtp -connect mail.example.com:587 -servername mail.example.com </dev/null
openssl s_client -connect mail.example.com:993 -servername mail.example.com </dev/null
```

最后必须做真实外部收发测试，并在收件方查看 Authentication-Results。预期：DKIM `pass`；SPF 是否 `pass` 取决于实际出口配置；DMARC 在对齐后 `pass`。DNS 正确并不保证信誉或投递率。
