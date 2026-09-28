package gopayoutbatches

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

const cny = "CNY"

func reg(t *testing.T, svc *Service, merchant, bizNo string, amount int64) *PayableItem {
	t.Helper()
	it, err := svc.RegisterItem(RegisterItemInput{
		MerchantID: merchant,
		BizNo:      bizNo,
		Payee:      "payee-" + bizNo,
		Amount:     MustNewMoney(amount, cny),
	})
	if err != nil {
		t.Fatalf("RegisterItem(%s/%s): %v", merchant, bizNo, err)
	}
	return it
}

func create(t *testing.T, svc *Service, merchant string, ids ...string) *Batch {
	t.Helper()
	b, err := svc.CreateBatch(CreateBatchInput{MerchantID: merchant, ItemIDs: ids})
	if err != nil {
		t.Fatalf("CreateBatch(%v): %v", ids, err)
	}
	return b
}

func submit(t *testing.T, svc *Service, merchant, batchID, extNo string) *SubmitResult {
	t.Helper()
	r, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: merchant, BatchID: batchID, ExternalNo: extNo})
	if err != nil {
		t.Fatalf("SubmitBatch(%s): %v", extNo, err)
	}
	return r
}

func receipt(svc *Service, merchant string, b *Batch, result ReceiptResult, serial string) (*ReceiptOutcome, error) {
	// 重新取最新批次，避免调用方持有的是提交前的旧副本。
	cur, err := svc.GetBatch(merchant, b.ID)
	if err != nil {
		return nil, err
	}
	return svc.ApplyReceipt(merchant, BankReceipt{
		ExternalNo:   cur.Submission.ExternalNo,
		Version:      cur.Submission.Version,
		BatchID:      cur.ID,
		Result:       result,
		BankSerialNo: serial,
		Amount:       cur.TotalAmount,
	})
}

func itemStatus(t *testing.T, svc *Service, id string) ItemStatus {
	t.Helper()
	return svc.store.items[id].Status
}

// ---------------------------------------------------------------------------
// 登记
// ---------------------------------------------------------------------------

