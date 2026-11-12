#!/bin/bash
# 从 local 分支构建镜像 magpie-local:<上游版本>-xm。
# 用法：soundworkbench/build.sh [提交或分支，默认 local]
# 先跑补丁的测试，不过就停，不构建。
# 镜像另打一个带提交号的标签 <上游版本>-xm-<提交号>，回退时用它找回确切的一版。
# 源码经 git archive 从标准输入送进 docker，不读工作树里未提交的改动。
set -u
REF=${1:-local}
IMAGE=${IMAGE:-magpie-local}
cd "$(git -C "$(dirname "$0")" rev-parse --show-toplevel)" || exit 1
SHA=$(git rev-parse --short "$REF^{commit}") || exit 1
TAG=$(git describe --tags --match 'v*' --abbrev=0 "$REF") || { echo "no upstream tag below $REF: fetch the upstream tags first"; exit 1; }
git archive "$REF" | docker run --rm -i -v magpie-gocache:/go -e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 golang:1.26-alpine \
  sh -c 'mkdir /src && tar -x -C /src && cd /src && go test -tags nogui -count=1 -run "RemoteAllowance|WebFetch|FetchVerification|CodexFetchText|ClaudeImage|ClaudeReplay|ClaudeTUIReplay|SubscriptionToolResultImages|LostMedia|ToolImages|MediaRemoved" ./internal/provider/ ./internal/gateway/ ./internal/claudetui/' || exit 3
git archive "$REF" | docker build -q --build-arg VERSION="${TAG#v}-xm" -t "$IMAGE:$TAG-xm" -t "$IMAGE:$TAG-xm-$SHA" - || exit 1
docker run --rm --entrypoint /magpie "$IMAGE:$TAG-xm" --version | head -1
echo "$IMAGE:$TAG-xm $IMAGE:$TAG-xm-$SHA"
