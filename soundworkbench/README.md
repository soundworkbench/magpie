# soundworkbench/magpie：本地构建

> English summary: this fork carries small patches on top of an upstream magpie release, on the `local` branch. `soundworkbench/build.sh` builds the image `magpie-local:<upstream version>-xm` from that branch, after running the patches' tests.

## 为什么有这个分叉

我们的 magpie 跑在上游发布版上，另加上游没有的补丁。补丁放在 `local` 分支：`local` = 上游某个发布标签 + 我们的提交。`main` 跟上游，不放我们的改动。

镜像只从 `local` 构建。家里节点和公司节点都跑同一份镜像。

## 补丁

### 1. routing: a Remote magpie is weighed by its account's windows

问题：中心 magpie 把别的 magpie 当作远端（Remote magpie）接入时，用量页能显示远端账号的额度窗口，调度却把远端当作“额度未知”。一组模型如果由几个远端 magpie 组成（每个远端只有一个 Claude 账号），调度只按成员顺序选，不看谁的额度先重置。

做法：远端 magpie 按账号的方式参与调度。额度取自中心 magpie 已经保存的远端用量卡片，用远端上服务该模型的那个代理的账号窗口；远端有几个这类账号时，取用得最少的那个。卡片过期时在请求之后后台重读，请求本身不等。

改动：`internal/provider/remote_allowance.go`（新增 `RemoteAllowance`），`internal/gateway/routing.go`（`allowance` 和 `weighRouted` 改为先读钥匙自己的窗口，读不到再读远端账号的窗口）。

我们为什么需要：我们每个 Claude 账号单独跑一个 magpie，中心 magpie 以远端方式接入它们，按额度在账号之间调度。

上游为什么没有：待确认。上游代码里远端 magpie 明确记为“未知”（`any other key isn't known`），没有找到说明原因的记录。

### 2. web_fetch: read a page by magpie, then by the ChatGPT backend's open when magpie can't

问题：`web_fetch` 只由 magpie 自己下载网页。有的站点返回验证页（“环境异常”“完成验证后即可继续访问”、Cloudflare 的 “Just a moment...” 等），或返回空文本，调用方拿不到内容。

做法：magpie 先自己下载。下载失败、正文为空、或正文是短的验证页时，如果设置里的网页搜索选的是 ChatGPT 账号的后端搜索，就改用该后端的 open 命令读这个网址。open 按窗口分段返回带行号的文本；补丁逐段续读，去掉行号、链接编号和抓取元数据，拼成正文。读不全时在末尾注明读到第几行。open 也失败时，返回原来下载失败的错误。

改动：`internal/gateway/search_fetch.go`（回退判断、验证页识别 `fetchVerification`），`internal/gateway/search_fetch_open.go`（新增 `codexFetch`、`codexOpen`、正文清理）。

设置里没有选 ChatGPT 后端搜索时，行为和上游一样。

上游为什么没有：待确认。

### 3. gateway: an image Claude refuses does not break the replay

问题：重放历史消息或传入工具结果时，图片的声明类型与真实字节不符、格式不受支持或文件不完整，会让 Claude Code 拒绝整轮对话。

做法：按字节识别 PNG、JPEG、GIF、WebP；能读取的其他格式在总像素不超过 3840x2160 时转成 PNG，无法读取或超过转换像素上限的图片换成一行说明。完整但 Go 无法解码的受支持格式保留给 Anthropic 判断，正确标注且无需缩放的图片保持原字节。检查结果缓存，重放不重复解码。Claude Code 的 `media_removed` 若自行继续，就等待重发的回复；若未能回复，则返回 400 `invalid_request_error`，已开始的流发送错误事件，不轮换或休息账号。

改动：`internal/gateway/claude_images.go`、`claude_subscription.go`、`internal/claudetui/transcript.go` 及其测试。构建前新增 `ClaudeImage|ClaudeReplay|ClaudeTUIReplay|SubscriptionToolResultImages|LostMedia|ToolImages|MediaRemoved`，并测试 `./internal/claudetui/`。当前轮图片粘贴文件的路径不变。

### 是否提给上游

这些补丁尚未提给上游。上游只接受协作者的 PR，社区的修复以 issue 的形式提交（见 `docs/contributors.md`）。补丁 1 修的是上游调度里一个可复现的不一致，适合附补丁提 issue；补丁 2 依赖 ChatGPT 后端的 open 接口，上游是否接受待确认。上游合入同等修复后，在下一次变基时丢掉我们的那个提交。

