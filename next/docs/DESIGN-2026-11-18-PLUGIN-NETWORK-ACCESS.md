# 插件联网：管理员可以指定插件能访问、不能访问的域名

2026-11-18，所有者决定：“默认放行，可以指定域名就行了”，交互照浏览器“网站设置”的截图（默认设置下拉框 + 一张随模式变化的域名列表 + 添加对话框）。契约写进 CONTRACTS §113。

## 1. 现状（改之前）

- 插件的对外连接只能经核心的出口隧道（`server/internal/plugin/egress`，`EgressService.Dial`；SDK 一侧 `sdk/pluginsdk/egress`）。Linux 上 seccomp 不让插件自己开套接字。
- 每次新连接调用 `Provider.check`，顺序是：
  1. 数据库例外：插件有 `db.schema` 且声明了数据库时，`GetDSN` 交出去的 PostgreSQL 地址按原名放行；
  2. 没有 `net` 授权的插件什么都连不了（DNS 也不行）；
  3. **插件自己的出口策略**（`AllowedHost`）：`plugins.egress_policy` 列，取值 `allow_all`（默认）或 `allowlist`，按插件存，`PUT /plugins/:key/egress-policy`（`plugin:manage`，审计 `plugin.egress_policy.update`）修改，插件详情的“外部访问”页里切换。`allowlist` 时目标必须是 `net` 授权范围 `domains` 里的一项（完全相同，或 `*.example.com` 匹配任意层子域名、不含本身，`*` 匹配全部）；`allow_all` 时不看授权范围；
  4. 核心自己的服务地址（Redis、其他名字下的 PostgreSQL）一律拒绝；
  5. 解析出的每个地址都必须是公网地址（`SUB2API_PLUGIN_EGRESS_ALLOW_PRIVATE=true` 只用于测试和开发）。
- 插件的 DNS 查询（`dns.sub2api:53`）同样先过第 2、3 步。
- 每个连接写一行 `plugin_egress_logs`（`result` 为 `open`/`ok`/`denied`/`dial_error`/`reset`，拒绝原因在 `error`），每个主机累计到 `plugin_egress_domains`；控制台插件详情的“外部访问”页按目标汇总、列出访问过的域名和连接明细，结果徽标的提示文字是 `error`。
- 没有全局的策略、环境变量或配置项；也没有管理员自己维护的域名列表。

## 2. 新模型

一项全局的系统设置“插件联网”（settings 表的 `plugin_egress` 行），对所有插件生效：

| 模式 `mode` | 含义 | 生效的列表 |
|---|---|---|
| `allow_all`（默认） | 允许所有域名，屏蔽列表里的除外 | `blocked_domains` |
| `block_all` | 屏蔽所有域名，只允许允许列表里的 | `allowed_domains` |

- 两个列表分开保存，切换模式不清空另一个列表，切回来还在。
- 没有这一行时就是默认值：`allow_all`、两个列表为空，与改之前完全相同。所以**不需要迁移**。

### 2.1 与插件自己的授权怎么组合

管理员的设置是在第 3 步之后**再加的一道检查**，不替换任何已有的检查：

1. 数据库例外不受影响（那是核心交给插件的地址，不是“联网”）；
2. 没有 `net` 授权仍然什么都连不了；
3. 插件自己的出口策略照旧：设为 `allowlist` 的插件仍只能连 `net` 授权里的域名；
4. **新增：** 目标还必须通过管理员的设置：`allow_all` 时不在屏蔽列表里，`block_all` 时在允许列表里；
5. 核心服务地址、非公网地址的检查照旧。

也就是说，一个主机要同时通过插件的授权和管理员的设置。管理员的允许列表不会放宽插件的授权：授权范围只有 `smtp.example.com` 的 `allowlist` 插件，在允许列表里加上 `other.com` 也连不上 `other.com`。任何一个插件能连的主机都不会比改之前多。DNS 查询同样走第 4 步，被拒的查询回 REFUSED。

### 2.2 全局还是按插件

截图里是一项全局设置，代码里也只有“按插件的模式开关”而没有按插件的列表（插件的域名来自它的授权）。所以这次只做全局的一项设置，按插件的 `egress_policy` 保持原样，作为上面的第 3 步。两者回答的是不同的问题：按插件的开关决定“这个插件是否只能去它申请过的域名”，全局设置决定“管理员允许插件去哪些域名”，不是同一件事的两层配置，不合并。插件详情的“外部访问”页加一行链接到全局设置。

### 2.3 域名匹配与输入

