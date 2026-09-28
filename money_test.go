package gopayoutbatches

import (
	"testing"
)

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in      string
		defCur  string
		amount  int64
		cur     string
		wantErr bool
	}{
		{"12.34", "CNY", 1234, "CNY", false},
		{"12.34 CNY", "", 1234, "CNY", false},
		{"12.34CNY", "", 1234, "CNY", false},
		{"12.3cny", "", 1230, "CNY", false},
		{"12", "USD", 1200, "USD", false},
		{"0.01", "CNY", 1, "CNY", false},
		{".5", "CNY", 50, "CNY", false},
		{"12.345", "CNY", 0, "", true},             // 超出最小货币单位精度
		{"12.3x", "CNY", 0, "", true},              // 非法币种
		{"12.3", "", 0, "", true},                  // 无币种且未给默认币种
		{"", "CNY", 0, "", true},                   // 空
		{"-1", "CNY", 0, "", true},                 // 负金额不合法
		{"100000000000000000", "CNY", 0, "", true}, // 溢出
	}
	for _, c := range cases {
		m, err := ParseMoney(c.in, c.defCur)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMoney(%q) = %v, want error", c.in, m)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMoney(%q) unexpected error: %v", c.in, err)
			continue
		}
		if m.Amount != c.amount || m.Currency != c.cur {
			t.Errorf("ParseMoney(%q) = %d %s, want %d %s", c.in, m.Amount, m.Currency, c.amount, c.cur)
		}
	}
}

func TestMoneyStringRoundTrip(t *testing.T) {
	for _, s := range []string{"0.00 CNY", "0.01 CNY", "12.34 CNY", "999.90 USD"} {
		m, err := ParseMoney(s, "")
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		if m.String() != s {
			t.Errorf("String() = %q, want %q", m.String(), s)
		}
	}
}

func TestMoneyAdd(t *testing.T) {
	a := MustNewMoney(150, "CNY")
	b := MustNewMoney(250, "CNY")
	sum, err := a.Add(b)
	if err != nil || sum.Amount != 400 || sum.Currency != "CNY" {
		t.Fatalf("Add = %+v, %v", sum, err)
	}

	c := MustNewMoney(1, "USD")
	if _, err := a.Add(c); err == nil {
		t.Error("Add across currencies should fail")
	}

	big := Money{Amount: 1<<63 - 1, Currency: "CNY"}
	if _, err := big.Add(MustNewMoney(1, "CNY")); err == nil {
		t.Error("Add overflow should fail")
	}
}

func TestNewMoneyValidation(t *testing.T) {
	if _, err := NewMoney(-1, "CNY"); err == nil {
		t.Fatal("negative amount should fail")
	}
	if _, err := NewMoney(1, "cn"); err == nil {
		t.Fatal("2-letter currency should fail")
	}
	if _, err := NewMoney(1, "CN1"); err == nil {
		t.Fatal("non-letter currency should fail")
	}
}
