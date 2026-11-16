# 设计：网关与核心新版本检测、一键手动更新（2026-11-17）

用户确认的设计稿：https://claude.ai/artifact/5hRtbJjMVrB6GQhgMxAScq 。本文是实现用的接口契约，实现完成后并入 CONTRACTS（§40 改写 + 新节）并删除本文。

命名：`next/gateway`（二进制 `sub2api-gateway`）对用户一律叫“网关”，`next/server` 叫“核心”。代码里已有的 shell 标识符不改名。

## 1. 已定的决定

- A：两条版本线都由 GitHub Actions 构建、签名并发布到 GitHub Release，以 Release 产物为准；线上也从 Release 导入。
- B：核心和网关都提供“一键手动更新”：管理员点击才执行，从不自动升级。
- C：更新源两种：`github`（github.com 公开或私有仓库，可选只读令牌）与 `url`（任意 HTTPS 发布目录 + `releases.json` 索引，可选 Bearer 密钥）。
- D：不发邮件，只在控制台提示。
- 不保留旧写法：`v*` tag + `releases/latest`、`POST /system/releases/import`（GitHub 导入）、按网关内存缓存的检测结果都删除。`release_origin` + CLI `import -manifest` 保留（离线 / 手工发布仍用它）。

## 2. 版本线与 tag

| 版本线 | `line` | tag | 签名清单资产 | 安装物资产 |
|---|---|---|---|---|
| 核心 | `core` | `core-v<semver>` | `next-core-manifest.json`（现有 `rc.SignedManifest`，payload `rc.Manifest`） | 每平台 `<bundle_digest>.tar.gz` |
| 网关 | `gateway` | `gateway-v<semver>` | `next-gateway-manifest.json`（`rc.SignedManifest`，payload `rc.GatewayManifest`） | 每平台 `sub2api-gateway_<version>_<os>_<arch>.tar.gz` |

- 只认 `StrictNewVersion` 合法、无预发布后缀的版本；草稿和 prerelease 忽略。
- 清单里的版本号必须等于 tag 去掉前缀后的版本（核心 `core_version`，网关 `gateway_version`）。

## 3. 网关签名清单（runtime-contract，新文件 `gateway_manifest.go`）

```go
// GatewayManifestKind separates gateway manifests from core manifests: a
// signature over one can never verify as the other.
const GatewayManifestKind = "sub2api-gateway-release-v1"

type GatewayManifest struct {
    Kind            string            `json:"kind"`             // == GatewayManifestKind
    GatewayVersion  string            `json:"gateway_version"`
    ReleaseID       string            `json:"release_id"`
    SourceCommit    string            `json:"source_commit"`
    BuildID         string            `json:"build_id"`
    PeerProtocol    int               `json:"peer_protocol"`    // control.PeerProtocol of the build
    RuntimeProtocol int               `json:"runtime_protocol"` // the gateway↔core runtime-contract protocol it speaks
    Platforms       []GatewayPlatform `json:"platforms"`
    Images          []GatewayImage    `json:"images"`           // informational: GHCR refs pinned by digest
}
type GatewayPlatform struct {
    OS, Arch      string // json "os", "arch"
    Archive       string // json "archive": asset file name
    ArchiveDigest string // json "archive_digest": sha256 hex of the archive
    ArchiveBytes  int64  // json "archive_bytes"
    BinaryDigest  string // json "binary_digest": sha256 hex of the extracted sub2api-gateway
}
type GatewayImage struct{ Ref, Digest string } // json "ref", "digest"
```

- 签名与核心清单同一个信封与密钥；网关清单验签时要求 `kind` 正确，核心清单验签时拒绝带 `kind` 的 payload（两边都用严格解码 `DisallowUnknownFields`）。
- 归档里只有一个常规文件 `sub2api-gateway`，解包限大小、拒绝路径穿越。

## 4. 更新源抽象（网关，新包 `gateway/internal/updatesource`）

```go
type Kind string // "github" | "url"

type Config struct {
    Enabled    bool
    Kind       Kind
    Repository string // github: normalized "owner/repo"
    URL        string // url: normalized https base, trailing "/"
    Secret     string // plaintext, only in memory
    Revision   int64
}

type Listing struct {
    Line        string    // "core" | "gateway"
    Version     string    // semver without prefix
    Tag         string    // github tag or "<line>-v<version>" for url
    PublishedAt time.Time
    Notes       string
    Page        string    // human page (GitHub release page / index "page")
    Assets      map[string]Asset // by file name
}
type Asset struct {
    Name string
    Size int64
    Ref  string // github: numeric asset id; url: path relative to the index
}

type Source interface {
    // Releases lists stable releases of both lines, newest first per line.
    Releases(ctx context.Context) ([]Listing, error)
    // Open streams one asset (≤ max bytes). The secret goes only to the API
    // host (github) or the configured origin (url).
    Open(ctx context.Context, a Asset, max int64) (io.ReadCloser, error)
}

func New(cfg Config, opts Options) (Source, error) // Options: AllowPrivate bool, Client *http.Client (tests)
func Latest(ls []Listing, line string) (Listing, bool)
```

