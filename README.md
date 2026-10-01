# 应用配置迁移引擎

Go 包 `configmigrator` 用注册式步骤把整份应用配置从一个版本迁移到另一个版本。

## 能力

- 新版本只需要调用 `Engine.Register` 增加迁移步骤，无需逐份修改用户配置。
- 先通过 `Plan` 解析完整路径；缺少步骤返回 `ErrNoMigrationPath`。
- 存在多条路径时返回 `ErrAmbiguousPath`，必须用 `WithPath(v...)` 明确选择分支。
- 每个步骤都在私有副本上运行；字段拆分、默认值填充或冲突检查失败时不会调用发布器。
- 整条路径成功后才调用发布回调；`JSONStore` 使用临时文件加原子重名，失败时保留旧状态。
- 每次发布保留迁移前原件、迁移后文档和逐步记录。
- 当前版本等于目标版本时返回未迁移结果，重复执行不改变内容。
- `Preview` 在迁移前给出影响预览：每个字段标明来源（旧值保留、默认值、拆分规则或未声明的派生），被删除或覆盖的字段作为信息丢失明确列入 `Losses`。
- 预览携带输入指纹；`Apply` 在校验输入未变化后才执行，预览后配置被修改会返回 `ErrStalePreview`，不会套用旧计划。
- 正向步骤可选择实现 `Reverse`；无可用反向路径时返回包装了 `ErrReverseUnsupported` 的错误，并用 `RollbackBlockers` 列出具体阻碍（哪个步骤缺少反向实现）。回退不保证无损：无法还原的字段会列入 `Result.Warnings`，不会生成看似已恢复的残缺配置。
- `PreviewGroup` 一次协调整组彼此引用的配置：`GroupSpec.References` 声明跨文件引用，预览展示引用如何跟随字段拆分、合并或身份变化（`ReferenceFollowed` / `ReferenceUnchanged` / `ReferenceBroken`）。
- 单份配置可以迁移不代表整组有效：无法对应到任何成员的引用会列入 `GroupPreview.Blockers`，`ApplyGroup` 在阻碍存在时返回 `ErrGroupBlocked`，不会发布半组新配置。
- 组预览携带整组指纹；确认后任一成员被修改，`ApplyGroup` 返回 `ErrStalePreview`，必须重新核对整组。
- `GroupStore` 把整组状态、发布历史（含迁移前原件）和已确认预览原子写入单个状态文件：保存或发布中断后重开，只会看到完整旧组或完整新组，历史预览仍可查询当时的依据。
- `GroupStore.Rollback(releaseID)` 按历史版本回退整组：`GroupRollback.Restored` 列出恢复的成员，`Unrecoverable` 列出快照中没有、仅留存于发布原件中的信息；回退不删除任何历史版本与原文件。

## 示例

```go
engine := configmigrator.NewEngine()
engine.MustRegister(configmigrator.Step{
    From: "v1",
    To:   "v2",
    Migrate: func(ctx context.Context, doc configmigrator.Document) (configmigrator.Document, error) {
        next := doc.Clone()
        next["version"] = "v2"
        next["timeout_seconds"] = 30
        return next, nil
    },
})

store := configmigrator.NewJSONStore("/var/lib/app/config-state.json")
result, err := engine.Publish(ctx, current, "v2", store.Publish())
```

分支必须显式指定：

```go
result, err := engine.Publish(ctx, current, "v4", store.Publish(),
    configmigrator.WithPath("v2", "v3", "v4"),
)
```
