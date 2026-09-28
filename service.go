package gopayoutbatches

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// Service 是商户应付款批次的应用服务，线程安全。
type Service struct {
	store *Store
}

// NewService 创建基于内存存储的服务。
func NewService() *Service {
	return &Service{store: NewStore()}
}

// NewServiceWithStore 使用给定存储构造服务（主要用于测试替换时钟等）。
func NewServiceWithStore(s *Store) *Service {
	if s == nil {
		s = NewStore()
	}
	return &Service{store: s}
}

func (s *Service) now() time.Time { return s.store.now() }

// ---------------------------------------------------------------------------
// 1. 应付款登记
// ---------------------------------------------------------------------------

// RegisterItemInput 登记一笔商户应付款。
type RegisterItemInput struct {
	MerchantID string
	BizNo      string // 商户侧业务单号，与商户ID共同构成幂等键
	Payee      string
	Amount     Money
}

// RegisterItem 登记应付款明细。相同 (MerchantID, BizNo) 且内容一致时
// 幂等返回已有明细；同号但收款人或金额不同则返回 ErrDuplicateRequest。
// 登记成功的明细处于 SETTLABLE 状态，才允许进入批次。
func (s *Service) RegisterItem(in RegisterItemInput) (*PayableItem, error) {
	if in.MerchantID == "" || in.BizNo == "" || in.Payee == "" {
		return nil, fmt.Errorf("%w: merchantID, bizNo and payee are required", ErrInvalidArgument)
	}
	if !in.Amount.IsPositive() {
		return nil, fmt.Errorf("%w: amount must be positive", ErrInvalidArgument)
	}

	return mutateT(s.store, func() (*PayableItem, error) {
		key := [2]string{in.MerchantID, in.BizNo}
		if id, ok := s.store.itemBizIndex[key]; ok {
			existing := s.store.items[id]
			if existing.Payee != in.Payee || !existing.Amount.Equal(in.Amount) {
				return nil, fmt.Errorf("%w: bizNo %q already registered with different payee/amount",
					ErrDuplicateRequest, in.BizNo)
			}
			return cloneItem(existing), nil
		}

		it := &PayableItem{
			ID:         s.store.nextID("P"),
			MerchantID: in.MerchantID,
			BizNo:      in.BizNo,
			Payee:      in.Payee,
			Amount:     in.Amount,
			Status:     ItemSettlable,
			CreatedAt:  s.now(),
		}
		s.store.items[it.ID] = it
		s.store.itemBizIndex[key] = it.ID
		return cloneItem(it), nil
	})
}

