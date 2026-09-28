# go-payout-batches

商户应付款的**批次冻结**与**银行结果确认**领域服务（Go，零三方依赖，线程安全）。

本包在内存存储上实现了完整的状态机与并发/幂等保证；存储的所有读改写都在同一把
互斥锁保护的临界区（`Store.mutate`）内完成，该临界区即“同一持久化边界”。
替换为数据库实现时，只需把每个 `mutate` 回调整体放进一个数据库事务。

## 快速开始

```go
svc := gopayoutbatches.NewService()

// 1. 登记应付款（(商户ID, 业务单号) 幂等），金额精确到分
a, _ := svc.RegisterItem(gopayoutbatches.RegisterItemInput{
    MerchantID: "M1", BizNo: "INV-1", Payee: "供应商甲",
    Amount: gopayoutbatches.MustNewMoney(12034, "CNY"), // 120.34 CNY
})

// 2. 创建批次：整批校验、冻结明细、固化金额快照（任一明细不合格则整批失败）
batch, _ := svc.CreateBatch(gopayoutbatches.CreateBatchInput{
    MerchantID: "M1", ItemIDs: []string{a.ID},
})

// 3. 提交银行：外部提交号是全局幂等键
svc.SubmitBatch(gopayoutbatches.SubmitBatchInput{
    MerchantID: "M1", BatchID: batch.ID, ExternalNo: "SUB-20260928-0001",
})

// 4. 银行异步回执（可能重复、乱序、迟到）
out, err := svc.ApplyReceipt("M1", gopayoutbatches.BankReceipt{
    ExternalNo:   "SUB-20260928-0001",
    Version:      1,
    BatchID:      batch.ID,
    Result:       gopayoutbatches.ReceiptSuccess, // 或 ReceiptFailure
    BankSerialNo: "BK-9988",
    Amount:       batch.TotalAmount,
})
```

金额也可从十进制字符串解析：`ParseMoney("120.34 CNY", "")`（小数最多两位，
超出最小货币单位精度直接报错，全程整数运算，无浮点误差）。

## 领域模型与状态机

```
PayableItem:  SETTLABLE ──创建批次──▶ FROZEN ──成功回执──▶ SETTLED
                  ▲                    │
                  └──── 取消 / 失败回执 ┘（同一持久化边界释放全部明细）

Batch:        CREATED ──提交──▶ SUBMITTED ──成功回执──▶ SUCCEEDED  (终态)
                 │                  │
                 └──取消──▶ CANCELLED (终态)
                                    └──失败回执──▶ FAILED     (终态)
```

- **只有 `SETTLABLE` 明细能进入批次**。明细一旦进入未结束批次即变为 `FROZEN`，
  同一明细不可能同时出现在两个未结束批次中。
- 批次创建时把每条明细的金额**快照**进批次（`BatchItem.Snapshot`）并汇总总额；
  批次内容此后不可变，提交/确认一律以快照为准。
- `CANCELLED` / `SUCCEEDED` / `FAILED` 是终态，终态不可被任何操作或回执覆盖。

## 接口（`Service`）

| 方法 | 说明 |
| --- | --- |
| `RegisterItem` | 登记应付款；`(MerchantID, BizNo)` 幂等，同号不同内容报 `ErrDuplicateRequest` |
| `CreateBatch` | 创建批次并冻结；任一明细不合格则整批失败，无部分冻结 |
| `SubmitBatch` | 提交银行，外部提交号幂等，返回 `SubmitResult`（`IdempotentReplay` 标记重放） |
| `CancelBatch` | 提交前取消并释放全部明细；已提交/终态批次不可取消 |
| `ApplyReceipt` | 处理银行回执；成功生成唯一结算记录+通知，失败原子释放全部明细 |
| `GetBatch` / `ListBatches` | 批次及冻结明细快照查询 |
| `ListItems` / `GetSettlement` / `GetNotification` | 明细、结算记录、通知查询 |

## 关键正确性保证

