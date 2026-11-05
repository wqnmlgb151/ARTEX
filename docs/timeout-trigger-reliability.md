# 超时触发器投递

任务从非 timeout 状态转换为 timeout 时，PostgreSQL 行触发器在状态更新事务内插入
`task_timeout_events`。重复写 timeout 不生成事件；重新运行后再次超时生成新事件。
升级仅安装触发器，不回填历史任务，也不改动其他触发条件的调度状态。

调度器用 `FOR UPDATE SKIP LOCKED` 领取未处理事件，在同一事务内创建
`timeout_trigger_deliveries` 并确认事件的 fanout。事件 ID 仅用于排序，不作为水位线。
没有启用的超时触发器时，事件被消费而不补发；与原有调度行为一致。

现有队列仍负责每个 Agent 的串行/并行上限及合并策略。出队时把合并后的运行、会话、
用户消息及投递关联一起提交。运行租约为 2 分钟，每 30 秒续期；崩溃后租约到期可恢复。
恢复使用原会话，不重复创建会话或用户消息。失败以 5 秒 × 尝试次数退避，上限 5 分钟。
`timeout_trigger_runs.attempts / last_error / lease_until / completed_at` 可查询投递状态；
领取、续期、执行和确认失败有服务端日志。数据库错误不会确认事件或运行成功。

这是可恢复的 At-Least-Once 执行，不是外部工具副作用的 Exactly-Once：若外部操作成功后、
运行完成确认前崩溃，恢复可能再次执行该操作。工具调用应采用业务幂等键。其他类型触发器
保留原实现。Agent 并发上限仍是单服务实例范围；同库多实例部署需要额外的全局运行配额。

集成验证需要独立 PostgreSQL 测试库：

```sh
ARTEX_PG_DSN='postgres://.../artex_test?sslmode=disable' go test ./db ./server -run 'Test(Timeout|Merge)' -count=1 -v
```

覆盖任务 ID 乱序、事件提交乱序、并发终态竞争、重复/再次超时、事务回滚、历史库重复迁移、
数据库临时故障、投递重启恢复、租约所有者隔离及合并元数据保留。