// ListItems 返回某商户名下的全部应付款明细。
func (s *Service) ListItems(merchantID string) ([]*PayableItem, error) {
	if merchantID == "" {
		return nil, fmt.Errorf("%w: merchantID is required", ErrInvalidArgument)
	}
	return mutateT(s.store, func() ([]*PayableItem, error) {
		out := make([]*PayableItem, 0)
		for _, it := range s.store.items {
			if it.MerchantID == merchantID {
				out = append(out, cloneItem(it))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// 2. 批次创建：整批校验 + 冻结 + 金额快照
// ---------------------------------------------------------------------------

// CreateBatchInput 创建批次的输入。
type CreateBatchInput struct {
	MerchantID string
	ItemIDs    []string // 选中的应付款明细，至少一条，不可重复
}

// CreateBatch 用选中的可结算明细创建批次。
//
// 原子保证（同一持久化边界内完成）：
//   - 只有 SETTLABLE 明细可以入批；不存在 / 跨商户 / 非可结算任一不满足，
//     整批失败，不产生任何冻结；
//   - 同一明细不可能同时存在于两个未结束批次（FROZEN 状态被锁内校验）；
//   - 入批时固化金额快照并汇总总金额。
func (s *Service) CreateBatch(in CreateBatchInput) (*Batch, error) {
	if in.MerchantID == "" {
		return nil, fmt.Errorf("%w: merchantID is required", ErrInvalidArgument)
	}
	if len(in.ItemIDs) == 0 {
		return nil, fmt.Errorf("%w: at least one item is required", ErrInvalidArgument)
	}
	seen := make(map[string]struct{}, len(in.ItemIDs))
	for _, id := range in.ItemIDs {
		if id == "" {
			return nil, fmt.Errorf("%w: empty item id", ErrInvalidArgument)
		}
		if _, dup := seen[id]; dup {
			return nil, fmt.Errorf("%w: duplicated item %s in batch", ErrInvalidArgument, id)
		}
		seen[id] = struct{}{}
	}

	return mutateT(s.store, func() (*Batch, error) {
		// 第一阶段：整批校验。任何一条不通过都直接返回，
		// 此时尚未修改任何状态，因此不会留下部分冻结。
		frozen := make([]BatchItem, 0, len(in.ItemIDs))
		var total Money
		currencySet := false

		for _, id := range in.ItemIDs {
			it, ok := s.store.items[id]
			if !ok {
				return nil, fmt.Errorf("%w: item %s", ErrNotFound, id)
			}
			if it.MerchantID != in.MerchantID {
				return nil, fmt.Errorf("%w: item %s belongs to another merchant", ErrForbidden, id)
			}
			if it.Status != ItemSettlable {
				return nil, fmt.Errorf("%w: item %s is %s", ErrItemNotSettlable, id, it.Status)
			}

			if !currencySet {
				total = MustNewMoney(0, it.Amount.Currency)
				currencySet = true
			}
			sum, err := total.Add(it.Amount)
			if err != nil {
				return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
			}
			total = sum
			frozen = append(frozen, BatchItem{ItemID: id, Snapshot: it.Amount})
		}

		// 第二阶段：全部通过后才落库——校验与冻结在同一临界区内，
		// 不存在“校验通过但被别人抢先冻结”的窗口。
		now := s.now()
		b := &Batch{
			ID:          s.store.nextID("B"),
			MerchantID:  in.MerchantID,
			Status:      BatchCreated,
			Items:       frozen,
			TotalAmount: total,
			CreatedAt:   now,
		}
		b.contentHash = contentHash(b.MerchantID, b.Items)
		s.store.batches[b.ID] = b

		for _, fi := range frozen {
			s.store.items[fi.ItemID].Status = ItemFrozen
		}
		return cloneBatch(b), nil
	})
}

// contentHash 计算批次提交内容指纹：商户 + 每条明细（按入批顺序）
// 的明细ID与快照金额。提交幂等的“同内容”即比对该指纹。
func contentHash(merchantID string, items []BatchItem) string {
	h := sha256.New()
	fmt.Fprintf(h, "m=%s;n=%d", merchantID, len(items))
	for _, it := range items {
		fmt.Fprintf(h, "|%s:%d:%s", it.ItemID, it.Snapshot.Amount, it.Snapshot.Currency)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------------------
// 3. 批次提交银行：外部提交号幂等
// ---------------------------------------------------------------------------

// SubmitBatchInput 提交批次到银行。
type SubmitBatchInput struct {
	MerchantID string
	BatchID    string
	ExternalNo string // 外部提交号（幂等键）
}

// SubmitResult 提交结果；IdempotentReplay=true 表示这是同号同内容的
// 重复提交，返回的是首次提交记录，没有产生新提交。
type SubmitResult struct {
	Batch            *Batch
	Submission       *Submission
	IdempotentReplay bool
}

// SubmitBatch 把已创建的批次提交银行。
//
// 幂等与冲突：
//   - 外部提交号首次使用：仅 CREATED 批次可提交，提交后进入 SUBMITTED；
//     已提交批次换号再提交报 ErrAlreadySubmitted，终态批次报 ErrBatchClosed；
//   - 同号 + 同批次 + 同内容：任意状态下都返回首次提交结果
//     （IdempotentReplay=true，Batch 带最新状态）；
//   - 同号但对应另一个批次，或批次内容/金额不一致：ErrConflict。
//
// 与 CancelBatch 在同一把锁上竞争，因此并发的“提交/取消”恰好一个成功。
func (s *Service) SubmitBatch(in SubmitBatchInput) (*SubmitResult, error) {
	if in.MerchantID == "" || in.BatchID == "" || in.ExternalNo == "" {
		return nil, fmt.Errorf("%w: merchantID, batchID and externalNo are required", ErrInvalidArgument)
	}

	return mutateT(s.store, func() (*SubmitResult, error) {
		b, err := s.loadOwnedBatch(in.BatchID, in.MerchantID)
		if err != nil {
			return nil, err
		}

		if boundID, used := s.store.externalIndex[in.ExternalNo]; used {
			bound := s.store.batches[boundID]

			// 同号绑定到了另一个批次。
			if bound.ID != b.ID {
				return nil, fmt.Errorf("%w: externalNo %q already used by batch %s",
					ErrConflict, in.ExternalNo, boundID)
			}
			// 同批次：内容（含金额快照）必须一致。
			if bound.contentHash != b.contentHash {
				return nil, fmt.Errorf("%w: externalNo %q reused with different batch content/amount",
					ErrConflict, in.ExternalNo)
			}

			// 同号 + 同批次 + 同内容：无论批次当前处于什么状态（已提交或
			// 已结束），都幂等返回首次提交记录（Batch 带最新状态），不产生
			// 新提交。批次内容创建后不可变，此处内容比对仅作防御。
			return &SubmitResult{
				Batch:            cloneBatch(b),
				Submission:       clonePtr(b.Submission),
				IdempotentReplay: true,
			}, nil
		}

		// 外部号首次使用。
		switch b.Status {
		case BatchCreated:
			// 正常提交路径。
		case BatchSubmitted:
			return nil, fmt.Errorf("%w: batch %s was already submitted under externalNo %q",
				ErrAlreadySubmitted, b.ID, b.Submission.ExternalNo)
		default:
			return nil, fmt.Errorf("%w: batch %s is %s", ErrBatchClosed, b.ID, b.Status)
		}

		now := s.now()
		sub := &Submission{
			BatchID:     b.ID,
			MerchantID:  b.MerchantID,
			ExternalNo:  in.ExternalNo,
			Version:     1,
			ContentHash: b.contentHash,
			SubmittedAt: now,
		}
		b.Status = BatchSubmitted
		b.Submission = sub
		b.SubmittedAt = now
		s.store.externalIndex[in.ExternalNo] = b.ID
		s.store.submissionIndex[[2]interface{}{in.ExternalNo, sub.Version}] = b.ID

		return &SubmitResult{Batch: cloneBatch(b), Submission: clonePtr(sub)}, nil
	})
}

// ---------------------------------------------------------------------------
// 4. 批次取消（仅提交前）
// ---------------------------------------------------------------------------

// CancelBatch 在提交银行之前本地取消批次：批次置 CANCELLED，
// 其冻结的全部明细在同一持久化边界内释放回 SETTLABLE。
// 已提交（SUBMITTED 或终态）的批次返回 ErrAlreadySubmitted / ErrBatchClosed。
func (s *Service) CancelBatch(merchantID, batchID string) (*Batch, error) {
	if merchantID == "" || batchID == "" {
		return nil, fmt.Errorf("%w: merchantID and batchID are required", ErrInvalidArgument)
	}

	return mutateT(s.store, func() (*Batch, error) {
		b, err := s.loadOwnedBatch(batchID, merchantID)
		if err != nil {
			return nil, err
		}
		switch b.Status {
		case BatchCreated:
			// 可取消：释放冻结。
		case BatchSubmitted:
			return nil, fmt.Errorf("%w: batch %s", ErrAlreadySubmitted, b.ID)
		default:
			return nil, fmt.Errorf("%w: batch %s is %s", ErrBatchClosed, b.ID, b.Status)
		}

		for _, fi := range b.Items {
			s.store.items[fi.ItemID].Status = ItemSettlable
		}
		b.Status = BatchCancelled
		b.FinishedAt = s.now()
		return cloneBatch(b), nil
	})
}

// ---------------------------------------------------------------------------
// 5. 银行回执：匹配版本、终态保护、原子释放 / 唯一结算
// ---------------------------------------------------------------------------

// ReceiptOutcome 回执处理结果。Duplicate=true 表示这是对既有终态的
// 重复确认（内容一致），未重复生成结算记录或通知。
type ReceiptOutcome struct {
	Batch        *Batch
	Result       ReceiptResult
	Duplicate    bool
	Settlement   *Settlement
	Notification *Notification
}

// ApplyReceipt 处理一条可能重复、乱序或迟到的银行回执。
//
// 确认规则：
//   - 必须同时匹配当前批次（ExternalNo 绑定的批次、BatchID）与提交版本，
//     且银行回传金额与批次快照总额一致，否则返回
//     ErrUnknownSubmission / ErrStaleReceipt / ErrReceiptMismatch；
//   - 仅 SUBMITTED 批次可被确认；
//   - 失败：批次 FAILED，全部明细在同一持久化边界释放回 SETTLABLE；
//   - 成功：批次 SUCCEEDED，明细 SETTLED，并生成“唯一”的结算记录和通知；
//   - 重复的同向回执幂等放行但不重建产物；反向回执或任何试图改写终态的
//     回执返回 ErrTerminalState，旧回执永远不能覆盖终态。
func (s *Service) ApplyReceipt(merchantID string, r BankReceipt) (*ReceiptOutcome, error) {
	if merchantID == "" || r.ExternalNo == "" || r.BatchID == "" {
		return nil, fmt.Errorf("%w: merchantID, externalNo and batchID are required", ErrInvalidArgument)
	}
	if r.Version < 1 {
		return nil, fmt.Errorf("%w: submission version must be >= 1", ErrInvalidArgument)
	}
	if r.Result != ReceiptSuccess && r.Result != ReceiptFailure {
		return nil, fmt.Errorf("%w: unknown receipt result %q", ErrInvalidArgument, r.Result)
	}

	return mutateT(s.store, func() (*ReceiptOutcome, error) {
		// 以 (外部提交号, 提交版本) 精确定位提交：索引里没有这一对，
		// 说明它要么引用了从未登记的外部号，要么版本号无对应提交
		// （乱序到达的未来版本 / 迟到但系统已无记录）。
		resolvedID, pairKnown := s.store.submissionIndex[[2]interface{}{r.ExternalNo, r.Version}]
		if !pairKnown {
			if _, extKnown := s.store.externalIndex[r.ExternalNo]; !extKnown {
				return nil, fmt.Errorf("%w: externalNo %q", ErrUnknownSubmission, r.ExternalNo)
			}
			return nil, fmt.Errorf("%w: externalNo %q has no submission v%d",
				ErrUnknownSubmission, r.ExternalNo, r.Version)
		}
		b, err := s.loadOwnedBatch(resolvedID, merchantID)
		if err != nil {
			return nil, err
		}

		// 这一对 (外部号, 版本) 真实存在，但必须是“当前”提交才能确认：
		// 指向历史版本即为旧回执（ErrStaleReceipt），不允许确认当前批次。
		if b.Submission == nil || b.Submission.ExternalNo != r.ExternalNo {
			return nil, fmt.Errorf("%w: externalNo %q not bound to batch %s",
				ErrUnknownSubmission, r.ExternalNo, b.ID)
		}
		if r.Version < b.Submission.Version {
			return nil, fmt.Errorf("%w: receipt v%d < current v%d for %q",
				ErrStaleReceipt, r.Version, b.Submission.Version, r.ExternalNo)
		}
		if r.Version > b.Submission.Version {
			// 索引中有该版本但批次当前版本更低，属于不一致的乱序状态。
			return nil, fmt.Errorf("%w: receipt v%d > current v%d for %q (out of order)",
				ErrUnknownSubmission, r.Version, b.Submission.Version, r.ExternalNo)
		}

		// 批次与金额交叉校验。
		if r.BatchID != b.ID {
			return nil, fmt.Errorf("%w: receipt batch %s != current batch %s",
				ErrReceiptMismatch, r.BatchID, b.ID)
		}
		if !r.Amount.Equal(b.TotalAmount) {
			return nil, fmt.Errorf("%w: receipt amount %s != batch amount %s",
				ErrReceiptMismatch, r.Amount, b.TotalAmount)
		}

		out := &ReceiptOutcome{Result: r.Result}

		// 终态保护：终态不可覆盖。同向重复回执幂等放行。
		if b.Status.IsTerminal() {
			sameOutcome := (b.Status == BatchSucceeded && r.Result == ReceiptSuccess) ||
				(b.Status == BatchFailed && r.Result == ReceiptFailure)
			if !sameOutcome {
				return nil, fmt.Errorf("%w: batch %s is %s, receipt %s rejected",
					ErrTerminalState, b.ID, b.Status, r.Result)
			}
			out.Batch = cloneBatch(b)
			out.Duplicate = true
			out.Settlement = cloneSettlement(b.Settlement)
			if nid := s.store.notificationID[b.ID]; nid != "" {
				out.Notification = cloneNotification(s.store.notifications[nid])
			}
			return out, nil
		}

		if b.Status != BatchSubmitted {
			return nil, fmt.Errorf("%w: batch %s is %s, no active submission",
				ErrConflict, b.ID, b.Status)
		}

		now := s.now()
		b.FinishedAt = now

		if r.Result == ReceiptFailure {
			// 失败：同一持久化边界内释放全部明细。
			for _, fi := range b.Items {
				s.store.items[fi.ItemID].Status = ItemSettlable
			}
			b.Status = BatchFailed
			out.Batch = cloneBatch(b)
			return out, nil
		}

		// 成功：先做产物唯一性防御检查，再一次性落库。
		if _, exists := s.store.settlements[b.ID]; exists {
			return nil, fmt.Errorf("%w: settlement for batch %s already exists",
				ErrTerminalState, b.ID)
		}
		for _, fi := range b.Items {
			s.store.items[fi.ItemID].Status = ItemSettled
		}
		st := &Settlement{
			BatchID:      b.ID,
			MerchantID:   b.MerchantID,
			Amount:       b.TotalAmount,
			BankSerialNo: r.BankSerialNo,
			SettledAt:    now,
		}
		s.store.settlements[b.ID] = st

		n := &Notification{
			ID:         s.store.nextID("N"),
			Type:       NotificationTypeSettled,
			BatchID:    b.ID,
			MerchantID: b.MerchantID,
			Amount:     b.TotalAmount,
			CreatedAt:  now,
		}
		s.store.notifications[n.ID] = n
		s.store.notificationID[b.ID] = n.ID

		b.Status = BatchSucceeded
		b.Settlement = st
		out.Batch = cloneBatch(b)
		out.Settlement = cloneSettlement(st)
		out.Notification = cloneNotification(n)
		return out, nil
	})
}

// ---------------------------------------------------------------------------
// 6. 查询
// ---------------------------------------------------------------------------

// GetBatch 查询批次（含冻结明细快照、提交信息、结算记录）。
func (s *Service) GetBatch(merchantID, batchID string) (*Batch, error) {
	if merchantID == "" || batchID == "" {
		return nil, fmt.Errorf("%w: merchantID and batchID are required", ErrInvalidArgument)
	}
	return mutateT(s.store, func() (*Batch, error) {
		return s.getOwnedBatch(batchID, merchantID)
	})
}

// ListBatches 返回某商户的全部批次（按ID排序）。
func (s *Service) ListBatches(merchantID string) ([]*Batch, error) {
	if merchantID == "" {
		return nil, fmt.Errorf("%w: merchantID is required", ErrInvalidArgument)
	}
	return mutateT(s.store, func() ([]*Batch, error) {
		out := make([]*Batch, 0)
		for _, b := range s.store.batches {
			if b.MerchantID == merchantID {
				out = append(out, cloneBatch(b))
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out, nil
	})
}

// GetSettlement 查询批次成功后生成的唯一结算记录；未成功返回 ErrNotFound。
func (s *Service) GetSettlement(merchantID, batchID string) (*Settlement, error) {
	return mutateT(s.store, func() (*Settlement, error) {
		if _, err := s.getOwnedBatch(batchID, merchantID); err != nil {
			return nil, err
		}
		st, ok := s.store.settlements[batchID]
		if !ok {
			return nil, fmt.Errorf("%w: settlement for batch %s", ErrNotFound, batchID)
		}
		return cloneSettlement(st), nil
	})
}

// GetNotification 查询批次成功后生成的唯一通知；未成功返回 ErrNotFound。
func (s *Service) GetNotification(merchantID, batchID string) (*Notification, error) {
	return mutateT(s.store, func() (*Notification, error) {
		if _, err := s.getOwnedBatch(batchID, merchantID); err != nil {
			return nil, err
		}
		nid, ok := s.store.notificationID[batchID]
		if !ok {
			return nil, fmt.Errorf("%w: notification for batch %s", ErrNotFound, batchID)
		}
		return cloneNotification(s.store.notifications[nid]), nil
	})
}

// loadOwnedBatch 取出存储内的批次真实指针并做归属校验。
// 调用必须已在持锁的 mutate 临界区内；对返回值的修改即对存储的修改，
// 因此只用于写路径。读路径请用 getOwnedBatch（返回深拷贝）。
func (s *Service) loadOwnedBatch(batchID, merchantID string) (*Batch, error) {
	b, ok := s.store.batches[batchID]
	if !ok {
		return nil, fmt.Errorf("%w: batch %s", ErrNotFound, batchID)
	}
	if b.MerchantID != merchantID {
		return nil, fmt.Errorf("%w: batch %s", ErrForbidden, batchID)
	}
	return b, nil
}

// getOwnedBatch 取出批次并做归属校验，返回深拷贝，供读路径使用。
// 调用必须已在持锁临界区内。
func (s *Service) getOwnedBatch(batchID, merchantID string) (*Batch, error) {
	b, err := s.loadOwnedBatch(batchID, merchantID)
	if err != nil {
		return nil, err
	}
	return cloneBatch(b), nil
}

func clonePtr(p *Submission) *Submission {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}
