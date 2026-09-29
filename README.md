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
- 正向步骤可选择实现 `Reverse`；无可用反向路径时返回包装了 `ErrReverseUnsupported` 的错误。

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
