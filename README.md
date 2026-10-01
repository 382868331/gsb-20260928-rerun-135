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
- `PreviewGroup` 协调整组彼此引用的配置：每条跨文件引用（`Reference`）在预览中标明迁移前后取值，展示引用如何跟随字段拆分、合并或身份变化；任一文件迁移失败或引用无法对应都会列入 `Blockers`，不会只报第一份失败的文件。
- `ApplyGroup` 只接受无阻碍的预览，并校验整组指纹：确认后任何一份输入被修改都会返回 `ErrStalePreview`，整组确认需重新核对；存在阻碍时返回 `ErrGroupBlocked`，绝不发布半组新配置。
- `RollbackGroup` 整组回退：任一文件缺少反向步骤时整组拒绝并逐文件列出阻碍；回退后引用仍无法对应的列入 `GroupResult.Blockers`，丢失字段列入 `Warnings`。
- `GroupStore` 按代（generation）原子发布整组：先完整写入新代目录，再以单次 manifest 重命名切换当前代，中断后重开只会看到完整旧组或完整新组；历史代目录与原始文件全部保留。
- `GroupStore.Rollback` 可把当前代切回任一历史代，报告哪些文件被恢复、哪些后续代仅归档不再当前（其中独有的修改不可恢复）；历史预览连同其指纹依据始终可通过 `Load`/`History` 查询。

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
