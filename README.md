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
- `NewMergeSession` 基于共同来源（base）合成两个团队各自迁移/修改后的候选：通过步骤声明的字段血缘（`FieldEffect.From`）对齐重命名、拆分与合并，迁移后名称不同也只对应一个对象，不会复制两份；`MergeItem` 逐项标明共同来源、两侧路径（`MergeRequest.A.Path`/`B.Path`）、一边新增、一边删除或双方改动同一含义。
- 操作者用 `Resolve` 逐项解决冲突，每次人工选择必须记录理由；未解决项可继续保留在会话里。`Related`/`MergePreview.Refs` 把同一交叉引用关联的对象一起显示，任何引用在合并草稿中无法对应都会进入 `Blockers`，`Commit` 只要存在未解决、已失效或引用不通的项就返回 `ErrMergeBlocked`，绝不发布表面有效但引用错误的正式版本。
- 解决期间某个候选继续变化时调用 `UpdateCandidates`：只有取值或名称依据发生变化的决定被标记为 `stale` 要求重新核对，已完成的不相关选择保持 `resolved`。
- `MergeStore` 原子持久化会话与正式版本：`Save` 为 open/resolve/update/reopen/adopt/undo 每个动作保留当时完整的两份候选与全部决定；`Undo` 回到最近一次人工动作之前但不删除任何历史记录；`Reopen` 可继续修改已采用的会话，原始候选和每次选择理由始终可通过 `Events` 查询。

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

两个团队基于同一份配置分别迁移并修改后，逐项合成正式版本：

```go
session, err := engine.NewMergeSession(ctx, configmigrator.MergeRequest{
    Base: base,
    A: configmigrator.MergeCandidate{Document: teamA, From: "v1", Path: []string{"v1", "v2"}},
    B: configmigrator.MergeCandidate{Document: teamB, From: "v1", Path: []string{"v1", "v2alt"}},
}, refs)
if err != nil { /* 候选必须与其声明路径一致 */ }

for _, item := range session.Preview().Items {
    if item.Status == configmigrator.MergeUnresolved {
        _ = session.Resolve(item.ID, "a", "保留 A 团队取值的理由", nil)
    }
}
result, err := session.Commit(ctx, mergeStore.Publish("merge-1")) // 全部核对通过才采用
```
