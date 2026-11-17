# ec2pool 部署记录

ec2pool（AWS us-west-2，x86_64）上的 sup2api 是一个独立集群，和 ovh/clicodeplus 集群不共用数据库。同一台机器上还有 sakura-2api、cli-proxy-api、antigravity 和 kin 容器，与 sup2api 无关，操作时不要碰。

## 现状（2026-11-17 起）：网关托管，单节点，网关 0.1.138、核心 0.1.303

- 目录 `/opt/sup2api-gw`，compose 项目 `sup2api-gw`，接入外部网络 `sup2api_default`。
  - `node-a`（容器 `sup2api-gw-a`，主节点，`127.0.0.1:18080`），镜像 `sup2api-gateway:0.1.138`，状态卷 `sup2api-gw_node-a`（`/var/lib/sub2api`，持久）。只有一个节点（用户 11-17 决定）：核心更新没有别的节点承接流量，走主节点优先，会短暂停服；网关一键更新单节点中断约 10 秒，控制台要求确认。
  - `releases`（`sup2api-gw-releases`，caddy，`https://sup2api-gw-releases/sup2api` 提供 `publish/sup2api/`）：节点配置的 `release_origin`，离线导入核心清单用；平时更新走控制台的更新源（默认 GitHub `Sub2API-Devs/sup2api`），这里的超管能用密码完成二次验证。
  - 证书是本机自签的 Ed25519 CA（`certs/`，CA 私钥在 `certs/ca/`，0700），SAN 为容器名；`config/node-a.json`：`cluster_id` = `sup2api-ec2pool`，`trusted_proxies` = docker 网关 `10.200.70.1/32`。
  - `.env`（0600）：与旧部署同一个 PostgreSQL、Valkey、`SUB2API_MASTER_KEY` 与引导管理员。
- PostgreSQL 16（`sup2api-pg-1`，库 `sup2api`）、Valkey 9（`sup2api-redis-1`，AOF）与开发插件市场（`sup2api-market-1`）仍属于旧项目 `sup2api`（`/opt/sup2api`，`./compose.sh`），继续运行。
- 入口：宿主机 systemd Caddy 的 `/etc/caddy/sup2api-customer.caddy`（`sub2api-customer.sakura-api.com`、`sup2api.32.191.20.99.nip.io`、`https://sup2api.32.191.20.99.sslip.io`）反代到 `127.0.0.1:18080`。主 Caddyfile 是 `admin off`，`systemctl reload caddy` 会失败，改配置要 `restart`（会断开同一个 Caddy 上 antigravity、cliproxy 站点的连接）。
- 内置插件签名密钥：核心 0.1.298 起在没设 `SUB2API_BUILTIN_TRUST_KEY` 时自己读 `builtin/trust.pub`，compose 里不再设这个变量（0.1.297 期间手动设过，备份 `compose.yml.bak-with-trustkey`；回到 0.1.297 要加回去）。
- 插件：`SUB2API_PLUGIN_VERIFY_SIGNATURES=false`（沿用旧部署）；已装插件包原来由旧核心用开发密钥 `sub2api-dev` 签名，Release 内置包用 `sup2api-plugins-2026` 签名，同版本的包保留原样。

## 2026-11-18 核心 0.1.303（codex 0.1.11 等，迁移 0105）

- Release `core-v0.1.303`（`4414334c8`，清单 `d97809231015c5269addc248f406e8db3851bba4f2f222c4c8c97564a92100a8`，amd64 bundle `cf96d8c6…44c0`）由 `fetch303.sh` 下载到 `stage303/`，核对摘要后放进 `publish/sup2api/`，`docker compose exec -T node-a sub2api-shell import …`；预检只有 node-a、primary-first-v1、无阻断（expected_revision 9）；计划 `4e649809f536490fe8b9dc0f9da79522` 05:15:28→05:15:34，`/readyz` 503 05:15:31.3→05:15:35.3 约 4 秒（`observe303.py`，输出 `plan303.out`）；迁移 0105 于 05:15:33 应用。
- 核验（`verify303.py`、`db303.sh`）：核心 0.1.303；anthropic 0.3.4、codex 0.1.11、gemini 0.3.2、moderation 0.12.1、openai 0.4.3 active、ready、restarts 0（05:15:37 内置同步），dynweight、mailer、volcengine 仍只安装未启用；两个 codex 账号（id 2、3）active、可调度；强制检测无更新；日志无 ERROR。
- 回滚：迁移 0105 改了 schema，不回退核心，出问题修复前进。

