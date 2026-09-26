# 参与维护

本 fork 的默认分支为 `custom/main`；`main` 专门镜像上游，请勿提交本地改动。

```bash
git fetch origin
git switch custom/main
git pull --ff-only origin custom/main
git switch -c feature/your-change
```

完成改动后测试并提交，commit message 使用英文，向 `custom/main` 创建 PR。
每个合并的 PR 会自动构建 macOS amd64/arm64 动态库并发布 fork 版本。

```bash
go test ./...
python3 -m unittest discover -s .github/scripts -p 'test_*.py' -v
git diff --check
```

不要手动修改上游版本号来生成 fork release，也不要推送原始上游标签触发
fork 发布；版本由合并后的发布脚本分配。同步 PR 必须保留 `main` 分支。

分支保护、同步权限、构建平台及安装入口详见
[Fork 维护说明](docs/fork-maintenance.md)。
