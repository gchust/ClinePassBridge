# Fork 维护说明

上游：`xiao-qiu-qiu/ClinePassBridge`。Fork：`gchust/ClinePassBridge`。

## 分支约定

- `main` 是上游 `main` 的镜像，只允许快进同步。禁止删除和强制推送，
  不在这个分支提交 fork 配置或本地功能。
- `custom/main` 是 fork 默认分支，保存本地改动、同步和发布工作流。
- 功能分支使用 `feature/*`，从 `custom/main` 创建，以 PR 合并回去。
  保留镜像 `main`，不要在合并同步 PR 后删除它。

## 初始化设置

以下操作修改 GitHub 线上仓库，应在确认配置后执行。

1. 在 `gchust` 下创建 GitHub fork，核对 parent 是上游仓库。
2. 推送已验证的 `custom/main`，将它设为默认分支。保留上游的 `main`。
3. 在 fork 中禁用上游 `release.yml` 工作流；该文件在镜像分支上保持原样。
   这避免镜像更新触发上游的 Linux/Windows 构建，也避免上游标签触发发布。
   启用新建的三个 fork 工作流。
4. 启用仓库 auto-merge；保留默认工作流权限为 `read`，允许 GitHub Actions
   创建 PR。工作流通过自己的 `permissions` 声明获取写入权限。
5. 使用 `.github/mirror-ruleset.json` 创建 `main` 的防删除、防强推规则，
   不配置组织级 Actions bypass。若账户策略不支持 rulesets，明确报告后
   使用等效的分支保护配置，不忽略保护失败。

主要设置命令：

```bash
gh repo fork xiao-qiu-qiu/ClinePassBridge --clone=false
git push -u origin custom/main
gh api repos/gchust/ClinePassBridge --method PATCH \
  -f default_branch=custom/main -F allow_auto_merge=true \
  -F delete_branch_on_merge=false
gh workflow disable release.yml --repo gchust/ClinePassBridge
gh workflow enable macos-ci.yml --repo gchust/ClinePassBridge
gh workflow enable upstream-release-sync.yml --repo gchust/ClinePassBridge
gh workflow enable release-on-custom-merge.yml --repo gchust/ClinePassBridge
gh api repos/gchust/ClinePassBridge/actions/permissions/workflow --method PUT \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=true
gh api repos/gchust/ClinePassBridge/rulesets --method POST \
  --input .github/mirror-ruleset.json
```

首次运行同步工作流，并通过一个实际维护 PR 的合并验证发布。若 `main`
已经包含在 `custom/main` 中，同步成功但不会制造一个无差异的 PR。

## 上游同步

`upstream-release-sync.yml` 每六小时运行一次（UTC 00:17、06:17、12:17、
18:17），也可手动运行。它读取上游最新正式 release，快进同步 `main`，
镜像对应标签，然后创建或更新 `main` → `custom/main` PR。

上游标签只标记发布基线，标签可能早于上游 `main` 当前 HEAD；镜像始终跟随
`main`，不会将镜像回退到发布标签。分支分叉或标签冲突时停止同步，不强制覆盖。

仓库允许时，工作流请求 merge commit 方式自动合并；无保护条件时可能立即
合并。有冲突、权限不足或策略限制时，保留 PR 并在日志中告警。若要求检查或
人工审核，应先满足对应规则；不要跳过保护。

默认使用 `GITHUB_TOKEN`。这个 token 创建的普通事件不会自动启动后续
工作流，因此同步脚本在确认合并后显式 dispatch 发布工作流；下一次同步
也会重试最近一次已合并同步 PR 的 dispatch，重复执行由发布脚本去重。

如果以后给 `custom/main` 增加必需 CI 检查，可以配置可选的
`UPSTREAM_SYNC_TOKEN`（限定到本 fork 的 contents、pull requests 和 actions
写权限），让机器人创建的 PR 正常触发 CI；否则需要手动触发检查或处理 PR。
无需为了初始设置复制本机 token 到 Actions secrets。

## Fork 发布

`release-on-custom-merge.yml` 处理合并到 `custom/main` 的 PR，包括功能 PR
和上游同步 PR。手动重试需要输入已经合并的 PR 编号，不接受任意提交。

发布脚本核验 PR 的仓库、目标分支和合并状态，取该 PR 的精确合并提交，
选择该提交已经包含的最近上游正式发布标签，生成 `vX.Y.Z-fork.N`。
上游出现但尚未合入的 release 不会被误用为当前版本基线。

同一合并提交只分配一个 fork 标签。标签通过 GitHub refs API 原子创建，
不同 PR 并发发布时不争用同一个版本号；每个 PR 的重试串行执行，不用全局
并发组丢弃排队的其他发布。构建失败后保留已分配标签，重试继续使用同一标签。
正式发布存在时跳过，草稿发布可继续上传。GitHub 按版本和创建日期自动维护 latest。

版本注入仅发生在构建工作区：同步修改 `internal/bridge/types.go` 的
`Version` 和打包用 registry，不回写到长期分支，减少后续上游合并冲突。
Release 说明同时记录上游标签、fork 标签、精确源提交和 PR。

## 平台与安装包

上游没有 Android 工程、APK/AAB 或 Android 构建工作流，因此默认仅构建：

- macOS amd64：`macos-15-intel` 原生构建。
- macOS arm64：`macos-15` 原生构建。

不发布 Linux、Windows 或 Android 资产。打包沿用上游约定：

```text
clinepassbridge_0.1.6-fork.1_darwin_amd64.zip
clinepassbridge_0.1.6-fork.1_darwin_arm64.zip
checksums.txt
registry.json
```

ZIP 根目录仅含 `clinepassbridge.dylib`；`checksums.txt` 包含两个 ZIP 的
SHA-256。上游没有签名配置，本 fork 同样不要求签名 secrets，库未经公证。

额外提供的 `registry.json` 保留原插件 ID、作者、schema 和安装类型，
只将版本和下载仓库指向 fork。使用此 fork 时，CPA 自定义市场源应使用：

```text
https://github.com/gchust/ClinePassBridge/releases/latest/download/registry.json
```

首次发布前这个地址不可用。原始 `marketplace/registry.json` 保留上游
内容用于同步，不作为 fork 的安装入口；CPA 官方插件市场仍安装上游版本。
相同插件 ID 不支持同时加载上游版和 fork 版。

## 验证与排错

```bash
python3 -m unittest discover -s .github/scripts -p 'test_*.py' -v
python3 .github/scripts/fork_release.py check
bash -n .github/scripts/build-macos.sh .github/scripts/upstream-sync.sh
git diff --check

gh workflow run upstream-release-sync.yml --repo gchust/ClinePassBridge
gh run list --repo gchust/ClinePassBridge
gh pr list --repo gchust/ClinePassBridge --base custom/main
gh release list --repo gchust/ClinePassBridge
```

远程验收检查默认分支、镜像规则、sync 日志、PR 合并后的 release、两个
ZIP 的架构/根目录、checksums 和 registry 版本。未在 GitHub 实际执行的
工作流不视为已经通过远程验收。
