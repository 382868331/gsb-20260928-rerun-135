# 应用配置迁移引擎

Go 包 `configmigrator` 用注册式步骤把整份应用配置从一个版本迁移到另一个版本。

## 能力

- 新版本只需要调用 `Engine.Register` 增加迁移步骤，无需逐份修改用户配置。
- 先通过 `Plan` 解析完整路径；缺少步骤返回 `ErrNoMigrationPath`。
- 存在多条路径时返回 `ErrAmbiguousPath`，必须用 `WithPath(v...)` 明确选择分支。
- 每个步骤都在私有副本上运行；字段拆分、默认值填充或冲突检查失败时不会调用发布器。
- 整条路径成功后才调用发布回调；`JSONStore` 使用临时文件加原子重名，失败时保留旧状态。
- `Preview` 在发布前展示实际版本路径、迁移后配置、字段旧值/默认值/拆分规则来源和明确标记的信息丢失。
- `ApplyPreview` 与 `PublishPreview` 使用输入和输出指纹校验；预览后配置或迁移结果变化会返回 `ErrPreviewStale`，不会应用旧计划。
- 每次发布保留迁移前原件、迁移后文档、实际版本路径、字段级结果和逐步记录。
- 当前版本等于目标版本时返回未迁移结果，重复执行不改变内容。
- 正向步骤可选择实现 `Reverse` 或 `ReverseDoc`；无可用反向路径时返回包装了 `ErrReverseUnsupported` 和 `ReverseBlockers` 的具体阻碍，不生成部分恢复配置。
- 存在反向信息丢失时，字段报告会设置 `Loss: true`；存在反向函数不代表总能无损还原。

先预览，确认输入未变化后再发布：

```go
preview, err := engine.Preview(ctx, current, "v4", configmigrator.WithPath("v2", "v3", "v4"))
if err != nil {
    return err
}
result, err := engine.PublishPreview(ctx, current, preview, store.Publish())
```

富报告步骤通过 `MigrateDoc` 返回 `FieldChange`；来源类型包括 `SourceOldValue`、`SourceDefault`、`SourceSplit` 和 `SourceComputed`。无法完整保留的字段必须把对应 `FieldChange.Loss` 设为 `true`。

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