## 维护 local 分支

远端约定：`upstream` = yetone/magpie，`origin` = soundworkbench/magpie。

升级到上游新版本（例：从 v0.1.3315 升到 v0.1.3400）：

```sh
git fetch upstream --tags
git fetch origin
git tag xm/v0.1.3315 origin/local          # 留住旧的 local，回退时能重新构建
git push origin xm/v0.1.3315
git switch -C local origin/local
git rebase --onto v0.1.3400 v0.1.3315 local
```

补丁冲突时：

1. 按上游新代码改我们的提交，保持补丁的行为不变。
2. 跑这个补丁的测试（见下面“构建”里的测试名），通过后 `git rebase --continue`。
3. 根目录 `README.md` 冲突时，保留我们的版本（`git checkout --theirs README.md`，变基时 theirs 指我们的提交）。上游的介绍从链接看。
4. 上游已经有同等修复时，`git rebase --skip` 丢掉这个提交，并在本文“补丁”里删掉它。
4. 改不动又不能丢时，停在当前版本，不升级。

变基后推送：

```sh
git push --force-with-lease origin local
```

提交信息沿用上游的写法：`<子系统>: <一句话描述行为>`，正文写问题和做法。

## 构建

```sh
soundworkbench/build.sh            # 从 local 构建
soundworkbench/build.sh <提交>     # 从指定提交构建
```

脚本做三件事：

1. 用 `git archive` 取出这个提交的源码，工作树里没提交的改动不进镜像。
2. 在 `golang:1.26-alpine` 里跑补丁的测试：`RemoteAllowance|WebFetch|FetchVerification|CodexFetchText|ClaudeImage|ClaudeReplay|ClaudeTUIReplay|SubscriptionToolResultImages|LostMedia|ToolImages|MediaRemoved`（`./internal/provider/`、`./internal/gateway/`、`./internal/claudetui/`）。不过就停，退出码 3。
3. 构建镜像，打两个标签，打印版本。

版本号规则：

| 项 | 写法 | 例 |
|---|---|---|
| 程序版本（`magpie --version`） | `<上游版本>-xm` | `0.1.3315-xm` |
| 镜像标签 | `magpie-local:v<上游版本>-xm` | `magpie-local:v0.1.3315-xm` |
| 带提交号的镜像标签 | `magpie-local:v<上游版本>-xm-<提交号>` | `magpie-local:v0.1.3315-xm-759d876a` |

上游版本取 `git describe --tags --match 'v*'`，即 `local` 下面最近的上游标签。同一个上游版本上改了补丁再构建，`-xm` 标签会指向新镜像；带提交号的标签不变，回退用它。

镜像名可用环境变量 `IMAGE` 改。

## 部署到各节点

每个节点（家里节点、公司节点）各自换版：

1. 在节点上拿到镜像：在节点上跑 `build.sh`，或在构建机上 `docker save` 后到节点 `docker load`。
2. 照原容器的配置（卷、网络、环境变量、重启策略）用新镜像重建容器。旧容器改名为 `<名字>-old-<旧版本>`，停着保留，不删。
3. 验证：
   - `docker exec <名字> /magpie --version` 显示 `-xm` 版本。
   - 容器健康检查为 healthy。
   - 经这个节点的网关，对一个 Claude 模型、一个 Codex 模型各发一次请求，都返回 200。

一个节点验证通过后再换下一个。

## 回退

两种办法，按情况选：

- 旧容器还在：删掉新容器，把 `<名字>-old-<旧版本>` 改回原名，设回重启策略并启动。

  ```sh
  docker rm -f <名字>
  docker rename <名字>-old-<旧版本> <名字>
  docker update --restart unless-stopped <名字>
  docker start <名字>
  ```

- 旧容器已删：照原配置用上一个镜像标签（带提交号的那个）重建容器。镜像也不在了，就从 `xm/<旧上游版本>` 标签重新构建。

数据都在 `/config` 卷里，换镜像不动数据。

## 已知问题

- 完整跑 `./internal/gateway/` 的测试，在 v0.1.3315-xm 上有 3 项不过：`TestClaudeBridgeSafeguardContextUpdates`、`TestClaudeClientCacheAbsentUnchanged`、`TestRefusalAfterShownThinkingGoesToTheNext`。上游 CI 在 v0.1.3315 上全部测试通过；这 3 项是补丁引起还是本地测试环境引起，待确认。`build.sh` 只跑上述补丁的测试。
