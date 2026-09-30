package gopayoutbatches

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

// 同一明细被两个批次并发选中时，必须恰好一个批次创建成功，
// 另一个收到 ErrItemNotSettlable，明细最终只属于一个批次。
func TestConcurrentCreateBatchSameItem(t *testing.T) {
	for n := 0; n < 200; n++ {
		svc := NewService()
		item := reg(t, svc, "M1", fmt.Sprintf("biz-%d", n), 100)

		var wg sync.WaitGroup
		var okCount, failCount int
		var counterMu sync.Mutex
		wg.Add(2)
		for g := 0; g < 2; g++ {
			go func() {
				defer wg.Done()
				_, err := svc.CreateBatch(CreateBatchInput{
					MerchantID: "M1",
					ItemIDs:    []string{item.ID},
				})
				counterMu.Lock()
				switch {
				case err == nil:
					okCount++
				case errors.Is(err, ErrItemNotSettlable):
					failCount++
				default:
					t.Errorf("unexpected create error: %v", err)
				}
				counterMu.Unlock()
			}()
		}
		wg.Wait()

		if okCount != 1 || failCount != 1 {
			t.Fatalf("iteration %d: want exactly one successful batch, got ok=%d fail=%d",
				n, okCount, failCount)
		}
		if got := itemStatus(t, svc, item.ID); got != ItemFrozen {
			t.Fatalf("iteration %d: item should be FROZEN, got %s", n, got)
		}
	}
}

// 多条成功回执并发到达时，只有一个能确认批次，其余只能作为终态幂等重放；
// 结算记录与通知必须只生成一次，全部明细最终为 SETTLED。
func TestConcurrentSuccessReceiptsCreateSingleSettlement(t *testing.T) {
	for n := 0; n < 100; n++ {
		svc := NewService()
		i1 := reg(t, svc, "M1", fmt.Sprintf("a-%d", n), 100)
		i2 := reg(t, svc, "M1", fmt.Sprintf("b-%d", n), 200)
		b := create(t, svc, "M1", i1.ID, i2.ID)
		cur := submit(t, svc, "M1", b.ID, fmt.Sprintf("EXT-%d", n)).Batch

		const writers = 8
		var wg sync.WaitGroup
		var confirmed, duplicates int
		var counterMu sync.Mutex
		wg.Add(writers)
		for g := 0; g < writers; g++ {
			go func() {
				defer wg.Done()
				out, err := svc.ApplyReceipt("M1", BankReceipt{
					ExternalNo:   cur.Submission.ExternalNo,
					Version:      cur.Submission.Version,
					BatchID:      cur.ID,
					Result:       ReceiptSuccess,
					BankSerialNo: "BANK-SN",
					Amount:       cur.TotalAmount,
				})
				if err != nil {
					t.Errorf("concurrent success receipt: %v", err)
					return
				}
				counterMu.Lock()
				if out.Duplicate {
					duplicates++
				} else {
					confirmed++
				}
				counterMu.Unlock()
			}()
		}
		wg.Wait()

		if confirmed != 1 || duplicates != writers-1 {
			t.Fatalf("iteration %d: want 1 confirmation and %d duplicates, got %d/%d",
				n, writers-1, confirmed, duplicates)
		}
		if len(svc.store.settlements) != 1 || len(svc.store.notifications) != 1 {
			t.Fatalf("iteration %d: want exactly one settlement/notification, got %d/%d",
				n, len(svc.store.settlements), len(svc.store.notifications))
		}
		if itemStatus(t, svc, i1.ID) != ItemSettled || itemStatus(t, svc, i2.ID) != ItemSettled {
			t.Fatalf("iteration %d: items should be SETTLED", n)
		}
	}
}

// 并发的成功与失败回执只有一个方向能确认终态；
// 若失败获胜，全部明细必须仍原子释放回 SETTLABLE（不存在半释放）。
func TestConcurrentMixedReceiptsSingleTerminal(t *testing.T) {
	for n := 0; n < 100; n++ {
		svc := NewService()
		i1 := reg(t, svc, "M1", fmt.Sprintf("x-%d", n), 100)
		i2 := reg(t, svc, "M1", fmt.Sprintf("y-%d", n), 200)
		b := create(t, svc, "M1", i1.ID, i2.ID)
		cur := submit(t, svc, "M1", b.ID, fmt.Sprintf("EXTM-%d", n)).Batch

		receiptFor := func(result ReceiptResult) (*ReceiptOutcome, error) {
			return svc.ApplyReceipt("M1", BankReceipt{
				ExternalNo:   cur.Submission.ExternalNo,
				Version:      cur.Submission.Version,
				BatchID:      cur.ID,
				Result:       result,
				BankSerialNo: "SN",
				Amount:       cur.TotalAmount,
			})
		}

		var wg sync.WaitGroup
		var successSeen, failureSeen int
		var mu sync.Mutex
		wg.Add(2)
		go func() {
			defer wg.Done()
			out, err := receiptFor(ReceiptSuccess)
			mu.Lock()
			if err == nil && !out.Duplicate {
				successSeen++
			}
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			out, err := receiptFor(ReceiptFailure)
			mu.Lock()
			if err == nil && !out.Duplicate {
				failureSeen++
			}
			mu.Unlock()
		}()
		wg.Wait()

		if successSeen+failureSeen != 1 {
			t.Fatalf("iteration %d: exactly one terminal outcome, got success=%d failure=%d",
				n, successSeen, failureSeen)
		}

		final, _ := svc.GetBatch("M1", b.ID)
		switch final.Status {
		case BatchSucceeded:
			if itemStatus(t, svc, i1.ID) != ItemSettled || itemStatus(t, svc, i2.ID) != ItemSettled {
				t.Fatalf("iteration %d: SUCCEEDED items must be SETTLED", n)
			}
			if len(svc.store.settlements) != 1 || len(svc.store.notifications) != 1 {
				t.Fatalf("iteration %d: SUCCEEDED must create one settlement/notification", n)
			}
		case BatchFailed:
			if itemStatus(t, svc, i1.ID) != ItemSettlable || itemStatus(t, svc, i2.ID) != ItemSettlable {
				t.Fatalf("iteration %d: FAILED must release ALL items to SETTLABLE", n)
			}
			if len(svc.store.settlements) != 0 || len(svc.store.notifications) != 0 {
				t.Fatalf("iteration %d: FAILED must not create settlement/notification", n)
			}
		default:
			t.Fatalf("iteration %d: unexpected final status %s", n, final.Status)
		}

		// 终态确定后，反方向回执必须被拒绝。
		opposite := ReceiptFailure
		if final.Status == BatchFailed {
			opposite = ReceiptSuccess
		}
		if _, err := receiptFor(opposite); !errors.Is(err, ErrTerminalState) {
			t.Fatalf("iteration %d: opposite receipt want ErrTerminalState, got %v", n, err)
		}
	}
}
