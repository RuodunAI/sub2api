# 用 GitHub Actions 发布核心与网关

核心与网关两条版本线都由 `.github/workflows/next-release.yml` 在 GitHub Actions 里构建、签名并发布成 GitHub Release；节点的版本检测与一键更新以 Release 里的签名清单为准（接口契约见 `next/docs/DESIGN-2026-11-17-UPDATE-SOURCES.md` §2、§3、§10）。插件仍走各自的批准与独立发布，内置插件随核心包发布。

| 版本线 | tag | 签名清单 | 安装物 |
|---|---|---|---|
| 核心 | `core-v<X.Y.Z>` | `next-core-manifest.json`（payload 是 `rc.Manifest`） | 每个平台一个 `<bundle_digest>.tar.gz`（`bin/` + `builtin/`） |
| 网关 | `gateway-v<X.Y.Z>` | `next-gateway-manifest.json`（payload 是 `rc.GatewayManifest`） | 每个平台一个 `sub2api-gateway_<X.Y.Z>_linux_<arch>.tar.gz`（只含 `sub2api-gateway` 一个文件），另推镜像 `ghcr.io/<owner 小写>/sup2api-gateway:<X.Y.Z>` |

版本号只认严格语义化版本（`X.Y.Z`，不带前缀、预发布或构建后缀），且必须大于同一条线已发布的最新版本。平台固定为 `linux/amd64` 与 `linux/arm64`。清单的 `release_id` 是 tag，`build_id` 是 `github-actions-<run_id>-<attempt>`，`source_commit` 是 tag 指向的完整提交。

## 流水线做什么

1. **validate tag**：校验 tag，经 GitHub API 找同一条线已发布的最新 Release，版本不大于它就失败；从 `CHANGELOG.md` 取发布说明（找不到就失败，格式见下文）。核心线还下载上一个 `core-v*` 的 `next-core-manifest.json`，读出它签名的 `schema_after` 和 `builtin/trust.pub` 摘要。
2. **gate**：调用 `next-ci.yml`（`workflow_call`），对 tag 指向的提交跑与日常 CI 相同的全部模块门禁。
3. **build**（不接触任何密钥）：
   - 核心：`deploy/docker/build-ui.sh` 构建控制台与插件原生界面；`deploy/docker/build-go.sh` 构建 linux/amd64 的核心、插件 CLI 与全部插件，内置插件集合与镜像构建完全相同（`BUILTIN_PLUGINS`），打包时连 PostgreSQL 服务容器做 `db check`；再用 `GOARCH=arm64 GO_BINARIES_ONLY=1` 构建 arm64 的两个二进制。Go 版本取 `next/Dockerfile` 的 `GO_IMAGE`。核心必须报告与 tag 相同的版本号，`schema-contract` 作为 `schema_after`。这一步的插件包用临时生成的密钥签名，只为让脚本照常跑完。
   - 网关：`deploy/gateway/Dockerfile` 交叉编译两个平台（`--build-arg VERSION` 注入 `main.Version`），导出二进制打成归档；非演练时用同一构建阶段把多架构镜像只按摘要推送（不打版本标签）并记录摘要。
4. **sign**（受保护环境 `release`，唯一能读签名密钥的任务）：`sub2api-release` 和（核心线）`sub2api-plugin` 在本任务里从同一提交的源码构建，构建产物里的任何文件都不在这里执行。
   - 核心（`.github/next-release/sign-core.sh`）：用插件密钥重签全部内置插件包，写入 `builtin/trust.pub`、`trust.keyid`；若上一版核心带的 `trust.pub` 与之不同则失败（换插件密钥会让已装插件验签失败）；按 `package-release.sh` 的布局（`bin/` + `builtin/`）为每个平台组 stage，一次 `sub2api-release pack --platform …` 签出含两个平台的清单。`schema_before` 取上一版清单的 `schema_after`；没有上一版时必须由 `workflow_dispatch` 的 `schema_before` 输入给出（见下文“注意”）。
   - 网关（`.github/next-release/sign-gateway.sh`）：`sub2api-release pack-gateway` 校验归档（只有一个可执行的常规文件 `sub2api-gateway`）并签出清单，含各平台归档与二进制摘要、镜像引用与摘要、`peer_protocol`、`runtime_protocol`。
