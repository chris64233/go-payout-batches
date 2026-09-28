package gopayoutbatches

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PayoutService 商户应付款批次冻结与银行结果确认服务。
//
// 所有写操作都在单个持久化事务内完成：
//   - 创建批次：校验全部明细后一次性冻结，任一不满足条件则整批回滚；
//   - 提交/取消：互斥串行，提交成功后本地取消必然失败；
//   - 银行回执：成功时生成唯一结算记录与通知，失败时在同一事务内释放全部明细；
//   - 重复 / 乱序 / 迟到的旧回执不会覆盖终态。
type PayoutService struct {
	store *Store
	now   func() time.Time
}

// NewPayoutService 创建服务实例。
func NewPayoutService(store *Store) *PayoutService {
	return &PayoutService{store: store, now: func() time.Time { return time.Now().UTC() }}
}

// ---- DTO ----

// RegisterPayableRequest 应付款登记请求。
type RegisterPayableRequest struct {
	MerchantID string
	Amount     Money
}

// CreateBatchRequest 创建批次请求。PayableIDs 顺序即批次行顺序。
type CreateBatchRequest struct {
	MerchantID string
	PayableIDs []string
}

// SubmitBatchRequest 批次提交银行请求。
type SubmitBatchRequest struct {
	BatchID    string
	ExternalNo string // 银行侧外部提交号，用于幂等
}

// CancelBatchRequest 批次取消请求。
type CancelBatchRequest struct {
	BatchID string
}

// BankReceipt 银行回执。
type BankReceipt struct {
	ExternalNo    string // 对应提交时使用的外部提交号
	BatchID       string // 回执声称所属批次
	SubmitVersion int64  // 回执对应的提交版本
	Success       bool
	ResultCode    string
	ResultMessage string
	ReceivedAt    time.Time // 可选，留空取接收时刻
}

// ReceiptOutcome 回执处理结果分类。
type ReceiptOutcome string

const (
	// ReceiptConfirmed 回执与当前批次、当前提交版本匹配，完成了成功/失败确认。
	ReceiptConfirmed ReceiptOutcome = "confirmed"
	// ReceiptDuplicate 匹配的重复回执，终态早已确定，本次幂等返回。
	ReceiptDuplicate ReceiptOutcome = "duplicate"
	// ReceiptIgnored 旧版本 / 批次不匹配的迟到或乱序回执，已忽略，不影响终态。
	ReceiptIgnored ReceiptOutcome = "ignored"
)

// ReceiptResult 银行回执处理结果。
type ReceiptResult struct {
	Outcome    ReceiptOutcome
	BatchID    string
	Status     BatchStatus
	Settlement *Settlement // 成功确认时（含重复成功回执）返回
}

// SubmitResult 提交结果。Replayed == true 表示同号同内容的重复提交，
// 返回首次提交的批次与状态。
type SubmitResult struct {
	Replayed bool
	Batch    Batch
}

// ---- 用例实现 ----