func TestRegisterItemIdempotent(t *testing.T) {
	svc := NewService()
	a := reg(t, svc, "M1", "BIZ-1", 100)
	b := reg(t, svc, "M1", "BIZ-1", 100)
	if a.ID != b.ID {
		t.Fatalf("same bizNo should return same item: %s != %s", a.ID, b.ID)
	}
	if a.Status != ItemSettlable {
		t.Fatalf("new item should be SETTLABLE, got %s", a.Status)
	}

	// 同号不同金额 -> 冲突。
	_, err := svc.RegisterItem(RegisterItemInput{
		MerchantID: "M1", BizNo: "BIZ-1", Payee: "payee-BIZ-1",
		Amount: MustNewMoney(200, cny),
	})
	if !errors.Is(err, ErrDuplicateRequest) {
		t.Fatalf("want ErrDuplicateRequest, got %v", err)
	}

	// 非正金额拒绝。
	_, err = svc.RegisterItem(RegisterItemInput{
		MerchantID: "M1", BizNo: "BIZ-2", Payee: "p", Amount: MustNewMoney(0, cny),
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("zero amount want ErrInvalidArgument, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 批次创建：快照、整批失败无部分冻结、不可重复冻结
// ---------------------------------------------------------------------------

func TestCreateBatchSnapshotAndTotal(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 250)
	b := create(t, svc, "M1", i1.ID, i2.ID)

	if b.Status != BatchCreated || b.TotalAmount.Amount != 350 || b.TotalAmount.Currency != cny {
		t.Fatalf("unexpected batch: %+v", b)
	}
	if len(b.Items) != 2 || b.Items[0].Snapshot.Amount != 100 {
		t.Fatalf("snapshots wrong: %+v", b.Items)
	}
	if itemStatus(t, svc, i1.ID) != ItemFrozen || itemStatus(t, svc, i2.ID) != ItemFrozen {
		t.Fatal("items should be FROZEN")
	}
}

func TestCreateBatchAllOrNothing(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 200)
	i3 := reg(t, svc, "M1", "3", 300)
	// 先把 i2 冻结进另一个批次。
	create(t, svc, "M1", i2.ID)

	// 新批次同时引用可结算的 i1、已冻结的 i2、可结算的 i3：必须整批失败，
	// i1/i3 不能留下部分冻结。
	_, err := svc.CreateBatch(CreateBatchInput{MerchantID: "M1", ItemIDs: []string{i1.ID, i2.ID, i3.ID}})
	if !errors.Is(err, ErrItemNotSettlable) {
		t.Fatalf("want ErrItemNotSettlable, got %v", err)
	}
	if got := itemStatus(t, svc, i1.ID); got != ItemSettlable {
		t.Errorf("i1 should remain SETTLABLE after failed batch, got %s", got)
	}
	if got := itemStatus(t, svc, i3.ID); got != ItemSettlable {
		t.Errorf("i3 should remain SETTLABLE after failed batch, got %s", got)
	}
}

func TestCreateBatchValidation(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	iOther := reg(t, svc, "M2", "9", 100)

	if _, err := svc.CreateBatch(CreateBatchInput{MerchantID: "M1", ItemIDs: nil}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("empty items want ErrInvalidArgument, got %v", err)
	}
	if _, err := svc.CreateBatch(CreateBatchInput{MerchantID: "M1", ItemIDs: []string{i1.ID, i1.ID}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("duplicate items want ErrInvalidArgument, got %v", err)
	}
	if _, err := svc.CreateBatch(CreateBatchInput{MerchantID: "M1", ItemIDs: []string{"P9999"}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing item want ErrNotFound, got %v", err)
	}
	if _, err := svc.CreateBatch(CreateBatchInput{MerchantID: "M1", ItemIDs: []string{iOther.ID}}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-merchant item want ErrForbidden, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 取消
// ---------------------------------------------------------------------------

func TestCancelReleasesItems(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	b := create(t, svc, "M1", i1.ID)

	got, err := svc.CancelBatch("M1", b.ID)
	if err != nil || got.Status != BatchCancelled {
		t.Fatalf("CancelBatch = %+v, %v", got, err)
	}
	if itemStatus(t, svc, i1.ID) != ItemSettlable {
		t.Fatal("cancelled batch should release items to SETTLABLE")
	}
	// 释放后可以重新进批次。
	create(t, svc, "M1", i1.ID)
}

func TestCannotCancelAfterSubmit(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	b := create(t, svc, "M1", i1.ID)
	submit(t, svc, "M1", b.ID, "EXT-1")

	if _, err := svc.CancelBatch("M1", b.ID); !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("want ErrAlreadySubmitted, got %v", err)
	}
	if itemStatus(t, svc, i1.ID) != ItemFrozen {
		t.Fatal("item must stay FROZEN after rejected cancel")
	}
}

// ---------------------------------------------------------------------------
// 提交幂等与冲突
// ---------------------------------------------------------------------------

func TestSubmitIdempotentSameContent(t *testing.T) {
	svc := NewService()
	b := create(t, svc, "M1", reg(t, svc, "M1", "1", 100).ID)

	r1 := submit(t, svc, "M1", b.ID, "EXT-1")
	if r1.IdempotentReplay || r1.Submission.Version != 1 {
		t.Fatalf("first submit unexpected: %+v", r1)
	}
	r2 := submit(t, svc, "M1", b.ID, "EXT-1")
	if !r2.IdempotentReplay || r2.Submission.ExternalNo != "EXT-1" || r2.Submission.Version != 1 {
		t.Fatalf("replay should return first submission: %+v", r2)
	}
}

func TestSubmitExternalNoConflicts(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 200)
	b1 := create(t, svc, "M1", i1.ID)
	b2 := create(t, svc, "M1", i2.ID)
	submit(t, svc, "M1", b1.ID, "EXT-1")

	// 同号换批次 -> 冲突。
	_, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: "M1", BatchID: b2.ID, ExternalNo: "EXT-1"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("same external no on other batch want ErrConflict, got %v", err)
	}
	// b2 必须仍然是 CREATED，未被“半提交”。
	if cur, _ := svc.GetBatch("M1", b2.ID); cur.Status != BatchCreated {
		t.Fatalf("b2 status = %s, want CREATED", cur.Status)
	}
}

func TestCannotSubmitTwiceWithDifferentNo(t *testing.T) {
	svc := NewService()
	b := create(t, svc, "M1", reg(t, svc, "M1", "1", 100).ID)
	submit(t, svc, "M1", b.ID, "EXT-1")
	_, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: "M1", BatchID: b.ID, ExternalNo: "EXT-2"})
	if !errors.Is(err, ErrAlreadySubmitted) {
		t.Fatalf("second external no want ErrAlreadySubmitted, got %v", err)
	}
}

// 提交与取消并发：恰好一个成功。
func TestConcurrentSubmitAndCancel(t *testing.T) {
	for n := 0; n < 200; n++ {
		svc := NewService()
		b := create(t, svc, "M1", reg(t, svc, "M1", fmt.Sprintf("biz-%d", n), 100).ID)

		var wg sync.WaitGroup
		var submitOK, cancelOK bool
		var mu sync.Mutex
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: "M1", BatchID: b.ID, ExternalNo: fmt.Sprintf("EXT-%d", n)})
			mu.Lock()
			submitOK = err == nil
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			_, err := svc.CancelBatch("M1", b.ID)
			mu.Lock()
			cancelOK = err == nil
			mu.Unlock()
		}()
		wg.Wait()

		if submitOK == cancelOK {
			t.Fatalf("iteration %d: exactly one of submit/cancel must succeed (submit=%v cancel=%v)",
				n, submitOK, cancelOK)
		}
		cur, _ := svc.GetBatch("M1", b.ID)
		switch {
		case submitOK && cur.Status != BatchSubmitted:
			t.Fatalf("submit won but status=%s", cur.Status)
		case cancelOK && cur.Status != BatchCancelled:
			t.Fatalf("cancel won but status=%s", cur.Status)
		}
	}
}

