# cpa-provider-nexus 开发知识库

维护约定：本文档是长期知识库，不是交接快照。每次排查出新根因、变更部署方式
或发现新的坑，把结论沉淀到对应章节；过期内容直接改写，不要另开新文件。

## 项目定位

`cpa-provider-nexus` 是 CLIProxyAPI（CPA）的独立 Linux 原生插件，不修改 CPA 主包。
插件以一个稳定身份聚合多个认证和模型来源；当前支持 Kiro 与 Cline。运行时只
需要 CPA 和插件 `.so`；`kiro-gateway`、`Kiro-Go`、`quotio-desktop` 仅作为
协议和登录流程参考。

仓库：<https://github.com/ViceEye/cpa-provider-nexus>

架构总览与目录职责见 `AGENTS.md`；本文档记录运行时行为、流程细节和经验教训。

### Provider 边界

- `internal/provider` 保留 CPA 方法分发、Kiro 凭据/OAuth/执行和统一管理接口；Kiro
  是本项目最早的核心实现，因此没有额外的 `internal/kiro` 目录。
- `internal/cline` 只承载 Cline 上游的凭据、OAuth、请求转换、Responses/SSE 和余额
  逻辑；这些协议和 Kiro 的 AWS Event Stream、请求格式并不相同，不强行合并。
- `internal/pluginrpc` 统一两边重复的宿主回调 wire format、HTTP stream 操作、响应
  envelope 和管理响应序列化。
- `integration/` 是正式的网络隔离端到端回归套件，不是临时目录；当前覆盖 Kiro，
  Cline 仍主要由包级测试覆盖。

### 身份约定

| 层级 | 值 |
| --- | --- |
| 项目、插件 ID、配置键 | `cpa-provider-nexus` |
| CPA Provider ID、凭证 `type` | `nexus` |
| 模型前缀 | `nexus/` |
| Kiro 凭证来源 | `kind: "kiro"` |
| Cline 凭证来源 | `kind: "cline"` |

`type` 用于 CPA 把认证记录路由到本插件，`kind` 只用于插件内部选择实际协议。
插件只接受 `type: "nexus"`，不保留旧 Provider ID 的兼容分支。

## 运行时与插件 ABI

- 插件是 Linux amd64/arm64 glibc 的 `c-shared` `.so`，必须匹配 CPA 容器架构，由宿主加载。
- C ABI：宿主通过 `cliproxy_plugin_init` 传入函数表，插件用
  `cliproxyPluginCall` 以 JSON 信封 `{ok, result, error}` 双向 RPC。
- 所有上游 HTTP 必须走宿主桥（`host.http.do` / `host.http.do_stream`），
  插件内不得直连网络。
- 控制台单文件 `internal/provider/console/index.html` 通过 go:embed 打进
  `.so`，改前端必须重建 `.so` 才生效。

## 凭证与认证体系

### 登录模式

| `login_mode` | 流程 | 适用 |
| --- | --- | --- |
| `kiro-browser`（默认） | `app.kiro.dev/signin` PKCE，回环回调 `http://localhost:3128` | 本机或可中继回调的场景 |
| `aws-device` | AWS 设备码授权，无浏览器回调 | 远程 CPA 推荐 |

- 组织账号（`sso_start_url` 为非默认 `*.awsapps.com/start`）在浏览器流程中
  会返回组织验证链接并转入设备码继续，最终凭证标记为 **Kiro Organization**。
- Kiro 生产登录页拒绝任意公网 redirect URI（只允许 localhost 或
  `app.kiro.dev` 子域），不要把 CPA 域名配成 `browser_redirect_uri`。

### 凭证 ID 策略（关键不变量）

- CPA 的认证记录 ID 由**物理认证文件的相对路径**推导（带 `.json` 后缀），
  文件扫描（`auth.parse`）和 `host.auth.save` 用同一套规则。
- 插件在 `auth.parse`（单账号文件）、命令行导入、`auth.refresh`、模型发现
  更新中**不得**返回自己的内容哈希 ID；返回空 ID/FileName 让宿主兜底，保存
  才能原地更新。**例外**：多账号文件必须保留逐账号内容哈希 ID，路径 ID 会
  在账号间碰撞。
