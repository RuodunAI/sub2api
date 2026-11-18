# 插件数据库只用核心的数据库用户

2026-11-18，所有者决定："它访问数据库都是通过核心的数据库配置来访问，而不需要任何新数据库用户"。契约写进 CONTRACTS §113。

## 1. 为什么改

改前，核心给每个持有 `db.schema` 的插件建一个登录角色 `plg_<key>`（`CREATE ROLE … LOGIN CONNECTION LIMIT 20`，密码由主密钥 HMAC 派生，角色注释作归属标记，`GRANT plg_<key> TO CURRENT_USER`，schema 交给该角色，角色的 `search_path` 固定为自己的 schema），插件迁移以该角色登录执行，迁移记录经两个 SECURITY DEFINER 函数（迁移 0003、0023）写入，卸载清除数据时 `DROP OWNED BY` 再 `DROP ROLE`。核心的数据库用户因此要有 `CREATEROLE`，没有时授予 `db.schema` 失败（`plugin_db_isolation_unavailable`），运维可用 `SUB2API_PLUGIN_DB_ROLE_ISOLATION=false` 改为共用核心的用户。

用生产库副本演练恢复时，不带属主（`--no-owner`）恢复后插件读不了自己的表（permission denied），要手工把 26 个对象改回属主。每个插件一个用户让备份恢复依赖属主和集群级的角色，而角色不在单库的备份里。

## 2. 改后的模型

- 只有一个数据库用户：核心的（`DATABASE_URL` 里的）。核心不再建、改任何角色，也不再需要 `CREATEROLE`。
- 持有 `db.schema` 的插件仍有自己的 schema `plg_<key>`，属主是核心的用户。核心在插件第一次需要时（迁移、`GetDSN`）`CREATE SCHEMA IF NOT EXISTS`。
- 插件迁移由核心以自己的用户执行，每个文件一个事务，执行前 `SET LOCAL search_path TO plg_<key>`；迁移记录由核心直接写 `public.plugin_migrations`。迁移 0111 删除两个 SECURITY DEFINER 函数。
- `HostService.GetDSN` 返回核心的 DSN，凭据、主机、库名、TLS 参数原样，另加 `options=-c search_path=plg_<key>`；`role_isolated` 不再填写（恒为 false，字段待 proto 的所有者删除）。连接仍经出口隧道，隧道对数据库地址的例外不变。
- 卸载并清除数据：`DROP SCHEMA plg_<key> CASCADE`，删除迁移记录。
- 删除的东西：角色的创建与修改、密码派生、归属标记的写入、成员授予、schema 移交、角色的 `search_path`、`rewriteDSN` 换凭据的分支、`Tracker.AsPluginRole`、`Status.Role`/`RoleIsolated`、错误 `plugin_db_isolation_unavailable` 与 `plugin_db_role_conflict`、配置 `SUB2API_PLUGIN_DB_ROLE_ISOLATION`，以及它们的测试。

## 3. 隔离的含义（直说）

持有 `db.schema` 的插件现在能访问核心的用户能访问的一切：核心自己的表（`public`）、升级计划（`updater`）、其他插件的 schema，能读也能写。插件之间、插件与核心之间的分隔只靠约定（`search_path` 只是默认位置，不是边界）和安装时对高风险权限 `db.schema` 的同意，PostgreSQL 不再强制。

实际能读到什么（已在代码里核对）：

- 读不到明文的：上游账号凭据（`accounts.credentials_enc`）、代理密码（`proxies.password_enc`）、API Key 的可还原副本（`api_keys.key_cipher`）、TOTP 密钥、插件的加密设置与 `secrets`、价格同步源的密钥，都用主密钥加密（`secret.Cipher`，AES-GCM），主密钥只在核心进程里（`SUB2API_MASTER_KEY`，读完即从环境删除），不在数据库里。API Key 与访问密钥按 SHA-256 存（`key_hash`、`token_hash`），用户密码存哈希。会话令牌在 Valkey，不在 PostgreSQL。
- 读得到的：用户邮箱和资料、余额与账本、用量日志、分组与价格、角色与授权（`casbin_rule`）、审计日志、其他插件表里的数据。
- 写得了的：上面所有表。能改余额、给自己或任何用户授予角色、改账号配置、覆盖加密列（换成自己的值不需要主密钥就能让数据失效）。也就是说，持有 `db.schema` 的插件能拿到整个系统的管理权。