## 2026-11-18 核心 0.1.302（mailer 0.1.4，无迁移）

- Release `core-v0.1.302`（`92ca8c469`，清单 `188dc02f01e331299f100dcccc4226a08c32e784415c515637e5f69d88c8ec78`，amd64 bundle `ba765c82…d5d9`）下载到 `stage302/`，核对摘要后放进 `publish/sup2api/`，`docker compose exec -T node-a sub2api-shell import …`；预检只有 node-a、primary-first-v1、无阻断（expected_revision 7）；计划 `ee9a061b7c1c6546f64cef3aba926e19` 03:44:02→03:44:09，`/readyz` 503 03:44:05.8→03:44:09.8 约 4 秒（`observe302.py`，输出 `plan302.out`）。
- 核验（`verify302.py`，读 `/opt/sup2api/ADMIN_CREDENTIALS`）：核心 0.1.302；anthropic、codex 0.1.10、gemini、moderation、openai active、ready、restarts 0，dynweight、mailer、volcengine 仍只安装未启用（mailer 0.1.4 包已存入）；两个 codex 账号 active、可调度；强制检测无更新；日志无 ERROR。
- 回滚：核心回 0.1.301（清单 `8fb8ddbd…b0b6`）建主节点优先计划。

## 2026-11-18 核心 0.1.301（codex 0.1.10，删除 custom）

- 发布前（只读）：custom 账号 0；账号只有两个 `codex_device`（id 2、3）。不备份数据库。
- Release `core-v0.1.301`（`e0a985c81`，清单 `8fb8ddbd75afb13eceaa6041f10bfc145249ed2aa7021df4a9add3369611b0b6`，amd64 bundle `3e632f88…8684` 核对摘要）下载到 `publish/sup2api/`，`docker compose exec -T node-a sub2api-shell import -config /etc/sub2api/shell.json -manifest https://sup2api-gw-releases/sup2api/<清单>.json`；预检只有 node-a、primary-first-v1、无阻断（expected_revision 5）；计划 `bf6597af4f50491e9567509e33d397d8` 01:20:07→01:20:13，`/readyz` 503 01:20:09.7→01:20:13.7 约 4 秒（`observe301.py`，输出 `plan301.out`）；迁移 0104 于 01:20:12 应用（`plugin_key_retirements` 记入 custom）。
- 核验（`verify301.py`）：核心 0.1.301；anthropic 0.3.3、codex 0.1.10、gemini 0.3.1、moderation 0.12.1、openai 0.4.2 active、ready、restarts 0，dynweight、mailer、volcengine 仍只安装未启用；无 custom 类型；两个 codex 账号 active、可调度；强制检测无更新、无错误；日志无 ERROR。
- codex 会话缓存（codex 0.1.10 的修复）：上线前 11-17 21:56–22:13 codex-test 22 次，cache_read/input 26.6%（cache_read 945280 / input 3558073）；上线后到记录时还没有新请求，复查用 `usage_logs` 按 `api_key_id`（codex-test）与 `created_at >= '2026-11-17 17:20:13+00'` 汇总 `cache_read_tokens` / `input_tokens`。
- 回滚：迁移 0104 单向，不回退核心，出问题修复前进。

## 2026-11-17 网关 0.1.138 + 核心 0.1.298（codex 0.1.8）