- 凭据 JSON 内部的 `auth_id`（`kiro-<sha256前10字节>`）只用于插件内统计
  匹配，与 CPA 记录 ID 无关，刷新/重登后内容哈希变化是正常现象。
- 宿主记录 ID 不得写进凭据 JSON；刷新响应要合并请求 Attributes 以保留
  文件路径属性；重序列化凭据时必须从原存储 JSON 合并未建模字段
  （`disabled`/`priority`/`note`），否则刷新会丢停用标记。

### 凭证导入

`--kiro-import` 支持：Kiro IDE JSON、AWS SSO 缓存 JSON、Enterprise
`clientIdHash` 注册、kiro-cli / Amazon Q SQLite（`auth_kv`）、目录批量。
`reference` 模式跟随源文件刷新，`copy` 模式存独立副本。

### Cline 来源

- Cline 凭证以 `type: "nexus", kind: "cline"` 存储，刷新后仍保持该结构。
- OAuth 使用浏览器回调 URL 换取 token；所有请求继续通过 CPA 的宿主 HTTP 桥，
  插件本身不直连网络。
- Cline 模型目录和对话响应使用其 API 信封，插件解包后统一注册为
  `nexus/<vendor>/<model>` 并输出标准 OpenAI Chat Completions 格式。
- Cline 免费模型没有 Kiro 订阅额度结构；面板只展示其上游实际可取得的余额或
  错误状态。

## OAuth / 重新登录流程

v0.10.1 起，公开 `/v0/resource/plugins/` 仅返回静态控制台和图标。
旧 OAuth 资源回调返回 404，不能修改会话、调用宿主或返回动态跳转。
`browser_redirect_uri` 不再接受 `/v0/resource/` 地址；使用 localhost 回调，
由控制台通过带 Management Key 的管理接口提交，或改用 `aws-device`。


顶部 OAuth 登录（`/console/oauth/*`）和认证文件卡片“重新登录”
（`/oauth/relogin/*`）共用底层 `startLogin` / `pollLogin`，但**保存语义不同**：

- 顶部 OAuth：新增凭证，保存为 `authData.FileName`（新内容哈希名）。
- 卡片重新登录：保存回**原认证文件名**，并保留旧凭证中未被新响应覆盖的字段
  （`relogin.go` 的 merge 逻辑）。

浏览器流程步骤：

1. 插件返回 Kiro 登录 URL 和 `state`。
2. 浏览器完成登录后复制 localhost 回调 URL，粘贴提交到 `/oauth/callback`。
3. 组织账号返回 AWS device 验证链接（`processBrowserCallback` 转
   `beginDeviceAuthorization`），前端展示链接与设备码并继续轮询。
4. 成功后前端清空流程 UI（v0.7.6 起），仅保留成功提示。

## Quota 唤醒计划

- Nexus 控制台目前只为 **Codex** 和 **Antigravity** 提供每日唤醒计划；计划会在
  指定时区的指定时间，对指定 `auth_index` 发起一次真实模型请求，用来消耗一次调用
  并启动上游的 quota 窗口。它不是额度查询，也不会因为打开或刷新面板而触发。
- 唤醒请求构建和严格成功判定适配自
  `quota-activation` 的 Codex/Antigravity direct HTTP 路径；所有请求仍经 CPA 的
  `host.http.do` 宿主桥发送，计划不走其它凭证的调度回退。
- 控制台通过 CPA 通用插件配置接口持久化
  `plugins.configs.cpa-provider-nexus.quota_triggers`。后台调度器每 15 秒检查一次，
  同一计划同一自然日最多执行一次；计划创建或服务启动时若已错过当天时间，则等待
  下一天，避免意外补发调用。
- 唤醒模型由面板通过 CPA 的 `/v0/management/auth-files/models` 按所选凭证动态获取，
  不在 Nexus 中维护固定模型列表；如果 CPA 没有返回可用模型，新计划不能保存。每次计划
  的成功/失败状态仅保存在插件运行时，计划配置本身在重启后保留。

## 错误与状态映射