// ---------------------------------------------------------------------------
// 银行回执
// ---------------------------------------------------------------------------

func TestReceiptSuccessUniqueSettlementAndNotification(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 250)
	b := create(t, svc, "M1", i1.ID, i2.ID)
	submit(t, svc, "M1", b.ID, "EXT-1")

	out, err := receipt(svc, "M1", b, ReceiptSuccess, "BANK-SN-1")
	if err != nil {
		t.Fatalf("ApplyReceipt success: %v", err)
	}
	if out.Batch.Status != BatchSucceeded || out.Settlement == nil || out.Notification == nil {
		t.Fatalf("unexpected outcome: %+v", out)
	}
	if out.Settlement.Amount.Amount != 350 || out.Settlement.BankSerialNo != "BANK-SN-1" {
		t.Fatalf("settlement wrong: %+v", out.Settlement)
	}
	if out.Notification.Type != NotificationTypeSettled || out.Notification.Amount.Amount != 350 {
		t.Fatalf("notification wrong: %+v", out.Notification)
	}
	if itemStatus(t, svc, i1.ID) != ItemSettled || itemStatus(t, svc, i2.ID) != ItemSettled {
		t.Fatal("items should be SETTLED")
	}

	// 查询：结算记录与通知唯一。
	st1, _ := svc.GetSettlement("M1", b.ID)
	n1, _ := svc.GetNotification("M1", b.ID)
	if st1.BankSerialNo != "BANK-SN-1" || n1.ID != out.Notification.ID {
		t.Fatal("query settlement/notification mismatch")
	}

	// 重复回执：幂等放行，不重建产物。
	out2, err := receipt(svc, "M1", b, ReceiptSuccess, "BANK-SN-1")
	if err != nil || !out2.Duplicate {
		t.Fatalf("duplicate success should be idempotent: %+v %v", out2, err)
	}
	if out2.Settlement.BankSerialNo != "BANK-SN-1" {
		t.Fatal("duplicate receipt must not rebuild settlement")
	}
	if len(svc.store.settlements) != 1 || len(svc.store.notifications) != 1 || len(svc.store.notificationID) != 1 {
		t.Fatal("there must be exactly one settlement and one notification")
	}
}

