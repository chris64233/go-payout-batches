package gopayoutbatches

import "time"

// ---------------------------------------------------------------------------
// 状态机
// ---------------------------------------------------------------------------

// ItemStatus 应付款明细状态。
type ItemStatus string

const (
	// ItemSettlable 可结算：登记成功后的初始状态，只有此状态能进入批次。
	ItemSettlable ItemStatus = "SETTLABLE"
	// ItemFrozen 已冻结：已被某个未结束批次占用。
	ItemFrozen ItemStatus = "FROZEN"
	// ItemSettled 已结算：银行确认批次成功。
	ItemSettled ItemStatus = "SETTLED"
)

// BatchStatus 批次状态。
type BatchStatus string

const (
	// BatchCreated 已创建（明细已冻结），尚未提交银行，可取消。
	BatchCreated BatchStatus = "CREATED"
	// BatchSubmitted 已提交银行，等待回执，不可本地取消。
	BatchSubmitted BatchStatus = "SUBMITTED"
	// BatchSucceeded 银行确认成功，终态。
	BatchSucceeded BatchStatus = "SUCCEEDED"
	// BatchFailed 银行确认失败，终态；明细已在同一事务中释放回 SETTLABLE。
	BatchFailed BatchStatus = "FAILED"
	// BatchCancelled 提交前本地取消，终态；明细已释放回 SETTLABLE。
	BatchCancelled BatchStatus = "CANCELLED"
)

// IsTerminal 报告批次是否处于不可再变更的终态。
func (s BatchStatus) IsTerminal() bool {
	return s == BatchSucceeded || s == BatchFailed || s == BatchCancelled
}

// ReceiptResult 银行回执结论。
type ReceiptResult string

const (
	ReceiptSuccess ReceiptResult = "SUCCESS"
	ReceiptFailure ReceiptResult = "FAILURE"
)

// NotificationType 通知类别。
const NotificationTypeSettled = "BATCH_SETTLED"

// ---------------------------------------------------------------------------
// 实体
// ---------------------------------------------------------------------------

// PayableItem 商户的一笔应付款明细。
type PayableItem struct {
	ID         string
	MerchantID string
	// BizNo 商户侧业务单号，用于登记接口的幂等判重。
	BizNo     string
	Payee     string
	Amount    Money
	Status    ItemStatus
	CreatedAt time.Time
}

// BatchItem 是明细进入批次时的冻结记录：金额在此刻快照固定，
// 之后即使明细其他信息变化，批次按快照执行。
type BatchItem struct {
	ItemID string
	// Snapshot 冻结时刻的金额快照。
	Snapshot Money
}

// Submission 记录一次“提交银行”动作及其幂等键。
// 每次（重新）提交 Version 加 1；回执必须同时匹配外部号与版本。
type Submission struct {
	BatchID    string
	MerchantID string
	// ExternalNo 外部提交号（幂等键）。
	ExternalNo string
	// Version 提交版本，从 1 开始。
	Version int
	// ContentHash 提交内容指纹（批次ID + 商户 + 明细ID顺序 + 快照金额）。
	ContentHash string
	SubmittedAt time.Time
}

// Settlement 成功回执生成的唯一结算记录，一个批次至多一条。
type Settlement struct {
	BatchID      string
	MerchantID   string
	Amount       Money
	BankSerialNo string
	SettledAt    time.Time
}

// Notification 成功回执生成的唯一通知，一个批次至多一条。
type Notification struct {
	ID         string
	Type       string
	BatchID    string
	MerchantID string
	Amount     Money
	CreatedAt  time.Time
}

// Batch 应付款批次。返回给调用方的实体均为内部数据的副本。
type Batch struct {
	ID         string
	MerchantID string
	Status     BatchStatus
	// Items 冻结的明细及金额快照，创建后不可变。
	Items []BatchItem
	// TotalAmount 创建时按快照汇总的批次总金额。
	TotalAmount Money

	// 当前提交信息；未提交时为 nil。
	Submission *Submission
	// 成功后的结算记录；仅 SUCCEEDED 时非 nil。
	Settlement *Settlement

	CreatedAt   time.Time
	SubmittedAt time.Time
	FinishedAt  time.Time

	// contentHash 批次内容指纹，提交时用于同号同内容比对。
	contentHash string
}

// BankReceipt 银行异步回执输入。
type BankReceipt struct {
	// ExternalNo 对应提交时使用的外部提交号。
	ExternalNo string
	// Version 回执对应的提交版本（银行侧回传）。
	Version int
	// BatchID 银行回传的批次标识，用于交叉校验。
	BatchID string
	Result  ReceiptResult
	// BankSerialNo 成功时银行流水号，写入结算记录。
	BankSerialNo string
	// Amount 银行回传的成交金额，用于交叉校验。
	Amount Money
}