- `401`/`403`：刷新一次并重试，仍失败则上抛状态码。
- `402`/`429`：直接上抛，由 CPA 做冷却和账号切换。
- 网络错误与 `5xx`：可重试上游故障；畸形客户端载荷：不可重试 `400`。
- 令牌：到期前 10 分钟 CPA 计划刷新；请求前发现临期/缺失也会先刷新；
  轮换后的 token 写回原认证条目。8 小时正常过期无需人工干预。

## 请求限制

- Kiro 上游限制约 600 KiB / 900 KiB。转换器的压缩、图片丢弃、工具结果截断、
  历史裁剪是**降级策略**，不是无限上下文支持。
- Kiro Claude 模型对工具 schema 和历史 tool call 校验严格：转换器会清理孤立
  调用、展开顶层 `oneOf/allOf/anyOf`、合并重复 `toolUseEvent` 分片。

## 构建与发布

```powershell
# 1. 控制台（改了 console-ui/src 才需要）
cd internal/provider/console-ui && npm run build
Copy-Item dist\index.html ..\console\index.html -Force

# 2. 插件 .so（Docker Desktop 需在运行；自动跑 gofmt/vet/test）
$cfg = Join-Path $env:TEMP ("nexus_dockercfg_" + [guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $cfg | Out-Null
$env:DOCKER_CONFIG = $cfg
docker build --output type=local,dest=dist .
```

发版同步三处版本号：`internal/provider/types.go` 的 `pluginVersion`、
`Dockerfile` 产物名、README 版本说明。

## 服务器部署

生产服务器（下例以 `cpa.example.com` 指代，Docker 容器 `cli-proxy-api`）：

| 宿主路径 | 容器路径 | 内容 |
| --- | --- | --- |
| `/root/CLIProxyAPI/config.yaml` | `/CLIProxyAPI/config.yaml` | CPA 配置（插件配置在其 `plugins.configs.cpa-provider-nexus`） |
| `/root/CLIProxyAPI/auths` | `/root/.cli-proxy-api` | 认证文件目录（`auth-dir`） |
| `/root/CLIProxyAPI/plugins` | `/CLIProxyAPI/plugins` | 插件目录（旧版本改名加后缀即可，非 `.so` 结尾不会被加载） |

部署检查清单：

1. 把配置键从旧的 `plugins.configs.kiro-provider` 改为
   `plugins.configs.cpa-provider-nexus`，并移除旧插件 `.so`。
2. 上传 `.so` 和 `.sha256` 到服务器 `/tmp/`，`sha256sum -c` 校验。
3. 备份插件目录中的旧 `.so`（改名，如 `.bak-<说明>`）。
4. 安装新 `.so` 并再次校验。
5. `docker restart cli-proxy-api`。
6. 日志确认 `plugin registered plugin_id=cpa-provider-nexus version=...`。
7. `/auth-files` 确认 Kiro 记录数与磁盘文件数一致（防重复注册回归），
   `/v1/models` 确认模型出现，再做一次 Chat Completions 测试。

服务器部署与 Git 推送分开执行；未明确要求时不要自动更新服务器。

## 历史决策与根因记录

### v0.10.1 OAuth 资源路由安全修复部署（2026-10-03）

- GitHub Release `v0.10.1` 对应提交 `745cbc095b86f0088baa014de108c73cc48fc3cc`，
  发布工作流成功，公开 amd64 ZIP 的 SHA256 与 `checksums.txt` 一致。
- 同一标签源码在 `ai.venja.cc` 原生 ARM64 构建，`go vet ./...`、`go test ./...`
  和插件编译通过；2026-10-03 12:59 UTC 更新并重启 CPA。
- 部署插件 SHA256：`38f5750531f206150d64ee33aa6dcd3710aa4eff16bb9911f181af3f3110c8cd`。
  旧版备份：`/root/CLIProxyAPI/plugins/cpa-provider-nexus-v0.10.0.so.bak-oauth-fix-20261003`。
- 启动日志确认 Nexus v0.10.1 和 key-policy v0.5.1 注册成功；更新前后管理接口的
  8 条凭据身份一致，控制台和模型列表均返回 200。
- 旧公开 OAuth 路径返回 404 且无跳转；匿名管理回调被拒绝，带管理鉴权的
  合成未知 state 请求返回预期的 400。
- `nexus/auto` 实际推理检查返回 400：`Kiro token refresh failed: Invalid request`。
  尚未确认 Kiro 推理可用，需要检查账号授权状态；不能将路由健康等同于推理成功。