因此：

- 官方发布者的插件照常授予 `db.schema`。
- 非官方插件授予 `db.schema` 一律要求操作者持有超管专属的 `plugin:grant:db_schema_unconfined`（改前只在关闭角色隔离时要求）；评审接口的 `needs_db_schema_unconfined` 同步，权限的说明改为"插件使用核心的数据库用户，能访问核心能访问的所有表"。

## 4. 已有数据库的升级

生产库里有这些角色，插件的表、视图、序列、schema 都属于它们。

- **启动时一次**：核心列出名为 `plg_%` 的角色，逐个在插件自己的 schema 锁（`plugin-schema:<key>`，与 Ensure、Drop 同一把事务级 advisory 锁）里转换：确认角色是本库为该插件建的（注释等于 `sub2api plugin role; database=<本库>; plugin=<key>`，或角色拥有本库的 `plg_<key>` schema——标记出现之前建的角色），然后在一个事务里 `REASSIGN OWNED BY plg_<key> TO CURRENT_USER`、`DROP OWNED BY plg_<key>`（撤销授给它的权限）、`DROP ROLE plg_<key>`。已卸载插件留下的角色同样转换。不是本库的同名角色（另一部署、运维自建）不动。
- **Ensure 时**：每次确保某插件的 schema（迁移、`GetDSN`、清除数据）先做同样的转换。
- **权限**：`REASSIGN OWNED`、`DROP OWNED` 要求核心有该角色的权限，`DROP ROLE` 要求 `CREATEROLE`（PostgreSQL 16 起还要对该角色有 ADMIN）。这些都是旧代码建角色时就具备的：角色由核心的用户创建（16 起自动得到 ADMIN），并且旧代码执行过 `GRANT plg_<key> TO CURRENT_USER`（继承权限）。所以不需要超级用户；Supabase 的 `postgres` 用户就是这种情况。逐个对象 `ALTER … OWNER TO` 要求同样的权限，不是退路，所以不做。
- **还有会话的角色**：PostgreSQL 允许删除仍有会话的角色（已在 PostgreSQL 17 上验证），删掉后旧进程会失去自己表的权限。所以转换前查 `pg_stat_activity`，有该角色的会话就整个跳过、只记一次警告，下次 Ensure（以及下次启动）再试；此期间核心的用户经成员关系照样能读写这些表，新插件进程不受影响。
- **多节点同时启动**：同一插件的转换在 schema 锁里串行，后到的节点再查时角色已不在，什么也不做；不同插件的转换另取一把全局锁 `plugin-role-conversion`（总在插件锁之后取），避免并发改同一对象的 ACL 报 `tuple concurrently updated`。
- **滚动重启怎么收尾**：本版本有核心迁移（0111），按主节点优先升级：从节点先停，主节点停机、迁移、启动；旧核心停下时它的插件进程随之退出，会话结束，所以主节点第一次启动就能转换全部角色。若某个旧插件进程的连接还没断（例如节点没按计划停），该角色留到下次 Ensure：那个节点换成新核心、插件重启取 DSN 时，或任何节点下次启动时完成。
- **恢复的三种情况**：
  - 不带属主恢复（全部对象已属于核心的用户、没有角色）：什么也不做，正常启动。
  - 带属主恢复（角色存在、对象属于角色）：与生产相同，启动时转换。
  - 带属主恢复但目标集群没有这些角色：恢复本身会在 `ALTER … OWNER TO plg_<key>` 上报错，对象留给执行恢复的用户；用核心的用户恢复即可，之后同第一种。

## 5. 不能回退

迁移 0111 删除了旧核心给插件角色用的两个函数。回退到旧核心时，旧核心会重新建角色并在授予这两个函数时失败，持有 `db.schema` 的插件起不来。所以这是单向升级：回退只能连同数据库一起回到升级前的备份。