- 匹配复用 `manifest.ExternalServiceHost`（清单 `externalServices` 用的那一个）：列出的域名匹配它自己和它的所有子域名，不区分大小写。`example.com` 匹配 `example.com`、`api.example.com`，不匹配 `badexample.com`。
- 比较的是插件拨号时用的名字（解析之前）。插件直接拨 IP 字面量时按这个 IP 比较：列表里写 IP（IPv4 原样，IPv6 存成 `[2001:db8::1]`）只匹配这个 IP 本身，没有“子域名”。要彻底管住插件能去哪里，用 `block_all`：屏蔽某个域名挡不住插件直接拨它解析出的 IP。
- 端口不参与匹配：一个域名在所有端口上同样允许或屏蔽。输入里带端口的会被拒绝，以免让人以为能按端口区分。
- 输入校验（服务端为准，控制台做同样的检查以便逐行提示）：去掉首尾空白和末尾的点、转小写；国际化域名转成 punycode；必须是合法的主机名（每段 1–63 个字母、数字或连字符，不以连字符开头或结尾，总长不超过 253）或 IP 字面量；带 `://`、路径、端口、通配符 `*`、空白的都拒绝。重复项去掉。不限制条数（本分支所有者规则：不加大小或次数上限）。

### 2.4 存储、生效、权限

- 存在 `settings` 表 `plugin_egress` 行（JSON `{mode, blocked_domains, allowed_domains}`），写法同其他系统设置（`store.UpdateSettingJSONTx` 在事务里锁行改写）。
- 接口 `GET /settings/plugin-egress`（`settings:read`）、`PUT /settings/plugin-egress`（`settings:manage`），与网关设置、环境变量等系统设置同一组权限；`PUT` 的字段都可省略，省略的保持原值，列表整体替换。写入在同一事务里记审计 `settings.plugin_egress.update`（`detail` 是保存后的整份设置），因为按插件的出口策略也记审计。
- 每个节点的出口隧道缓存这份设置 10 秒（与网关设置、环境变量的刷新间隔相同）；保存后本节点立即失效，并广播 `config:changed {"key":"settings.plugin_egress"}`，其他节点收到即失效，漏收时最多 10 秒后读到新值。只影响之后的新连接和 DNS 查询，已经建立的连接不断开。
- 读失败时沿用上次读到的设置；从来没有读成功过（节点刚启动时数据库不可用）时拒绝连接，原因 `plugin network access settings could not be read`，不按默认放行，以免 `block_all` 在故障时失效。

### 2.5 日志

被管理员设置拒绝的连接照常写一行 `plugin_egress_logs`，`result = denied`，`error` 写明原因：

- `allow_all`：`<host> is on the blocked list of plugin network access`
- `block_all`：`<host> is not on the allowed list of plugin network access`

在插件详情“外部访问”页的连接明细里显示（结果“已拒绝”，鼠标悬停看原因），也计入按目标汇总的“已拒绝”和访问过的域名。

### 2.6 范围

只管出口隧道（插件自己开的连接和 DNS）。核心替插件发出的上游请求（`RelayUpstream`、`SendAccountRequest`）和取回（Fetch）有自己的规则（账号地址、清单 `externalServices`、SSRF、代理），不受这项设置影响。

## 3. 控制台

“系统设置 → 运行环境 → 插件联网”（`/settings?tab=plugin-egress`，`settings:read` 可看、`settings:manage` 可改）。放在系统设置里，因为它是一项全局的系统设置、权限与同组设置一致；插件详情“外部访问”页（按插件的出口策略和日志所在处）加一行说明和链接，运维从任一处都能找到。

- 标题与一行说明，下面一行“所有域名的默认设置”，右侧下拉框只有“允许所有域名”（默认）和“屏蔽所有域名”。切换立即保存。
- 下面一行提示随模式变化：“插件可以访问所有域名，但您在下方屏蔽的域名除外” / “插件只能访问您在下方允许的域名”。
- 一张列表，标题随模式变化：“已屏蔽的域名”（插件无法访问这些域名）/“已允许的域名”（插件可以访问这些域名）；只有“域名”一列，空时“尚未添加任何域名。”；右上角“+ 添加域名”；每行一个“移除”。
- 添加对话框：标题“添加要屏蔽的域名”/“添加要允许的域名”，一个输入框（占位 `website.com`），“+ 添加其他域名”再加一行，右侧“批量添加”切换为多行文本框（每行一个），按钮“取消 / 继续”。逐行校验，错误显示在对应行下；去掉空行、重复项和列表里已有的。
- 开发模拟数据（`web/mock`）提供这两个接口。

## 4. 与旧设置的关系、升级

没有要替换的旧配置项：以前没有全局策略。按插件的 `egress_policy` 不变，设成 `allowlist` 的插件升级后行为不变。新设置默认 `allow_all`、列表为空，升级后所有插件能连的主机与升级前相同。CHANGELOG 写明这一点。

## 5. 测试

- 出口隧道：默认放行；`allow_all` 屏蔽的域名和它的子域名被拒，其他放行；`block_all` 只放行允许的域名和子域名；切换模式两个列表都保留；没有 `net` 授权仍被拒；插件 `allowlist` 的限制不被允许列表放宽；保存后不重启即生效（失效后下一次连接读到新值）；拒绝写进日志、原因正确；DNS 查询同样被拒。
- 接口：校验（模式、非法域名逐项报错、端口、通配符、IDN、IPv6）、去重、省略字段保持原值、审计、广播。
- 控制台：模式切换显示对应的列表和文字；单个、多行、批量添加；非法输入逐行提示；移除。
- e2e：`TestAC13_EgressLogAndStrictNetwork` 用默认设置，期望不变；需要在集成后的栈上确认一次。