1. **整批冻结，失败无残留**：创建批次先在锁内做完全部校验（存在性、商户归属、
   `SETTLABLE` 状态、币种一致），全部通过后才写入批次并置 `FROZEN`。
   校验与冻结在同一临界区，不存在“通过校验却被并发抢先冻结”的窗口。
2. **提交幂等与冲突**：外部提交号一旦使用即与唯一批次绑定。
   - 同号 + 同批次 + 同内容（明细集合与快照金额的 SHA-256 指纹一致）→
     返回首次提交结果，不产生新提交；
   - 同号换批次，或同号但批次内容/金额不同 → `ErrConflict`。
3. **提交与取消互斥**：二者走同一把锁且都做状态前置检查，并发时恰好一个成功；
   已经提交的批次不能再被本地取消（`ErrAlreadySubmitted`）。
4. **回执必须匹配当前批次与提交版本**：
   - 按 `(外部号, 版本)` 定位提交，再交叉校验批次 ID 与金额；
   - 陌生外部号 / 未知版本 → `ErrUnknownSubmission`；
   - 属于历史版本的旧回执 → `ErrStaleReceipt`；
   - 批次或金额不符 → `ErrReceiptMismatch`。
5. **终态保护与唯一产物**：成功/失败终态不可被旧回执或反向回执覆盖
   （`ErrTerminalState`）；同向重复回执幂等放行，但结算记录与通知**只生成一次**
   （按批次 ID 唯一约束 + 终态短路）。
6. **失败原子释放**：失败回执在同一持久化边界内把批次置 `FAILED` 并把
   **全部**明细释放回 `SETTLABLE`，之后这些明细可重新组批。

## 错误类型

所有领域错误都是哨兵错误，用 `errors.Is(err, gopayoutbatches.ErrXxx)` 判断：

| 错误 | 触发场景 |
| --- | --- |
| `ErrInvalidArgument` | 入参缺失、金额非正、明细重复、混合币种等 |
| `ErrNotFound` | 明细 / 批次 / 结算记录不存在 |
| `ErrForbidden` | 资源属于其他商户 |
| `ErrItemNotSettlable` | 明细不是 `SETTLABLE`，不能入批 |
| `ErrDuplicateRequest` | 幂等键重复但请求内容不同 |
| `ErrConflict` | 同号换批次/换内容、状态不允许的操作 |
| `ErrAlreadySubmitted` | 批次已提交，不能本地取消，或换号再次提交 |
| `ErrBatchClosed` | 批次已处于终态 |
| `ErrUnknownSubmission` | 回执引用了不存在的外部号或提交版本 |
| `ErrStaleReceipt` | 回执属于旧提交版本 |
| `ErrReceiptMismatch` | 回执批次 ID 或金额与当前提交不一致 |
| `ErrTerminalState` | 回执/操作试图覆盖成功或失败终态 |

## 金额表示

`Money{Amount int64, Currency string}`：`Amount` 是最小货币单位（如人民币“分”），
币种为 ISO 4217 三字母代码。`Add` 校验币种一致与 int64 溢出；`ParseMoney`
支持 `"12.34 CNY"` / `"12.34CNY"` / `"12.34"` 等形式，超过两位小数直接报错。

## 测试

```
go test -race -count=1 ./...
```

覆盖：金额解析/溢出/币种、登记幂等、整批失败无部分冻结、重复冻结拒绝、
提交幂等/冲突、提交与取消 200 轮并发竞争（恰一个成功）、回执重复/乱序/迟到/
旧版本/未来版本/批次与金额不符、失败原子释放、成功唯一结算记录与通知、
终态不可覆盖、跨商户隔离与查询。

## 文件结构

| 文件 | 内容 |
| --- | --- |
| `money.go` | 精确金额 `Money`：构造、解析、加法、格式化 |
| `errors.go` | 哨兵错误定义 |
| `models.go` | 状态枚举与领域实体（明细/批次/提交/结算/通知/回执） |
| `store.go` | 线程安全的内存存储、事务边界、索引与深拷贝 |
| `service.go` | 应用服务：登记、建批、提交、取消、回执、查询 |
| `*_test.go` | 自动化测试（含竞态检测） |