// RegisterPayable 登记一笔商户应付款，初始状态为可结算。
func (svc *PayoutService) RegisterPayable(ctx context.Context, req RegisterPayableRequest) (*Payable, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.MerchantID) == "" {
		return nil, fmt.Errorf("%w: merchantID 不能为空", ErrInvalidArgument)
	}
	if err := req.Amount.Validate(); err != nil {
		return nil, err
	}
	if req.Amount.IsZero() {
		return nil, fmt.Errorf("%w: 应付款金额必须大于零", ErrInvalidArgument)
	}

	var out *Payable
	err := svc.store.Update(func(tx *Tx) error {
		now := svc.now()
		p := &Payable{
			ID:         tx.nextID("pay"),
			MerchantID: strings.TrimSpace(req.MerchantID),
			Amount:     req.Amount,
			Status:     PayableSettlable,
			CreatedAt:  now,
			Version:    1,
		}
		tx.putPayable(p)
		out = clonePayable(p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetPayable 查询单笔应付款明细。
func (svc *PayoutService) GetPayable(ctx context.Context, id string) (*Payable, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out *Payable
	err := svc.store.View(func(tx *Tx) error {
		p, ok := tx.getPayable(id)
		if !ok {
			return fmt.Errorf("%w: 应付款 %s", ErrNotFound, id)
		}
		out = clonePayable(p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateBatch 创建批次：校验并冻结所选明细及金额快照。
// 任一明细不存在、金额非法、币种/商户不一致、或不是可结算状态，
// 整批失败，不留下任何部分冻结。
func (svc *PayoutService) CreateBatch(ctx context.Context, req CreateBatchRequest) (*Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	merchantID := strings.TrimSpace(req.MerchantID)
	if merchantID == "" {
		return nil, fmt.Errorf("%w: merchantID 不能为空", ErrInvalidArgument)
	}
	if len(req.PayableIDs) == 0 {
		return nil, fmt.Errorf("%w: 批次至少包含一笔明细", ErrInvalidArgument)
	}
	seen := make(map[string]struct{}, len(req.PayableIDs))
	for _, id := range req.PayableIDs {
		if strings.TrimSpace(id) == "" {
			return nil, fmt.Errorf("%w: 应付款 ID 不能为空", ErrInvalidArgument)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: 明细 %s 在批次中重复", ErrInvalidArgument, id)
		}
		seen[id] = struct{}{}
	}

	var out *Batch
	err := svc.store.Update(func(tx *Tx) error {
		// 第一阶段：完整校验 + 收集快照。此阶段不做任何写入，
		// 因此后续任一校验失败都不会留下部分冻结。
		items := make([]BatchItem, 0, len(req.PayableIDs))
		var currency string
		var total int64
		for seq, pid := range req.PayableIDs {
			p, ok := tx.getPayable(pid)
			if !ok {
				return fmt.Errorf("%w: 应付款 %s 不存在", ErrNotFound, pid)
			}
			if p.MerchantID != merchantID {
				return fmt.Errorf("%w: 应付款 %s 不属于商户 %s", ErrInvalidArgument, pid, merchantID)
			}
			if err := p.Amount.Validate(); err != nil {
				return err
			}
			if currency == "" {
				currency = p.Amount.Currency
			} else if p.Amount.Currency != currency {
				return fmt.Errorf("%w: 批次内币种不一致（%s 与 %s）", ErrInvalidArgument, currency, p.Amount.Currency)
			}
			if p.Status != PayableSettlable {
				return fmt.Errorf("%w: 应付款 %s 当前状态 %s，仅可结算明细能进入批次（可能已在未结束批次中）",
					ErrConflict, pid, p.Status)
			}
			items = append(items, BatchItem{
				PayableID:  p.ID,
				MerchantID: p.MerchantID,
				AmountSnap: p.Amount,
				Seq:        seq,
			})
			total += p.Amount.Amount
		}

		// 第二阶段：全部校验通过后，同一事务内一次性冻结。
		now := svc.now()
		batch := &Batch{
			ID:          tx.nextID("batch"),
			MerchantID:  merchantID,
			Currency:    currency,
			Status:      BatchOpen,
			Items:       items,
			TotalAmount: Money{Currency: currency, Amount: total},
			CreatedAt:   now,
		}
		tx.putBatch(batch)
		for _, it := range items {
			p := tx.st.payables[it.PayableID]
			p.Status = PayableFrozen
			p.BatchID = batch.ID
			p.Version++
		}
		out = cloneBatch(batch)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetBatch 查询批次。
func (svc *PayoutService) GetBatch(ctx context.Context, batchID string) (*Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out *Batch
	err := svc.store.View(func(tx *Tx) error {
		b, ok := tx.getBatch(batchID)
		if !ok {
			return fmt.Errorf("%w: 批次 %s", ErrNotFound, batchID)
		}
		out = cloneBatch(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListBatchItems 查询批次明细（含金额快照），按行顺序返回。
func (svc *PayoutService) ListBatchItems(ctx context.Context, batchID string) ([]BatchItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var items []BatchItem
	err := svc.store.View(func(tx *Tx) error {
		b, ok := tx.getBatch(batchID)
		if !ok {
			return fmt.Errorf("%w: 批次 %s", ErrNotFound, batchID)
		}
		items = cloneItems(b.Items)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return items, nil
}

// SubmitBatch 将批次提交银行。
//
// 外部提交号保证幂等：
//   - 同号 + 同批次同金额内容：返回首次提交结果（Replayed=true）；
//   - 同号但批次或金额内容不同：ErrIdempotencyConflict；
//   - 提交与取消并发时由存储互斥：先提交成功则取消失败，反之亦然。
func (svc *PayoutService) SubmitBatch(ctx context.Context, req SubmitBatchRequest) (*SubmitResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	externalNo := strings.TrimSpace(req.ExternalNo)
	if strings.TrimSpace(req.BatchID) == "" {
		return nil, fmt.Errorf("%w: batchID 不能为空", ErrInvalidArgument)
	}
	if externalNo == "" {
		return nil, fmt.Errorf("%w: externalNo 不能为空", ErrInvalidArgument)
	}

	var out *SubmitResult
	err := svc.store.Update(func(tx *Tx) error {
		b, ok := tx.getBatch(req.BatchID)
		if !ok {
			return fmt.Errorf("%w: 批次 %s", ErrNotFound, req.BatchID)
		}

		// 外部提交号已被使用：只能是同内容重放，否则冲突。
		if existing, used := tx.getSubmission(externalNo); used {
			if existing.BatchID != b.ID {
				return fmt.Errorf("%w: 外部提交号 %s 已用于批次 %s",
					ErrIdempotencyConflict, externalNo, existing.BatchID)
			}
			fp := batchFingerprint(b)
			if existing.ContentFingerprint != fp {
				return fmt.Errorf("%w: 外部提交号 %s 的批次金额内容与首次提交不一致",
					ErrIdempotencyConflict, externalNo)
			}
			out = &SubmitResult{Replayed: true, Batch: *cloneBatch(b)}
			return nil
		}

		// 新提交号：只有 open 批次可以提交。
		// 已提交属于并发重复提交（不同外部号）；终态批次不能再提交。
		if b.Status != BatchOpen {
			if b.Status == BatchSubmitted {
				return fmt.Errorf("%w: 批次 %s 已提交（外部号 %s），请勿更换外部号重复提交",
					ErrConflict, b.ID, b.ExternalNo)
			}
			return fmt.Errorf("%w: 批次 %s 已处于终态 %s", ErrTerminalState, b.ID, b.Status)
		}

		now := svc.now()
		b.SubmitVersion++
		b.Status = BatchSubmitted
		b.ExternalNo = externalNo
		b.SubmittedAt = now
		tx.putSubmission(&Submission{
			ExternalNo:         externalNo,
			ContentFingerprint: batchFingerprint(b),
			BatchID:            b.ID,
			SubmitVersion:      b.SubmitVersion,
			FirstSubmittedAt:   now,
		})
		out = &SubmitResult{Replayed: false, Batch: *cloneBatch(b)}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CancelBatch 在提交前本地取消批次，并在同一事务内释放全部明细。
// 已提交（含已有终态）的批次不能再被本地取消。
func (svc *PayoutService) CancelBatch(ctx context.Context, req CancelBatchRequest) (*Batch, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.BatchID) == "" {
		return nil, fmt.Errorf("%w: batchID 不能为空", ErrInvalidArgument)
	}

	var out *Batch
	err := svc.store.Update(func(tx *Tx) error {
		b, ok := tx.getBatch(req.BatchID)
		if !ok {
			return fmt.Errorf("%w: 批次 %s", ErrNotFound, req.BatchID)
		}
		// 与 SubmitBatch 在同一把写锁上串行：二者只有一个能成功。
		if b.Status == BatchSubmitted {
			return fmt.Errorf("%w: 批次 %s 已提交银行，不能本地取消", ErrConflict, b.ID)
		}
		if b.Status.IsTerminal() {
			return fmt.Errorf("%w: 批次 %s 已处于终态 %s", ErrTerminalState, b.ID, b.Status)
		}

		now := svc.now()
		b.Status = BatchCanceled
		b.FinishedAt = now
		for _, it := range b.Items {
			releasePayable(tx, it.PayableID, b.ID)
		}
		out = cloneBatch(b)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HandleBankReceipt 处理银行回执，兼容重复、乱序与迟到：
//
//   - 回执必须能通过外部提交号找到提交记录，且批次、提交版本与当前一致才有效；
//   - 有效成功回执：批次置成功、明细结算、生成唯一结算记录与通知（重复回执幂等返回）；
//   - 有效失败回执：批次置失败并在同一事务内释放全部明细；
//   - 旧版本 / 批次不匹配的回执：ignored，绝不覆盖终态。
func (svc *PayoutService) HandleBankReceipt(ctx context.Context, rcpt BankReceipt) (*ReceiptResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	externalNo := strings.TrimSpace(rcpt.ExternalNo)
	if externalNo == "" || strings.TrimSpace(rcpt.BatchID) == "" {
		return nil, fmt.Errorf("%w: 回执缺少外部提交号或批次号", ErrInvalidArgument)
	}
	if rcpt.SubmitVersion <= 0 {
		return nil, fmt.Errorf("%w: 回执提交版本必须为正数", ErrInvalidArgument)
	}

	var out *ReceiptResult
	err := svc.store.Update(func(tx *Tx) error {
		sub, ok := tx.getSubmission(externalNo)
		if !ok {
			return fmt.Errorf("%w: 外部提交号 %s 无对应提交记录（迟到或伪造回执）", ErrNotFound, externalNo)
		}

		// 批次不匹配：迟到/串单回执，忽略。
		if sub.BatchID != rcpt.BatchID {
			b, exists := tx.getBatch(rcpt.BatchID)
			status := BatchStatus("")
			if exists {
				status = b.Status
			}
			out = &ReceiptResult{Outcome: ReceiptIgnored, BatchID: rcpt.BatchID, Status: status}
			return nil
		}

		b, ok := tx.getBatch(sub.BatchID)
		if !ok {
			// 理论不可达：提交记录必然指向存在的批次。
			return fmt.Errorf("%w: 批次 %s", ErrNotFound, sub.BatchID)
		}

		// 提交版本不匹配：旧版本的迟到回执，忽略。
		if rcpt.SubmitVersion != b.SubmitVersion || rcpt.SubmitVersion != sub.SubmitVersion {
			out = &ReceiptResult{Outcome: ReceiptIgnored, BatchID: b.ID, Status: b.Status}
			return nil
		}

		// 版本匹配但批次已终态：重复回执，幂等返回首次结果，不重复生成结算/通知。
		if b.Status.IsTerminal() {
			res := &ReceiptResult{Outcome: ReceiptDuplicate, BatchID: b.ID, Status: b.Status}
			if stl, ok := tx.getSettlementByBatch(b.ID); ok {
				s := *stl
				res.Settlement = &s
			}
			out = res
			return nil
		}

		if b.Status != BatchSubmitted {
			return fmt.Errorf("%w: 批次 %s 当前状态 %s，无法确认回执", ErrConflict, b.ID, b.Status)
		}

		now := svc.now()
		if !rcpt.ReceivedAt.IsZero() {
			now = rcpt.ReceivedAt.UTC()
		}

		if rcpt.Success {
			b.Status = BatchSucceeded
			b.FinishedAt = now
			b.BankResultCode = rcpt.ResultCode
			b.BankResultMessage = rcpt.ResultMessage
			for _, it := range b.Items {
				p, ok := tx.getPayable(it.PayableID)
				if !ok {
					return fmt.Errorf("%w: 应付款 %s", ErrNotFound, it.PayableID)
				}
				p.Status = PayableSettled
				p.Version++
				// p.BatchID 保留，便于追溯。
			}
			// 结算记录按批次唯一（settlements 以 batchID 为键）。
			stl := &Settlement{
				ID:          tx.nextID("stl"),
				BatchID:     b.ID,
				ExternalNo:  externalNo,
				MerchantID:  b.MerchantID,
				Currency:    b.Currency,
				TotalAmount: b.TotalAmount,
				SettledAt:   now,
			}
			tx.putSettlement(stl)
			tx.appendNotification(&Notification{
				ID:           tx.nextID("ntf"),
				SettlementID: stl.ID,
				BatchID:      b.ID,
				MerchantID:   b.MerchantID,
				Message: fmt.Sprintf("批次 %s 银行确认成功，结算金额 %s",
					b.ID, b.TotalAmount.String()),
				CreatedAt: now,
			})
			s := *stl
			out = &ReceiptResult{
				Outcome:    ReceiptConfirmed,
				BatchID:    b.ID,
				Status:     BatchSucceeded,
				Settlement: &s,
			}
			return nil
		}

		// 失败确认：同一事务内释放全部明细，不生成结算记录。
		b.Status = BatchFailed
		b.FinishedAt = now
		b.BankResultCode = rcpt.ResultCode
		b.BankResultMessage = rcpt.ResultMessage
		for _, it := range b.Items {
			releasePayable(tx, it.PayableID, b.ID)
		}
		out = &ReceiptResult{Outcome: ReceiptConfirmed, BatchID: b.ID, Status: BatchFailed}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetSettlement 按批次查询结算记录；无结算记录（未成功）返回 ErrNotFound。
func (svc *PayoutService) GetSettlement(ctx context.Context, batchID string) (*Settlement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out *Settlement
	err := svc.store.View(func(tx *Tx) error {
		if _, ok := tx.getBatch(batchID); !ok {
			return fmt.Errorf("%w: 批次 %s", ErrNotFound, batchID)
		}
		stl, ok := tx.getSettlementByBatch(batchID)
		if !ok {
			return fmt.Errorf("%w: 批次 %s 尚无结算记录", ErrNotFound, batchID)
		}
		s := *stl
		out = &s
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListNotifications 查询全部通知（主要用于测试与运维核对）。
func (svc *PayoutService) ListNotifications(ctx context.Context) ([]Notification, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []Notification
	err := svc.store.View(func(tx *Tx) error {
		ns := tx.allNotifications()
		out = make([]Notification, 0, len(ns))
		for _, n := range ns {
			out = append(out, *n)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ---- 内部辅助 ----

// releasePayable 将明细释放回可结算状态；并校验它确实由该批次冻结。
func releasePayable(tx *Tx, payableID, batchID string) {
	p, ok := tx.getPayable(payableID)
	if !ok {
		return
	}
	if p.BatchID != batchID {
		// 不应发生：冻结/释放严格按批次配对。防御性跳过。
		return
	}
	p.Status = PayableSettlable
	p.BatchID = ""
	p.Version++
}

// batchFingerprint 计算批次提交内容指纹：商户 + 每行（明细ID、币种、最小单位金额，按行顺序）。
// 同一外部提交号再次提交时，批次或任一金额快照不同都会导致指纹变化。
func batchFingerprint(b *Batch) string {
	var sb strings.Builder
	sb.WriteString("v1|")
	sb.WriteString(b.MerchantID)
	sb.WriteString("|")
	items := append([]BatchItem(nil), b.Items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].Seq < items[j].Seq })
	for _, it := range items {
		fmt.Fprintf(&sb, "%s:%s:%d;", it.PayableID, it.AmountSnap.Currency, it.AmountSnap.Amount)
	}
	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:])
}

func clonePayable(p *Payable) *Payable {
	c := *p
	return &c
}

func cloneBatch(b *Batch) *Batch {
	c := *b
	c.Items = cloneItems(b.Items)
	return &c
}

func cloneItems(items []BatchItem) []BatchItem {
	if items == nil {
		return nil
	}
	c := make([]BatchItem, len(items))
	copy(c, items)
	return c
}

// IsNotFound / IsConflict / ... 便捷错误分类。

// IsNotFound 判断是否为 ErrNotFound。
func IsNotFound(err error) bool { return errors.Is(err, ErrNotFound) }

// IsConflict 判断是否为状态冲突错误（ErrConflict 或终态错误）。
func IsConflict(err error) bool {
	return errors.Is(err, ErrConflict) || errors.Is(err, ErrTerminalState)
}

// IsIdempotencyConflict 判断是否为外部提交号幂等冲突。
func IsIdempotencyConflict(err error) bool { return errors.Is(err, ErrIdempotencyConflict) }

// IsInvalidArgument 判断是否为入参错误。
func IsInvalidArgument(err error) bool { return errors.Is(err, ErrInvalidArgument) }
