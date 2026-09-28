package gopayoutbatches

import "time"

// PayableStatus 应付款明细状态。
type PayableStatus string

const (
	// PayableSettlable 可结算：只有该状态的明细才能进入批次。
	PayableSettlable PayableStatus = "settlable"
	// PayableFrozen 已被某个未结束批次冻结。
	PayableFrozen PayableStatus = "frozen"
	// PayableSettled 已随成功批次完成结算。
	PayableSettled PayableStatus = "settled"
)

// Payable 商户应付款明细。
type Payable struct {
	ID         string
	MerchantID string
	// Amount 登记时金额；进入批次时会再次快照到 BatchItem 上。
	Amount    Money
	CreatedAt time.Time
	Status    PayableStatus
	// BatchID 当状态为 frozen/settled 时，记录所属批次。
	BatchID string
	Version int64 // 乐观版本号，每次状态变更递增
}

// BatchStatus 批次状态。
type BatchStatus string

const (
	// BatchOpen 已创建并冻结明细，尚未提交银行。
	BatchOpen BatchStatus = "open"
	// BatchSubmitted 已提交银行，等待回执。
	BatchSubmitted BatchStatus = "submitted"
	// BatchSucceeded 银行确认成功，终态。
	BatchSucceeded BatchStatus = "succeeded"
	// BatchFailed 银行确认失败，明细已释放，终态。
	BatchFailed BatchStatus = "failed"
	// BatchCanceled 提交前被本地取消，明细已释放，终态。
	BatchCanceled BatchStatus = "canceled"
)

// IsTerminal 判断批次状态是否为终态。
func (s BatchStatus) IsTerminal() bool {
	return s == BatchSucceeded || s == BatchFailed || s == BatchCanceled
}

// BatchItem 批次内的明细行，保存金额快照，此后商户明细金额变化不影响批次。
type BatchItem struct {
	PayableID  string
	MerchantID string
	AmountSnap Money
	Seq        int // 行序号，决定批次内容指纹的稳定顺序
}

// Batch 应付款批次。
type Batch struct {
	ID          string
	MerchantID  string
	Currency    string
	Status      BatchStatus
	Items       []BatchItem
	TotalAmount Money
	CreatedAt   time.Time
	// SubmitVersion 每次提交递增；银行回执必须匹配当前提交版本方有效。
	SubmitVersion int64
	// SubmittedAt / ExternalNo 最近一次提交信息。
	SubmittedAt time.Time
	ExternalNo  string
	// 终态时间。
	FinishedAt time.Time
	// BankResultCode/Message 回执附带的银行侧信息。
	BankResultCode    string
	BankResultMessage string
}

// FrozenPayableIDs 返回批次冻结的全部应付款 ID（按行顺序）。
func (b *Batch) FrozenPayableIDs() []string {
	ids := make([]string, 0, len(b.Items))
	for _, it := range b.Items {
		ids = append(ids, it.PayableID)
	}
	return ids
}

// Submission 外部提交号的幂等记录。
type Submission struct {
	ExternalNo string
	// ContentFingerprint 提交内容指纹（批次 + 明细 + 金额快照）。
	ContentFingerprint string
	BatchID            string
	SubmitVersion      int64
	FirstSubmittedAt   time.Time
}

// Settlement 成功后生成的唯一结算记录。
type Settlement struct {
	ID          string
	BatchID     string
	ExternalNo  string
	MerchantID  string
	Currency    string
	TotalAmount Money
	SettledAt   time.Time
}

// Notification 成功后生成的唯一通知。
type Notification struct {
	ID           string
	SettlementID string
	BatchID      string
	MerchantID   string
	Message      string
	CreatedAt    time.Time
}
