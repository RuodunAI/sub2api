# 网关内置的发布信任公钥

本目录下每个 `<key id>.pub` 是一把编进网关二进制（`go:embed`）的 Ed25519 发布公钥：

- 文件名去掉 `.pub` 就是 key id，即签名清单 `rc.SignedManifest.key_id`；
- 文件内容是公钥的标准 base64（32 字节），与网关配置 `trusted_keys` 的值格式相同，首尾空白会被去掉。

网关启动时把这些公钥与配置的 `trusted_keys` 合并，核心发布清单与网关发布清单（`next-core-manifest.json`、`next-gateway-manifest.json`）都用合并后的集合验签。同一个 key id 在两处对应不同公钥时网关拒绝启动；格式不对的文件同样拒绝启动。

目录里没有 `.pub` 文件时网关只信任配置的 `trusted_keys`。

现在的官方发布公钥是 `sup2api-ovh-2026.pub`（2026-11-17 加入）：签名线上核心清单的同一把密钥，GitHub Actions 的 `release` 环境用它签核心与网关的 Release（`SUB2API_RELEASE_KEY_ID` = `sup2api-ovh-2026`）。

**分叉（fork）自行发布时**：删除这里的公钥文件，换成自己的发布公钥（`sub2api-release keygen` 生成），并在自己的发布流水线里用对应私钥签名。不要保留上游公钥，除非也打算安装上游的发布。
