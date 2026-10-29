# 自有版本维护

## 入口与范围

- Fork：https://github.com/SisyphusSQ/sub2api
- 上游：https://github.com/Wei-Shaw/sub2api
- Linear 项目：https://linear.app/sisyphus-sq/project/sub2api-轻量维护-ceaaa16aa281
- 本目录索引：[补丁清单](patches.md)、[版本记录](releases.md)。完整功能需求分别由 TOO-674、TOO-675、TOO-676 承载，本文维护公共操作规则。
- 保留上游前端、PostgreSQL、Redis；不开展 MySQL 迁移或前端重写。

## 机器与目录

适用机器：sqmc03 + sqmc05。源码统一为 `/Users/suqing/Coding/golang/00_self/sub2api`。
sqmc03 承载现有服务；sqmc05 用于后续 Codex 定期同步，克隆源码不表示服务或定时任务已部署。
仅 sqmc03：部署配置 `/Users/suqing/.config/sub2api/deploy.env`，权限 600；密钥目录 `/Users/suqing/.config/sub2api`；备份 `/Users/suqing/.local/share/sub2api/backups`。配置和密钥不得提交或复制到 sqmc05。
业务数据保留 Apple Container 命名卷 sub2api-apple-data、sub2api-apple-postgres-data、sub2api-apple-redis-data。

现有上游部署脚本必须显式传入外置配置，例如：
```sh
SUB2API_ENV_FILE=/Users/suqing/.config/sub2api/deploy.env /Users/suqing/Coding/golang/00_self/sub2api/deploy/apple-container.sh status
```
此例为人工运维入口；不要在同步任务中执行 up/restart/down/init。旧源码目录 `/Users/suqing/app/sub2api` 已迁移，不再作为入口。

## Git 约定

- upstream：官方只读远端；origin：自有 fork（SSH）。
- main：只快进跟踪 upstream/main，不放本地修改。
- suqing/stable：以正式标签为基线，合入已验收补丁；不 rebase、不强推。
- suqing/fix/<topic>：单项补丁，从 stable 创建。
- suqing/sync/<version>：正式版候选，从 stable 创建并 merge 上游标签。
- 向上游贡献时从 upstream/main 建独立分支，仅移入对应通用修复；提 PR 另行授权。
- 一项补丁解决一件事，不混入无关格式化。更新补丁清单，记录移除条件；上游等价修复通过相关验证后再去重。

## 同步流程（待 TOO-676 实施）

由 sqmc05 Codex 每日检查正式 Release，执行时刻待确认。先人工演练，再启用任务。
拉取并阅读变更/迁移 → 建立或复用候选分支 → 合入正式标签 → 检查每个补丁 → 隔离构建与验证 → 推送候选及报告 → 用户确认后推进 stable → 单独决定部署。
工作区脏、分支分歧、冲突或验证失败时停止，不自动覆盖、stash 或提交人工工作；单任务互斥。相同版本相同失败且无条件变化时不重复执行。无更新保持安静，有候选结果、新阻塞或待决策事项才通知。
候选环境使用独立配置、端口、数据库和容器卷，不连接运行数据库。真实上游调用须先明确预算。报告写明实际验证与未覆盖内容，不能将构建成功当成功能验收。
状态和报告拟保存在 sqmc05 `/Users/suqing/.local/share/sub2api/sync`，不保存凭据。

## 发布与回退

部署单独授权，使用固定自有版本镜像并记录源码提交、上游版本和镜像摘要。先分析数据库迁移并备份；不兼容迁移或新增写入不能只靠旧镜像回退。
提交、推送、打标签收尾复用已有测试输出，不重复运行测试；此约定不证明测试已通过。
