# go-payout-batches

商户应付款（payable）的批次冻结与银行结果确认服务。纯 Go 标准库实现，无外部依赖。

## 能力总览

| 用例 | 入口方法 |
| --- | --- |
| 应付款登记 | `RegisterPayable` |
| 应付款查询 | `GetPayable` |
| 创建批次（原子冻结 + 金额快照） | `CreateBatch` |
| 批次 / 批次明细查询 | `GetBatch` / `ListBatchItems` |
| 提交银行（外部提交号幂等） | `SubmitBatch` |
| 提交前本地取消 | `CancelBatch` |
| 银行回执确认（兼容重复/乱序/迟到） | `HandleBankReceipt` |
| 结算记录 / 通知查询 | `GetSettlement` / `ListNotifications` |

## 核心一致性规则

### 1. 批次创建：只有可结算明细可入批次，冻结必须原子

- 应付款状态机：`settlable → frozen → settled`；失败或取消回到 `settlable`。
- 创建批次时先做**全量校验**（存在性、商户归属、币种一致、状态为 `settlable`、批次内不重复），
  全部通过后才在**同一个事务**内冻结明细并写入金额快照（`BatchItem.AmountSnap`）。
  任一明细不满足条件则整批失败，不会出现部分冻结。
- 已在未结束批次中的明细（`frozen`）不能进入第二个批次。
- 金额快照保证：批次提交/确认时使用的始终是入批时的金额，与明细后续变化无关。

### 2. 提交银行：外部提交号幂等，提交与取消互斥

- 每个外部提交号（`ExternalNo`）记录首次提交的**内容指纹**
  （商户 + 每行明细 ID、币种、最小单位金额）。
  - 同号 + 同批次同内容：幂等返回首次提交结果（`SubmitResult.Replayed = true`）。
  - 同号但批次不同：`ErrIdempotencyConflict`。
  - 同号同批次但金额/明细内容不同：`ErrIdempotencyConflict`。
- 已提交的批次不能换另一个外部号重复提交（`ErrConflict`），也不能再被本地取消。
- 提交与取消在同一把写事务锁上串行执行：并发时**恰好一个**成功
  （测试 `TestSubmitCancelRaceExactlyOneWins` 用 `-race` 验证）。

### 3. 银行回执：按批次 + 提交版本匹配，旧回执不覆盖终态

回执只有同时满足以下条件才会确认成功/失败：

1. 外部提交号存在对应提交记录；
2. 回执批次 == 提交记录绑定的批次；
3. 回执 `SubmitVersion` == 批次当前 `SubmitVersion`。

处理结果三分类：

- `confirmed`：匹配有效，完成状态迁移；
- `duplicate`：终态后再次到达的匹配回执，幂等返回，**不重复**生成结算记录/通知；
- `ignored`：批次不匹配（串单）或版本不匹配（乱序/迟到），直接忽略。

副作用与状态迁移在同一持久化事务内完成：

- **成功**：批次 → `succeeded`，明细 → `settled`，按批次生成**唯一**结算记录
  （`settlements` 以 `batchID` 为键）和一条通知。
- **失败**：批次 → `failed`，同一事务内释放全部明细回 `settlable`，
  不生成结算记录与通知；释放后的明细可以重新组批。
- 终态（`succeeded` / `failed` / `canceled`）不可被任何后到回执或操作覆盖。

## 金额表示

`Money{Currency, Amount}` 中 `Amount` 为最小货币单位的 **int64 整数**
（CNY/USD/EUR 为分，JPY 为元），全程无浮点：

```go
m, _ := ParseMoney("CNY", "12.34") // -> Amount 1234
m2 := MustParseMoney("JPY", "100") // -> Amount 100
```

超出币种精度的尾数（如 CNY `"1.234"`）直接报错，不做静默舍入。

## 错误分类

均为哨兵错误，用 `errors.Is`（或包内便捷函数）判断：

| 错误 | 含义 | 便捷判断 |
| --- | --- | --- |
| `ErrInvalidArgument`（含 `ErrInvalidAmount`） | 入参/金额非法 | `IsInvalidArgument` |
| `ErrNotFound` | 明细/批次/提交记录不存在 | `IsNotFound` |
| `ErrConflict` | 状态冲突（明细已冻结、已提交不可取消等） | `IsConflict` |
| `ErrIdempotencyConflict` | 外部号相同但批次/金额内容不同 | `IsIdempotencyConflict` |
| `ErrTerminalState` | 批次已终态，操作被拒绝 | `IsConflict`（一并识别） |

## 存储与事务

`Store` 为内存事务存储：`Update` 在状态克隆上执行业务逻辑，
成功后整体替换，回调返回 error 时直接丢弃克隆、原状态不变，
因此业务方法中的多步修改具有"同一持久化边界"——要么全部生效，要么全部回滚。
`View` 提供一致性只读快照。

> 该 Store 仅依赖标准库且接口极小，替换为 SQL 实现时，
> 把每个 `Update` 回调映射为一个数据库事务即可（冻结用 `SELECT ... FOR UPDATE`
> 或 `UPDATE ... WHERE status = 'settlable'` 乐观条件）。

## 快速上手

```go
svc := NewPayoutService(NewStore())
ctx := context.Background()

p, _ := svc.RegisterPayable(ctx, RegisterPayableRequest{
    MerchantID: "M-001",
    Amount:     MustParseMoney("CNY", "100.00"),
})

b, _ := svc.CreateBatch(ctx, CreateBatchRequest{
    MerchantID: "M-001",
    PayableIDs: []string{p.ID},
})

r, err := svc.SubmitBatch(ctx, SubmitBatchRequest{
    BatchID: b.ID, ExternalNo: "REQ-20260928-001",
})
// r.Replayed == false, r.Batch.Status == submitted

res, err := svc.HandleBankReceipt(ctx, BankReceipt{
    ExternalNo: "REQ-20260928-001",
    BatchID:    b.ID,
    SubmitVersion: 1, // 必须与提交版本一致
    Success:    true,
})
// res.Outcome == confirmed; 生成唯一 Settlement 与 Notification
```

完整流程示例见 `service_test.go` 中的 `ExamplePayoutService`。

## 测试

```bash
go test -race -count=1 ./...
```

覆盖要点：

- 金额解析/精度/混币种；
- 整批失败不留部分冻结、跨商户/混币种/重复明细校验；
- 外部提交号同内容重放、同号换批次、同号换金额冲突；
- 提交/取消并发恰好一个成功、同一明细并发组批恰好一个成功（`-race`）；
- 成功回执生成唯一结算+通知，失败回执原子释放全部明细；
- 回执重复（不重复结算）、串单（ignored）、版本不匹配（ignored）、
  终态后失败回执不得翻案；
- 失败释放 → 重新组批 → 成功的端到端生命周期。