func TestReceiptFailureReleasesAllAtomically(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 200)
	b := create(t, svc, "M1", i1.ID, i2.ID)
	submit(t, svc, "M1", b.ID, "EXT-1")

	out, err := receipt(svc, "M1", b, ReceiptFailure, "")
	if err != nil || out.Batch.Status != BatchFailed {
		t.Fatalf("failure receipt = %+v %v", out, err)
	}
	if itemStatus(t, svc, i1.ID) != ItemSettlable || itemStatus(t, svc, i2.ID) != ItemSettlable {
		t.Fatal("FAILED batch must release all items to SETTLABLE")
	}
	// 不产生结算/通知。
	if _, err := svc.GetSettlement("M1", b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed batch settlement want ErrNotFound, got %v", err)
	}
	// 释放后明细可重新入批。
	b2 := create(t, svc, "M1", i1.ID, i2.ID)
	if b2.TotalAmount.Amount != 300 {
		t.Fatalf("reused items total wrong: %d", b2.TotalAmount.Amount)
	}

	// 重复失败回执幂等；反向成功回执不得覆盖终态。
	if _, err := receipt(svc, "M1", b, ReceiptFailure, ""); err != nil {
		t.Fatalf("duplicate failure: %v", err)
	}
	if _, err := receipt(svc, "M1", b, ReceiptSuccess, "X"); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("success after failure want ErrTerminalState, got %v", err)
	}
	cur, _ := svc.GetBatch("M1", b.ID)
	if cur.Status != BatchFailed {
		t.Fatalf("terminal state overwritten: %s", cur.Status)
	}
}

func TestReceiptSuccessThenFailureRejected(t *testing.T) {
	svc := NewService()
	b := create(t, svc, "M1", reg(t, svc, "M1", "1", 100).ID)
	submit(t, svc, "M1", b.ID, "EXT-1")
	if _, err := receipt(svc, "M1", b, ReceiptSuccess, "SN"); err != nil {
		t.Fatal(err)
	}
	// 迟到的失败回执不能推翻成功终态。
	if _, err := receipt(svc, "M1", b, ReceiptFailure, ""); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("late failure want ErrTerminalState, got %v", err)
	}
}

func TestReceiptRejectsUnknownStaleMismatch(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 200)
	b := create(t, svc, "M1", i1.ID, i2.ID)
	submit(t, svc, "M1", b.ID, "EXT-1")

	base := BankReceipt{ExternalNo: "EXT-1", Version: 1, BatchID: b.ID, Result: ReceiptSuccess, Amount: b.TotalAmount}

	// 完全陌生的外部号。
	r := base
	r.ExternalNo = "EXT-UNKNOWN"
	if _, err := svc.ApplyReceipt("M1", r); !errors.Is(err, ErrUnknownSubmission) {
		t.Fatalf("unknown external no want ErrUnknownSubmission, got %v", err)
	}

	// 旧版本（当前为 v1，构造一个 v0 场景需先存在历史版本；
	// 这里直接验证版本号大于/小于当前版本的处理路径——v2 属于未来版本）。
	r = base
	r.Version = 2
	if _, err := svc.ApplyReceipt("M1", r); !errors.Is(err, ErrUnknownSubmission) {
		t.Fatalf("future version want ErrUnknownSubmission, got %v", err)
	}

	// 批次不匹配。
	r = base
	r.BatchID = "B9999"
	if _, err := svc.ApplyReceipt("M1", r); !errors.Is(err, ErrReceiptMismatch) {
		t.Fatalf("wrong batch want ErrReceiptMismatch, got %v", err)
	}

	// 金额不匹配。
	r = base
	r.Amount = MustNewMoney(999, cny)
	if _, err := svc.ApplyReceipt("M1", r); !errors.Is(err, ErrReceiptMismatch) {
		t.Fatalf("wrong amount want ErrReceiptMismatch, got %v", err)
	}

	// 未提交批次的回执（另一个只创建未提交的批次）。
	b2 := create(t, svc, "M1", reg(t, svc, "M1", "3", 50).ID)
	_, err := svc.ApplyReceipt("M1", BankReceipt{
		ExternalNo: "EXT-NEVER", Version: 1, BatchID: b2.ID,
		Result: ReceiptSuccess, Amount: b2.TotalAmount,
	})
	if !errors.Is(err, ErrUnknownSubmission) {
		t.Fatalf("receipt for never-submitted no want ErrUnknownSubmission, got %v", err)
	}

	// 所有拒绝都不能改变批次状态。
	cur, _ := svc.GetBatch("M1", b.ID)
	if cur.Status != BatchSubmitted {
		t.Fatalf("rejected receipts must not change status, got %s", cur.Status)
	}
}

