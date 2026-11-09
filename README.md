# soundworkbench/magpie

这是 [yetone/magpie](https://github.com/yetone/magpie) 的分叉。我们在上游的发布版上加了两个补丁，自己构建镜像，在自己的节点上运行。

> English: a fork of yetone/magpie. The `local` branch is an upstream release plus two patches. For magpie itself, read the [upstream README](https://github.com/yetone/magpie#readme).

## 和上游的区别

| 补丁 | 作用 |
|---|---|
| 远端账号参与调度 | 一个 magpie 把别的 magpie 当作远端接入时，按远端账号的剩余额度分配请求。上游把远端的额度当作未知。 |
| 读网页的后备 | magpie 自己读不到网页时（下载失败、正文为空、遇到验证页），改用 ChatGPT 后端读。只在设置里选了 ChatGPT 后端搜索时生效。 |

其他行为和上游相同。每个补丁的细节见 [soundworkbench/README.md](soundworkbench/README.md)。

## 分支

- `local`：上游某个发布标签加我们的两个补丁。镜像只从这个分支构建。
- `main`：跟上游，不放我们的改动。

## 构建

```sh
soundworkbench/build.sh            # 从 local 构建
soundworkbench/build.sh <提交号>    # 从指定提交构建
```

脚本先跑两个补丁的测试，不通过就停。镜像名是 `magpie-local:<上游版本>-xm`，另打一个带提交号的标签，用来回退到确切的一版。

## 升级与回退

升级上游版本时，把 `local` 变基到新的发布标签。回退时，换回上一个带提交号的镜像标签。步骤见 [soundworkbench/README.md](soundworkbench/README.md)。

## 上游

magpie 的功能、安装和配置，见上游的 [README](https://github.com/yetone/magpie#readme) 和[文档](https://usemagpie.ai/docs/start)。本仓库沿用上游的 MIT 许可证，见 [LICENSE](LICENSE)。