- GitHub：`GET https://api.github.com/repos/<r>/releases?per_page=50`；资产一律 `GET /repos/<r>/releases/assets/<id>` + `Accept: application/octet-stream`，只对 `api.github.com` 带 `Authorization: Bearer`，跳转只允许到 GitHub 资产 CDN 且清空请求头（沿用 `release.GitHubGet` 的策略，提炼复用，删掉旧的 `releases/download` 路径）。
- URL：`GET <base>releases.json`；资产 `GET <base><path>`，路径必须相对、不得含 `..`、解析后与 base 同源；不跟随跨源跳转；密钥只对同源带。索引格式：

```json
{"schema": "sub2api-releases-v1",
 "releases": [{"line": "core", "version": "0.1.291", "published_at": "2026-11-20T08:00:00Z",
               "notes": "…", "page": "https://…",
               "assets": {"next-core-manifest.json": {"path": "core/0.1.291/next-core-manifest.json", "size": 1234}}}]}
```

- 网络：不用环境代理；连接时解析并固定地址，默认只允许公网地址（`release.publicIP`），网关配置 `allow_private_update_sources: true` 时 `url` 来源允许内网。所有响应限大小（列表 / 索引 2 MiB，清单 `release.MaxManifestBytes`）。
- 错误信息不含 URL 的 userinfo / query，不含密钥。

## 5. 存储（网关 `control/schema.sql`，只做幂等的加列加表）

- `updater.clusters`：
  - `update_check_enabled boolean NOT NULL DEFAULT true`
  - `update_source_kind text NOT NULL DEFAULT 'github'`
  - `update_repository` 默认改为 `'Sub2API-Devs/sup2api'`，并 `UPDATE … SET update_repository='Sub2API-Devs/sup2api' WHERE update_repository=''`（新语义下 github 来源必须有仓库，关闭检测用开关）
  - `update_url text NOT NULL DEFAULT ''`
  - `update_secret_enc bytea`（NULL = 未设置）
  - 已有 `update_source_revision`：任何来源字段或密钥变化都 +1
- `updater.update_checks`（每集群每版本线一行）：`cluster_id, line, PRIMARY KEY(cluster_id,line), source_revision bigint, baseline text, result jsonb, checked_at timestamptz, last_success_at timestamptz`。
- `updater.nodes.gateway_version text NOT NULL DEFAULT ''`：心跳写入。
- 网关发布与计划（W2）：`updater.releases` 加 `line text NOT NULL DEFAULT 'core'`；从更新源导入的发布加 `source_assets jsonb`（文件名 → `Asset`）与来源 revision，下载走 `Source.Open`；`bundle_base` 只给 `release_origin` 导入的发布用。同一集群任何时刻只有一个进行中的计划（现有唯一索引覆盖两条版本线）。

密钥：`runtime-contract/secret`（已从核心移来）用 `SUB2API_MASTER_KEY`（base64 32 字节，网关进程本来就持有）加密，AAD = `updater:source-secret:<kind>:<repository 或 url>`。主密钥缺失时设置密钥返回 400 `master_key_missing`，公开来源照常。

## 6. 网关管理接口（核心经 `server/internal/updater` 原样转发，前缀 `/api/v1`）

| 方法 路径 | 权限（核心桥） | 说明 |
|---|---|---|
| `GET /system/update-source` | `settings:read` | `{enabled, kind, repository, url, has_secret, default_repository, revision}` |
| `PUT /system/update-source` | `settings:manage` + 二次认证 | 请求 `{enabled, kind, repository, url, secret?}`：`secret` 省略 = 不变，`""` = 清除，非空 = 设置；kind/repository/url 任一变化且未给新 secret 时清除旧密钥。成功后异步触发一次检测。响应同 GET |
| `GET /system/update-check[?force=true]` | `system:update:read` | 读 `update_checks`；`force` 时同步检测（30 秒内重复返回已有结果） |
| `POST /system/updates/apply` | `system:update:execute` + 二次认证 | `{line, version, confirm}` → `{status:"started", plan_id}` 或 `{status:"needs_confirmation", reasons:[…], preflight:{…}}` |

`update-check` 响应：

```json
{"enabled": true,
 "source": {"kind": "github", "repository": "Sub2API-Devs/sup2api", "url": ""},
 "core": {"current_version": "0.1.290", "latest_version": "0.1.291", "has_update": true,
          "compatible": true, "reason": "", "strategy": "rolling", "tag": "core-v0.1.291",
          "notes": "…", "page": "https://…", "published_at": "…",
          "checked_at": "…", "last_success_at": "…", "error": ""},
 "gateway": {"nodes": [{"node_id": "sup2api-1", "version": "0.1.135"}],
             "latest_version": "0.1.136", "has_update": true, "compatible": true, "reason": "",
             "self_update": true, "image": "ghcr.io/…@sha256:…", "tag": "gateway-v0.1.136",
             "notes": "…", "page": "…", "published_at": "…",
             "checked_at": "…", "last_success_at": "…", "error": ""}}
```