5. **publish**：之前失败留下的同 tag 草稿会被删掉重建；已发布的 Release 从不修改（镜像标签也不再动），出错只能发下一个版本。网关线这时才给清单记录的镜像摘要打上 `:<X.Y.Z>` 标签。然后先建草稿 Release，上传全部资产，再发布，检测方不会看到只有一半资产的 Release。

`workflow_dispatch` 可以对已有 tag 重跑：填 `tag`，`dry_run` 默认勾选；`schema_before` 只在第一个 `core-v*` 发布时填。演练照常经过门禁、构建与签名（仍需审批 `release` 环境），但不推镜像、不建 Release，签好的资产在运行的 `release-assets` artifact 里（保留 7 天）。建议在 “Use workflow from” 里选这个 tag 本身或默认分支，各任务都检出 tag 指向的提交。

## CHANGELOG 小节的格式

发布说明取自 tag 提交里的根目录 `CHANGELOG.md`（`.github/next-release/changelog-notes.sh`）：第一个名字里写着 `核心 X.Y.Z`（核心线）或 `网关 X.Y.Z`（网关线）的三级标题，版本号后面不能紧跟数字或“.数字”。一个标题可以同时写两条线，例如：

```markdown
## 2026-11-20

### 核心 0.1.291 + 网关 0.1.135：一句话概括（`<commit>`，滚动；无迁移）

- 用户能看到的变化 …

#### 子节也会一起带上
```

Release 标题是这个标题的文字，正文到下一个一至三级标题为止（`####` 子节包含在内）。所以打 tag 之前要先把“未发布”里的内容写成这样的一节并合并；上线后的时间、计划号、核验结果仍照常补在同一节里。

## 仓库要做的配置（自有仓库与 fork 相同）

1. **密钥**（在离线的可信机器上生成，私钥不进仓库、不上节点）：
   - 发布密钥签核心与网关清单：`go run ./gateway/cmd/sub2api-release keygen --out ./keys`（在 `next/` 下），得到 `release.key`、`release.pub`。已有托管集群时沿用节点 `trusted_keys` 里已经信任的那把密钥。
   - 插件密钥签内置插件：`go run ./tools/sub2api-plugin keygen --key-id <key-id> --out ./keys`。已有部署必须沿用线上内置插件的签名密钥（镜像构建缓存 `sub2api-next-devkeys` 里的 `<key-id>.key`，key id 默认 `sub2api-dev`），否则 sign 会因为 `trust.pub` 变了而失败。
2. **内置信任公钥**：把发布密钥的公钥提交为 `next/gateway/internal/release/trusted/<发布 key id>.pub`，内容是 `sub2api-release pubkey --key release.key` 的输出（也就是 `release.pub` 的内容）。网关把它与配置的 `trusted_keys` 合并；fork 用自己的密钥时，先发布带这个文件的网关，节点才认新密钥签的 Release。
3. **环境 `release`**（Settings → Environments → New environment）：
   - Required reviewers：指定能批准发布的人，取消 “Allow administrators to bypass”。
   - Deployment branches and tags：Selected，加 tag 规则 `core-v*`、`gateway-v*`，再加默认分支（供 `workflow_dispatch` 用）。
   - Environment secrets：`SUB2API_RELEASE_SIGNING_KEY` = `release.key` 的内容；`SUB2API_PLUGIN_SIGNING_KEY` = `<key-id>.key` 的内容（各是一行 base64）。
   - Environment variables：`SUB2API_RELEASE_KEY_ID` = 发布 key id（与 `trusted/<id>.pub`、节点 `trusted_keys` 的 id 相同）；`SUB2API_PLUGIN_KEY_ID` = 插件 key id。
   缺任何一项，sign 会失败并列出缺的名字。
