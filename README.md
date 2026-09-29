# 应用配置迁移引擎

Go 包 `configmigrator` 用有向迁移步骤把旧版本应用配置迁移到目标版本。

## 核心能力

- 新增配置版本时通过 `Register` 或 `New(...)` 注册迁移步骤，无需逐份手改配置。
- 迁移前先用 `Plan` 检查完整路径；缺步骤、循环不可达或不支持反向迁移都会直接报错。
- 存在多条路径时必须通过 `PlanOptions.Route` 明确选择中间版本。
- 所有步骤在内存副本上执行；任一步失败或版本核对失败，原配置不变。
- 已经处于目标版本时直接返回，步骤不执行，文件内容不改变。
- `FileStore` 对 JSON 文件执行暂存写入和原子替换，并保留原文件备份与迁移记录。

## 内存配置示例

```go
m, err := configmigrator.New(
    configmigrator.Step{
        FromVersion: "v1",
        ToVersion:   "v2",
        Migrate: func(c configmigrator.Config) error {
            c["version"] = "v2"
            c["retries"] = 3 // 默认值
            return nil
        },
    },
    configmigrator.Step{
        FromVersion: "v2",
        ToVersion:   "v3",
        Migrate: func(c configmigrator.Config) error {
            c["first_name"] = c["name"] // 字段拆分示例
            delete(c, "name")
            c["version"] = "v3"
            return nil
        },
    },
)
if err != nil {
    return err
}

cfg := configmigrator.Config{"version": "v1", "name": "Ada"}
result, err := m.Migrate(cfg, "v3", nil)
```

## 分支选择

当 `v1 -> v2 -> v3` 和 `v1 -> v2x -> v3` 同时存在时，直接迁移会返回 `ambiguous` 错误：

```go
result, err := m.Migrate(cfg, "v3", &configmigrator.PlanOptions{
    Route: []string{"v2"},
})
```

也可以先调用 `Routes("v1", "v3")` 展示可选路径，再让调用方明确选择。

## JSON 文件迁移

```go
store, err := configmigrator.NewFileStore(m)
if err != nil {
    return err
}

result, err := store.MigrateFile(
    "config/app.json",
    "v3",
    "backups",
    "migration-records",
    &configmigrator.PlanOptions{Route: []string{"v2"}},
)
```

成功后原配置会复制到 `backups`，迁移步骤记录会写入 `migration-records`。同名历史文件不会覆盖；失败时不发布新配置，也不会留下已发布的备份或记录。

## 版本字段约定

默认从配置的 `version` 字段读取字符串版本。如果应用使用其他字段，可在迁移前调用：

```go
err = m.SetVersionKey("schema_version")
```

每个迁移步骤负责修改字段并在结束时把版本字段设置为自己的 `ToVersion`。引擎会在每一步后核对版本，最终成功后才提交整份配置。
