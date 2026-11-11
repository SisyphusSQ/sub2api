# 自有版本维护

## 入口与范围

- Fork：https://github.com/SisyphusSQ/sub2api
- 上游：https://github.com/Wei-Shaw/sub2api
- Linear 项目：https://linear.app/sisyphus-sq/project/sub2api-轻量维护-ceaaa16aa281
- 本目录索引：[补丁清单](patches.md)、[版本记录](releases.md)。完整功能需求分别由 TOO-674、TOO-675、TOO-676、TOO-680 承载，本文维护公共操作规则。
- Decisions 补丁接入与异常结算规则：[OpenAI Decisions](../OPENAI_DECISIONS.md)；[开发验证记录](too-675-verification.md)。
- 百炼 TTS 补丁：[接入与计费说明](../BAILIAN_TTS.md)、[TOO-680 验证记录](too-680-verification.md)；sqmc03 部署和镜像回执见需求卡交付评论。
- 生图计费补丁：[TOO-674 开发验证记录](too-674-verification.md)，包含定价优先级、隔离网关对账与证据边界。
- v0.2.21 同步候选：[TOO-703 验证记录](too-703-verification.md)、[上线与回退流程](v0.2.21-deployment.md)。
- 保留上游前端、PostgreSQL、Redis；不开展 MySQL 迁移或前端重写。

## 机器与目录

适用机器：sqmc03 + sqmc05。开发源码为 `/Users/suqing/Coding/golang/00_self/sub2api`。
sqmc03 承载现有服务。sqmc05 的定期同步使用独立控制克隆 `/Users/suqing/Coding/golang/00_self/sub2api-sync` 和版本工作树 `/Users/suqing/Coding/golang/00_self/sub2api-sync-worktrees/<version>`，不修改开发克隆；同步任务已启用不代表服务已部署。
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

## 同步流程

sqmc05 的 Codex 任务 `sub2api-09-00` 已启用，按 Asia/Shanghai 每日 09:00 检查正式 Release；每轮向 Codex 与钉钉分别报告实际结果。2026-10-30 已完成独立工作树和隔离环境演练；Docker/testcontainers 集成测试及真实付费上游调用未在该演练中运行。任务启用和演练不代表需求卡中的所有工作已完成。
拉取并阅读变更/迁移 → 建立或复用候选分支 → 合入正式标签 → 检查每个补丁 → 隔离构建与验证 → 推送候选及报告 → 用户确认后推进 stable → 单独决定部署。
控制克隆或目标候选工作树脏、分支分歧、无法明确处理的冲突或验证阻塞时保留现场，不自动覆盖、stash 或提交人工工作；任务持有共享锁，stable 只跟踪自有远端的已合入提交。相同版本相同失败且无条件变化时不重复执行；当日无更新也报告检查结果。
候选环境使用独立配置、端口、数据库和 Redis，不连接运行数据库。定时任务不进行真实付费上游调用；另行授权的验收应单独记录。报告写明实际验证与未覆盖内容，不能将构建成功当成功能验收。
状态和通知意图保存在 sqmc05 `/Users/suqing/.local/share/sub2api/sync`，不保存凭据；通知回执和同步观察分别记录。

## 发布与回退

部署单独授权，使用固定自有版本镜像并记录源码提交、上游版本和镜像摘要。先分析数据库迁移并备份；不兼容迁移或新增写入不能只靠旧镜像回退。
提交、推送、打标签收尾复用已有测试输出，不重复运行测试；此约定不证明测试已通过。