4. **tag 保护**：Settings → Rules → Rulesets 新建 tag 规则，目标 `core-v*`、`gateway-v*`，限制创建、禁止更新与删除，只有发布负责人能推这类 tag。
5. **GHCR**：第一次发布网关后，在仓库所有者的 Packages 里打开 `sup2api-gateway` → Package settings：确认已关联本仓库（镜像带 `org.opencontainers.image.source` 标签）；节点要免登录拉取就把可见性改成 Public。组织仓库还要在组织设置里允许公开包。
6. Actions 的默认 `GITHUB_TOKEN` 权限保持只读即可，工作流按任务声明所需权限（`contents: write` 只给 publish，`packages: write` 只给网关构建和 publish）。

fork 完成以上配置后，在自己 fork 的控制台“设置 → 在线更新”把来源改成自己的 `owner/repo`。

## 发布

```sh
# CHANGELOG 小节、released.json 等已经合并；tag 打在要发布的提交上
git tag core-v0.1.291 <commit>
git push origin core-v0.1.291
```

然后在 Actions 里批准 `sign` 任务，等 publish 完成，核对 Release 的资产与 sign 任务摘要里的 sha256。核心与网关各打各的 tag，可以同一个提交。

注意：

- **第一个 `core-v*` 发布**没有上一版清单可读：推 tag 触发的运行会在 validate tag 失败。在 Actions 里手动运行本工作流，填这个 tag、取消 `dry_run`，`schema_before` 填它要升级的线上核心的 `sub2api schema-contract` 输出（64 位十六进制）；与本版相同时策略记为 `rolling`，不同时为 `maintenance`（主节点优先升级）。
- 镜像按摘要推送发生在审批之前；审批被拒时 GHCR 里只留下一个无标签的版本，可在包设置里删掉。节点只认清单里记录的镜像摘要。
- 旧网关（本流水线之前的版本）只认 `v*` tag 的 `releases/latest`，会把新 tag 当成检测失败，不会误装。

## 镜像源：`sub2api-release mirror`

把某个来源的若干 Release 原样下载成 `url` 类型更新源的目录（`<line>/<version>/<资产>` + `releases.json`，`schema` 为 `sub2api-releases-v1`），清单不改、不重签。再次运行会合并 `releases.json`：同一版本的条目被替换，其他保留；已存在且大小一致的资产不重新下载。

```sh
cd next/gateway && go build -o /usr/local/bin/sub2api-release ./cmd/sub2api-release

# GitHub：每条线最新 3 个稳定版；私有仓库把只读令牌放在环境变量里
GITHUB_TOKEN=… sub2api-release mirror --github owner/repo --token-env GITHUB_TOKEN --latest 3 --out /srv/sub2api-releases
# 指定 tag
sub2api-release mirror --github owner/repo --tags core-v0.1.291,gateway-v0.1.135 --out /srv/sub2api-releases
# 从另一个 url 来源转存（可选 Bearer 密钥）
MIRROR_SECRET=… sub2api-release mirror --url https://releases.example.com/sub2api/ --token-env MIRROR_SECRET --latest 5 --out /srv/sub2api-releases
```

令牌只发给 `api.github.com`（或 url 来源本身的源站），跳转到其他源站时去掉。用任意 HTTPS 服务器发布这个目录，在控制台把更新源设为 `url`，地址填目录 URL（以 `/` 结尾）。

## 离线与手工发布

不经 GitHub 的发布仍用 `deploy/gateway/package-release.sh` / `sub2api-release pack` 生成清单，放到节点的 `release_origin` 后用 CLI `import -manifest` 导入，见 `deploy/gateway/README.md`。`pack` 用 `--platform os/arch=DIR`（可重复）把多个平台签进同一个清单。