// 旧提交版本的回执在存在历史版本时必须被判为 stale。
func TestReceiptStaleVersion(t *testing.T) {
	svc := NewService()

	// 白盒构造“版本 2”场景：v1 已存在，当前批次被重新提交到 v2。
	i1 := reg(t, svc, "M1", "1", 100)
	b := create(t, svc, "M1", i1.ID)
	submit(t, svc, "M1", b.ID, "EXT-1")

	// 直接在存储内模拟一次“重新提交”：版本推进到 2。
	err := svc.store.mutate(func() error {
		live := svc.store.batches[b.ID]
		live.Submission.Version = 2
		svc.store.submissionIndex[[2]interface{}{"EXT-1", 2}] = b.ID
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.ApplyReceipt("M1", BankReceipt{
		ExternalNo: "EXT-1", Version: 1, BatchID: b.ID,
		Result: ReceiptSuccess, Amount: b.TotalAmount,
	})
	if !errors.Is(err, ErrStaleReceipt) {
		t.Fatalf("v1 receipt on v2 submission want ErrStaleReceipt, got %v", err)
	}

	// v2 回执正常确认。
	out, err := svc.ApplyReceipt("M1", BankReceipt{
		ExternalNo: "EXT-1", Version: 2, BatchID: b.ID,
		Result: ReceiptSuccess, Amount: b.TotalAmount, BankSerialNo: "SN-2",
	})
	if err != nil || out.Batch.Status != BatchSucceeded {
		t.Fatalf("current version receipt should confirm: %+v %v", out, err)
	}
}

// 乱序场景：失败回执先到（批次仍 SUBMITTED 时它会被接受），
// 成功回执迟到 -> ErrTerminalState，终态保持 FAILED。
func TestReceiptOutOfOrderFailureThenSuccess(t *testing.T) {
	svc := NewService()
	b := create(t, svc, "M1", reg(t, svc, "M1", "1", 100).ID)
	submit(t, svc, "M1", b.ID, "EXT-1")
	if _, err := receipt(svc, "M1", b, ReceiptFailure, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := receipt(svc, "M1", b, ReceiptSuccess, "SN"); !errors.Is(err, ErrTerminalState) {
		t.Fatalf("late success want ErrTerminalState, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// 归属与查询
// ---------------------------------------------------------------------------

func TestMerchantIsolation(t *testing.T) {
	svc := NewService()
	b := create(t, svc, "M1", reg(t, svc, "M1", "1", 100).ID)
	submit(t, svc, "M1", b.ID, "EXT-1")

	if _, err := svc.GetBatch("M2", b.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-merchant GetBatch want ErrForbidden, got %v", err)
	}
	if _, err := svc.CancelBatch("M2", b.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-merchant CancelBatch want ErrForbidden, got %v", err)
	}
	if _, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: "M2", BatchID: b.ID, ExternalNo: "X"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-merchant SubmitBatch want ErrForbidden, got %v", err)
	}
	// 外部号是全局幂等键：别的商户不能用同一号。
	if _, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: "M2", BatchID: "B-NONE", ExternalNo: "EXT-1"}); !errors.Is(err, ErrNotFound) {
		// 先查批次，未找到 -> ErrNotFound（而非泄漏其他商户的冲突信息）。
		t.Fatalf("expected ErrNotFound for missing batch, got %v", err)
	}

	// 别的商户不能用 M1 的 (外部号, 版本) 确认 M1 的批次。
	got, _ := svc.GetBatch("M1", b.ID)
	if _, err := svc.ApplyReceipt("M2", BankReceipt{
		ExternalNo: "EXT-1", Version: 1, BatchID: got.ID,
		Result: ReceiptSuccess, Amount: got.TotalAmount,
	}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-merchant receipt want ErrForbidden, got %v", err)
	}
	if cur, _ := svc.GetBatch("M1", b.ID); cur.Status != BatchSubmitted {
		t.Fatal("rejected cross-merchant receipt must not change status")
	}
}

func TestBatchDetailQuery(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2 := reg(t, svc, "M1", "2", 200)
	b := create(t, svc, "M1", i1.ID, i2.ID)
	submit(t, svc, "M1", b.ID, "EXT-9")

	got, err := svc.GetBatch("M1", b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != BatchSubmitted || got.Submission == nil || got.Submission.ExternalNo != "EXT-9" {
		t.Fatalf("query detail wrong: %+v", got)
	}
	if len(got.Items) != 2 {
		t.Fatalf("detail should include frozen items: %+v", got.Items)
	}

	bs, err := svc.ListBatches("M1")
	if err != nil || len(bs) != 1 {
		t.Fatalf("ListBatches = %v, %v", bs, err)
	}
	its, err := svc.ListItems("M1")
	if err != nil || len(its) != 2 {
		t.Fatalf("ListItems = %v, %v", its, err)
	}
	if _, err := svc.GetBatch("M1", "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing batch want ErrNotFound, got %v", err)
	}
}

// 同号同内容在批次已结束后重放，仍返回首次提交记录。
func TestSubmitIdempotentReplayAfterTerminal(t *testing.T) {
	svc := NewService()
	b := create(t, svc, "M1", reg(t, svc, "M1", "1", 100).ID)
	r1 := submit(t, svc, "M1", b.ID, "EXT-1")
	if _, err := receipt(svc, "M1", b, ReceiptFailure, ""); err != nil {
		t.Fatal(err)
	}
	r3, err := svc.SubmitBatch(SubmitBatchInput{MerchantID: "M1", BatchID: b.ID, ExternalNo: "EXT-1"})
	if err != nil {
		t.Fatalf("same-no same-content replay after terminal should succeed: %v", err)
	}
	if !r3.IdempotentReplay || r3.Submission.ExternalNo != r1.Submission.ExternalNo ||
		r3.Submission.Version != r1.Submission.Version {
		t.Fatalf("replay must return first submission: %+v vs %+v", r3.Submission, r1.Submission)
	}
	if r3.Batch.Status != BatchFailed {
		t.Fatalf("replay batch should reflect current state, got %s", r3.Batch.Status)
	}
}

// 批次内不允许混合币种。
func TestCreateBatchRejectsMixedCurrency(t *testing.T) {
	svc := NewService()
	i1 := reg(t, svc, "M1", "1", 100)
	i2, err := svc.RegisterItem(RegisterItemInput{
		MerchantID: "M1", BizNo: "2", Payee: "p2", Amount: MustNewMoney(100, "USD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateBatch(CreateBatchInput{MerchantID: "M1", ItemIDs: []string{i1.ID, i2.ID}}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("mixed currency want ErrInvalidArgument, got %v", err)
	}
	// 失败后两条明细仍可结算。
	if itemStatus(t, svc, i1.ID) != ItemSettlable || itemStatus(t, svc, i2.ID) != ItemSettlable {
		t.Fatal("no items should be frozen by failed mixed-currency batch")
	}
}

// 终态批次不能再被取消（CANCELLED/SUCCEEDED/FAILED）。
func TestCannotCancelTerminal(t *testing.T) {
	svc := NewService()

	// CANCELLED
	bc := create(t, svc, "M1", reg(t, svc, "M1", "c", 100).ID)
	if _, err := svc.CancelBatch("M1", bc.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelBatch("M1", bc.ID); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("cancel CANCELLED want ErrBatchClosed, got %v", err)
	}

	// SUCCEEDED
	bs := create(t, svc, "M1", reg(t, svc, "M1", "s", 100).ID)
	submit(t, svc, "M1", bs.ID, "EXT-S")
	if _, err := receipt(svc, "M1", bs, ReceiptSuccess, "SN"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CancelBatch("M1", bs.ID); !errors.Is(err, ErrBatchClosed) {
		t.Fatalf("cancel SUCCEEDED want ErrBatchClosed, got %v", err)
	}
}