- `error` 非空表示最近一次检测失败，其余字段保留最近一次成功的结果；从未成功时 `latest_version` 为空。前端据此显示“检测失败”，绝不显示成“已是最新”。
- 核心 apply：导入（验签、存 `releases`）→ 预检 → 预检无阻断且策略为 rolling 时直接创建计划；有阻断返回 409 + blockers；策略为 primary-first（停服）且 `confirm` 不为 true 时返回 `needs_confirmation`。
- 网关 apply（W2）：导入网关发布 → 预检（各节点可写 `<root>/gateway`、协议兼容、≥2 个可让位节点；只有一个节点时 `needs_confirmation`）→ 创建 `gateway-rolling-v1` 计划。
- 核心桥审计：`system.update_source.update`（details 只记 kind、repository、url、`secret: "set"|"cleared"|"unchanged"`，绝不记密钥）、`system.update.apply`（line、version、plan_id）。

## 7. 检测

- 网关后台：每节点控制循环里一个 6 小时定时器（启动后 1 分钟先跑一次），只有升级主节点执行，再加 PG 咨询锁防并发。保存来源后也触发一次。
- 一次检测 = 一次 `Releases` + 每条版本线最多一次清单下载与验签；结果按 `source_revision` + 集群基线写入 `update_checks`。
- 核心：验签、`core_version` == tag、当前平台存在、所有平台 bundle 资产存在且大小一致、`Compatibility`；`strategy` 取自清单。
- 网关：验签（内置公钥 + `trusted_keys`）、`kind`、版本 == tag、各节点平台都有归档、`peer_protocol` == 集群当前值、`runtime_protocol` 被当前核心支持；`self_update` = 所有节点上报了 `gateway_version` 且是会自更新的版本。
- 失败、限流、签名不受信（报出 `key_id`）、资产不全都写 `error`，保留上次成功结果。

## 8. 网关版本号与内置公钥

- `gateway/cmd/sub2api-gateway`：`var Version = "0.0.0-dev"`，构建注入 `-X main.Version=<ver>`；子命令 `sub2api-gateway version`；传入控制层，心跳写 `gateway_version`，`Node` JSON 带 `gateway_version`。
- 内置信任公钥：`gateway/internal/release/trusted/*.pub`（`go:embed`，文件名去 `.pub` = key id，内容 = 与 `trusted_keys` 相同的 base64 公钥），与配置的 `trusted_keys` 合并；同 id 不同公钥时启动失败。

## 9. 网关一键更新（W2）

- 计划策略 `gateway-rolling-v1`：从节点在前、主节点最后，每节点 `gw-prepare`（下载、验 `archive_digest`、解包、验 `binary_digest`，放到 `<root>/gateway/<version>/sub2api-gateway`）→ `yield` → `stop`（停本机核心）→ `gw-swap`（写 `<root>/gateway/current` 指向新版本，持久化步骤后 `exec` 新二进制）→ 新网关启动、拉起核心、`admit`、`local` → `gw-confirm`（心跳上报的 `gateway_version` == 目标）→ `resume`。
- 启动选版本：网关启动最先检查 `<root>/gateway/current`；其版本高于自身且二进制摘要与记录一致时 `exec` 它；否则用镜像自带的。损坏或不一致时记录 WARN 并继续用自带版本。
- 回退：保留上一版本目录；计划回退把 `current` 指回上一版本并按同样步骤执行。
- 只有一个节点时步骤相同但无处让位，需 `confirm`。

## 10. 发布流水线（W4）

- `.github/workflows/next-release.yml`：tag `core-v*` / `gateway-v*` 触发；先跑与 next-ci 相同的门禁；构建 linux amd64/arm64；签名步骤在受保护环境 `release` 中读 Secrets `SUB2API_RELEASE_SIGNING_KEY`（网关清单与核心清单）、`SUB2API_PLUGIN_SIGNING_KEY`（内置插件），变量 `SUB2API_RELEASE_KEY_ID`、`SUB2API_PLUGIN_KEY_ID`；缺失时失败并说明。先建草稿 Release，资产全部上传后再发布。
- 核心：`schema_before` 取上一个 `core-v*` Release 清单的 `schema_after`（没有则等于本版）；发布说明取 `CHANGELOG.md` 对应版本小节。
- 网关：二进制归档 + `sub2api-release pack-gateway` 签名清单；多架构镜像推 `ghcr.io/<owner>/sup2api-gateway:<version>`，清单里记录镜像摘要。
- `sub2api-release mirror`：把某个来源的若干 Release 下载成 `url` 来源的目录结构并生成 `releases.json`。
- 构建脚本给网关注入版本号（`deploy/docker/build-go.sh`、`deploy/gateway/Dockerfile` 的 `ARG VERSION`）。