- 网关：镜像 `sup2api-gateway:0.1.138` 从 ovh 复制（二进制 sha256 `82baa5c2…51f4`，与 Release `gateway-v0.1.138` 一致）；`.env` 备份 `.env.bak-gw137` 后改 `GATEWAY_IMAGE`，22:37:44 重建 node-a（在途 0），22:37:49 `/readyz` 200。
- 核心：Release `core-v0.1.298`（`0bbda1192`，清单 `108d2c7d0115cccdb9f83839be85045ea8ca26fedf6a4fbd9e415e8cff2ff9d7`，amd64 bundle `698a2d21…07a4` 核对摘要）放进 `publish/sup2api/` 导入；预检只有 node-a、primary-first-v1、无阻断；计划 `5598fc2899a37cbd5f7212e118ad618d` 22:40:05→22:40:11，`/readyz` 503 22:40:07.8→22:40:11.8 约 4 秒（`plan298.out`）；codex rollout 到 0.1.8。
- 去掉 `SUB2API_BUILTIN_TRUST_KEY`（备份 `compose.yml.bak-with-trustkey`），22:40:36 重建 node-a，22:40:42 就绪；日志没有 `unknown signing key`，内置插件“run the versions of this release”。这次各内置插件版本都已存在，没有触发新的上传验签；下一个带新内置插件版本的核心会真正走到它。
- 核验：核心 0.1.298、网关 0.1.138；6 个启用插件 active、ready（codex 0.1.8）；codex 认证方式“设备码”“Codex”；三个入口域名 `/readyz` 200；检测无更新、无错误；日志无 ERROR。
- 回滚：核心用 0.1.297 清单 `64bc544f…c431` 建主节点优先计划，并把 `SUB2API_BUILTIN_TRUST_KEY` 加回 compose；网关 `.env` 换回 `.env.bak-gw137` 后重建。

## 2026-11-17 核心 0.1.297（codex 0.1.7），删除 ChatGPT 账号

- 20:40:17（北京时间）经控制台接口软删除 `chatgpt_siwc` 账号 id 1 `gpt-plus-1117-01`（用户要求，codex 0.1.7 删除了这个类型）；之后没有 `chatgpt_siwc` 账号。
- Release `core-v0.1.297`（`f40c7831d`，清单 `64bc544f5b13d683d4565cb4fb5a0fefc60369ed6f0e434457285e857710c431`，amd64 bundle `7222fad2…bfa5` 核对摘要）放进 `publish/sup2api/`，`import -manifest https://sup2api-gw-releases/sup2api/<清单>.json`。
- 预检的计划节点仍是 `["node-a","node-b"]`：网关 0.1.137 会把停用节点放进计划，计划走到它会暂停（停止步骤还要等它的停止确认），而 node-b 已不存在。经用户同意（“继续做”）只改一行 `UPDATE updater.nodes SET stopped = true WHERE cluster_id = 'sup2api-ec2pool' AND node_id = 'node-b' AND NOT enabled`（1 行），再 `remove-node -node node-b`（登记与准入记录删除）；重新预检只剩 node-a。网关 0.1.138（`09ee340f6`）起不再需要这样做。
- 计划 `64d9594ce44007181b36ac7dafa1749c` primary-first-v1 completed，21:16:24→21:16:30；入口 `/readyz` 503 21:16:26.9→21:16:31.0 约 4 秒（`observe.py`，输出 `plan297.out`）。
- 内置插件没有升级：`builtin plugin … upload …: plugin signature: unknown signing key "sup2api-plugins-2026"`。在 `compose.yml` 补 `SUB2API_BUILTIN_TRUST_KEY`（备份 `compose.yml.bak-pre-trustkey`）后 21:17:45 重建 node-a（`/readyz` 约 5 秒后 200），codex rollout 升到 0.1.7、旧包删除；其余内置插件同版本已存在，保留原包（WARN `version already stored with other content`）。
- 核验：核心 0.1.297；anthropic 0.3.3、codex 0.1.7、custom 0.1.1、gemini 0.3.1、moderation 0.12.1、openai 0.4.2 active、ready、restarts 0；codex 账号类型只剩 `codex_device`、`codex_oauth`；三个入口域名 `/readyz` 200；30 分钟内没有请求，5xx 0；强制检测核心、网关无更新、无错误。
- 回滚：核心用 0.1.296 清单 `9fbbdae1f86286fa0b72498f74fb61352403805bc9d7ce4aa46d2f13bc7bcf90` 建主节点优先计划（插件只升不降，codex 留在 0.1.7）。

## 2026-11-17 改为单节点