### v0.10.0 WorkBuddy 接入审查（2026-09-28）

- 已部署到 `ai.venja.cc` 的 ARM64 CPA（2026-09-28 22:54 UTC）。amd64/arm64
  构建均通过 `go vet ./...`、`go test ./...`；WorkBuddy 另通过 race 测试。
  启动日志确认 Nexus v0.10.0 与 key-policy v0.5.1 均注册成功，管理接口的
  6 条认证身份未变，控制台和模型列表返回 200，国内/国际 OAuth start 均成功。
  尚无 WorkBuddy 账号，未验证实际授权完成、配额及推理。
- 旧插件备份为
  `/root/CLIProxyAPI/plugins/cpa-provider-nexus-v0.9.5.so.bak-workbuddy-20260928`；
  已部署 ARM64 成品 SHA256：
  `a1bc3969ba516f736619dc9b873dd568823d7fe0c24e83af32f13a276a6f0fac`。
- `internal/workbuddy` 通过宿主 HTTP 桥实现国内/国际网关，凭据固定为
  `type: nexus, kind: workbuddy`，`region` 为 `cn` 或 `intl`。
- 请求前与 401/403 后刷新必须写回请求的物理文件名，不能按新 token 重新计算文件名；
  额度查询传递原始 `name`。刷新还必须保留 `disabled/priority/note` 等未知字段。
- 非流式聚合不接受空响应、非 SSE JSON、错误事件或缺少完成标记的截断响应；
  工具索引可不连续。流式桥的 `error` 字段必须显式上抛。
- 当前沿用 9router 的网关提示词兼容逻辑，会改写 CN 的 agent system prompt，
  Intl 会替换 system/developer 内容；不能视作透明 Responses 或原生 compact 通道。
- 模型目录为静态列表，真实账号权限、OAuth 和推理可用性需要线上授权后确认。
- 部署前同时核对宿主、CPA 镜像、插件 ELF 架构。Dockerfile 使用 BuildKit 的
  `TARGETARCH` 输出 `dist/linux/<arch>/`；ARM64 CPA 必须使用 ARM64 `.so`，
  可在 ARM64 服务器原生构建，避免 CGO 使用不匹配的交叉编译器。

### v0.7.6 / v0.7.7（2026-08-29）重新登录后重复凭证

- **现象**：重新登录成功后原凭证刷新了，但面板多出一个新凭证；且旧版本会话
  中曾出现磁盘上多出 `kiro-<hash>.json` 文件。
- **根因**：插件 `auth.parse` 返回凭据内容哈希 ID（`kiro-<hash>`，无
  `.json`），CPA `host.auth.save` 按文件路径注册 ID（`xxx.json`）。同一物理
  文件被挂两条管理器记录（`GetByID` 找不到路径 ID 记录就 `Register`）。
- **修复**：插件在 `auth.parse`（仅单账号文件）、导入、刷新、模型发现更新中
  不再自带 ID；宿主记录 ID 不写入凭据 JSON；`persistCredentialBestEffort`
  不再盲目拼 `.json` 后缀。**多账号文件必须保留逐账号内容哈希 ID**——
  路径 ID 会在账号间碰撞（v0.7.7 review 发现并回归测试锁定）。
- **附带修复**：`auth.refresh` 响应原来直接重序列化凭据结构体，会丢掉插件
  未建模的存储字段（`disabled`/`priority`/`note`），刷新一次就把停用的凭证
  复活；现在从原存储 JSON 合并未知字段回来。
- **验证方式**：重启后 `/auth-files` 仅一条 Kiro 记录，触发真实 token 刷新
  后仍是一条；`TestRefreshAuthPreservesUnknownStoredFields` 锁定字段合并。

### 其他已知事项

- Kiro session/refresh token 可能过期且不可刷新，需要通过卡片“重新登录”
  重新授权。
- `profileArn` 缺失时插件自动发现；上游仍拒绝时需重新登录或导入完整凭证。
- 当前工作树包含未提交的历史开发改动，**禁止 `git reset --hard` 清理**。
- 临时调试脚本放 `temp/`（gitignore），正式测试在 `integration/`。
