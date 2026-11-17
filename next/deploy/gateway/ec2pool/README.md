# ec2pool 部署记录

ec2pool（AWS us-west-2，x86_64）上的 sup2api 是一个独立集群，和 ovh/clicodeplus 集群不共用数据库。同一台机器上还有 sakura-2api、cli-proxy-api、antigravity 和 kin 容器，与 sup2api 无关，操作时不要碰。

## 现状（2026-11-17 起）：网关托管，两节点同机

- 目录 `/opt/sup2api-gw`，compose 项目 `sup2api-gw`，接入外部网络 `sup2api_default`。
  - `node-a`（容器 `sup2api-gw-a`，主节点，`127.0.0.1:18080`）与 `node-b`（`sup2api-gw-b`，`127.0.0.1:18082`），镜像 `sup2api-gateway:0.1.137`，状态卷 `sup2api-gw_node-a`、`sup2api-gw_node-b`（`/var/lib/sub2api`，持久）。
  - `releases`（`sup2api-gw-releases`，caddy，`https://sup2api-gw-releases/sup2api` 提供 `publish/sup2api/`）：节点配置的 `release_origin`，离线导入核心清单用；平时更新走控制台的更新源（默认 GitHub `Sub2API-Devs/sup2api`）。
  - 节点间 7443 只在 docker 网络内；证书是本机自签的 Ed25519 CA（`certs/`，CA 私钥在 `certs/ca/`，0700），SAN 为容器名。
  - `config/node-a.json`、`node-b.json`：`cluster_id` = `sup2api-ec2pool`，`trusted_proxies` = docker 网关 `10.200.70.1/32`。
  - `.env`（0600）：与旧部署同一个 PostgreSQL、Valkey、`SUB2API_MASTER_KEY` 与引导管理员。
- PostgreSQL 16（`sup2api-pg-1`，库 `sup2api`）、Valkey 9（`sup2api-redis-1`，AOF）与开发插件市场（`sup2api-market-1`）仍属于旧项目 `sup2api`（`/opt/sup2api`，`./compose.sh`），继续运行。
- 入口：宿主机 systemd Caddy 的 `/etc/caddy/sup2api-customer.caddy`（`sub2api-customer.sakura-api.com`、`sup2api.32.191.20.99.nip.io`、`https://sup2api.32.191.20.99.sslip.io`）反代到 `127.0.0.1:18080`，只到 node-a。node-a 换网关或重启的几秒里入口不可用；要让 node-b 接住，可以像 clicodeplus 一样在 Caddy 加 `127.0.0.1:18082` 作为 `lb_policy first` 的备用上游并检查 `/readyz`（尚未做）。
- 插件：`SUB2API_PLUGIN_VERIFY_SIGNATURES=false`（沿用旧部署）；已装插件包原来由旧核心用开发密钥 `sub2api-dev` 签名，Release 内置包用 `sup2api-plugins-2026` 签名，同版本的包保留原样。

## 2026-11-17 由 deploy/single 切到网关托管，核心 0.1.296 + 网关 0.1.137

- 用户要求“ec2pool 也改成 docker compose 的形式更新上去”，在卡片上选了“网关托管，先发 GitHub 0.1.296”。旧部署：`/opt/sup2api`（`next/deploy/single`，非托管，没有网关），app、app-2 两个核心跑 `sup2api:179142ae6`（自报 0.1.295），插件包存在旧核心的本机目录 `plugins/blobs`。
- 核心基线：GitHub Release `core-v0.1.296`（`cf4718dcb`，含 `179142ae6`），清单 sha256 `9fbbdae1f86286fa0b72498f74fb61352403805bc9d7ce4aa46d2f13bc7bcf90`，amd64 bundle `5a59c505…2777` 下载后核对摘要放进 `publish/sup2api/`。网关镜像 `sup2api-gateway:0.1.137` 从 ovh 复制（二进制 sha256 `fc74f2f2…2618`，与 Release `gateway-v0.1.137` 一致）。
- `switch.sh`（北京时间）：19:25:47 起 releases、停旧 app 与 app-2（pg、redis、market 不停）；19:25:59 把旧 `sup2api_appdata` 卷的 `plugins/blobs/`（11 个包）原样复制到 node-a 卷的 `plugin-blobs/`（托管后核心改从网关取包，不复制则已装插件起不来）；19:26:00 `init -manifest https://sup2api-gw-releases/sup2api/<清单>.json` 建 `updater` schema 并导入基线；19:26:05 node-a `/readyz` 200，19:26:09 node-b 200。入口约 6 秒不可用（流量几乎为 0）。
- 核验：两节点网关 0.1.137、核心 0.1.296（`updater.nodes` 两行 `self_update` true）；anthropic 0.3.3、codex 0.1.6、custom 0.1.1、gemini 0.3.1、moderation 0.12.1、openai 0.4.2 两节点 active、ready、restarts 0（dynweight、mailer、volcengine 原本只安装未启用）；三个入口域名 `/readyz` 200，超管经 https 域名登录、`/plugins`、`/accounts`、`/usage` 200；更新源为默认 GitHub，强制检测核心、网关都没有更新、无错误；日志只有验签关闭与 moderation 钩子匹配未安装平台的 WARN。
- 数据：同一个库、同一个 schema（`ab35f2f6…187d`），没有迁移；新增 `updater` schema，旧核心不读它。
- 回滚：`sh /opt/sup2api-gw/rollback.sh`（停 node-b、node-a，卷保留；`/opt/sup2api/compose.sh up -d app app-2` 起旧核心，入口端口相同，Caddy 不用改）。旧 compose、镜像 `sup2api:179142ae6` 与 `sup2api_appdata`/`appdata2` 卷都保留。