切换后先按两节点（node-b 在 `127.0.0.1:18082`）运行；用户随后说“ec2pool 单节点就行了”。node-b 先 `yield`（在途 0），`disable-node -id node-b` 停用，停容器，从 `compose.yml` 删掉 node-b 服务与卷（两节点版本留在 `compose.yml.bak-two-nodes`），删除容器、卷 `sup2api-gw_node-b`、`config/node-b.json` 与证书；`switch.sh`、`rollback.sh` 改成单节点。node-a 全程没有中断。
- （网关 0.1.138 起已修：停用节点不进计划，`remove-node` 不再要求 `stopped`。）`remove-node` 删不掉 node-b 的登记：它要求 `updater.nodes.stopped` 为 true 且 5 分钟内没有心跳，而 `stopped` 只由升级计划的停止步骤写入，手动停容器或停用都不写。所以 `updater.nodes` 里留一行 `enabled=false` 的 node-b：预检、网关自更新与更新检测只看已启用的节点，不受影响；控制台节点页（核心心跳）只剩 node-a，升级页的节点列表里会显示停用的 node-b。
- Caddy 曾为 node-b 加过备用上游（`lb_policy first` + `/readyz`），因为 `admin off` 没能 reload，随即用备份 `sup2api-customer.caddy.bak-20261117-pre-node-b` 还原，运行中的配置始终未变。
- 核验：`/nodes` 只有 node-a（核心 0.1.296）；强制检测核心、网关都没有更新、无错误；三个入口域名 `/readyz` 200。

## 2026-11-17 由 deploy/single 切到网关托管，核心 0.1.296 + 网关 0.1.137

- 用户要求“ec2pool 也改成 docker compose 的形式更新上去”，在卡片上选了“网关托管，先发 GitHub 0.1.296”。旧部署：`/opt/sup2api`（`next/deploy/single`，非托管，没有网关），app、app-2 两个核心跑 `sup2api:179142ae6`（自报 0.1.295），插件包存在旧核心的本机目录 `plugins/blobs`。
- 核心基线：GitHub Release `core-v0.1.296`（`cf4718dcb`，含 `179142ae6`），清单 sha256 `9fbbdae1f86286fa0b72498f74fb61352403805bc9d7ce4aa46d2f13bc7bcf90`，amd64 bundle `5a59c505…2777` 下载后核对摘要放进 `publish/sup2api/`。网关镜像 `sup2api-gateway:0.1.137` 从 ovh 复制（二进制 sha256 `fc74f2f2…2618`，与 Release `gateway-v0.1.137` 一致）。
- `switch.sh`（北京时间）：19:25:47 起 releases、停旧 app 与 app-2（pg、redis、market 不停）；19:25:59 把旧 `sup2api_appdata` 卷的 `plugins/blobs/`（11 个包）原样复制到 node-a 卷的 `plugin-blobs/`（托管后核心改从网关取包，不复制则已装插件起不来）；19:26:00 `init -manifest https://sup2api-gw-releases/sup2api/<清单>.json` 建 `updater` schema 并导入基线；19:26:05 node-a `/readyz` 200，19:26:09 node-b 200。入口约 6 秒不可用（流量几乎为 0）。
- 核验：两节点网关 0.1.137、核心 0.1.296（`updater.nodes` 两行 `self_update` true）；anthropic 0.3.3、codex 0.1.6、custom 0.1.1、gemini 0.3.1、moderation 0.12.1、openai 0.4.2 两节点 active、ready、restarts 0（dynweight、mailer、volcengine 原本只安装未启用）；三个入口域名 `/readyz` 200，超管经 https 域名登录、`/plugins`、`/accounts`、`/usage` 200；更新源为默认 GitHub，强制检测核心、网关都没有更新、无错误；日志只有验签关闭与 moderation 钩子匹配未安装平台的 WARN。
- 数据：同一个库、同一个 schema（`ab35f2f6…187d`），没有迁移；新增 `updater` schema，旧核心不读它。
- 回滚：`sh /opt/sup2api-gw/rollback.sh`（停 node-a，卷保留；`/opt/sup2api/compose.sh up -d app` 起旧核心，入口端口相同，Caddy 不用改）。旧 compose、镜像 `sup2api:179142ae6` 与 `sup2api_appdata`/`appdata2` 卷都保留。
