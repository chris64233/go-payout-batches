package gopayoutbatches

import "testing"

func TestParseMoney(t *testing.T) {
	cases := []struct {
		currency string
		decimal  string
		want     int64
	}{
		{"CNY", "12.34", 1234},
		{"cny", "12.34", 1234},
		{"CNY", "12", 1200},
		{"CNY", "0.01", 1},
		{"CNY", "-0.50", -50},
		{"CNY", "100.", 10000},
		{"CNY", ".5", 50},
		{"JPY", "100", 100},
		{"JPY", "100.00", 100}, // 零尾数不丢失精度，按 100 日元处理
	}
	for _, c := range cases {
		m, err := ParseMoney(c.currency, c.decimal)
		if err != nil {
			t.Fatalf("ParseMoney(%s,%s) 意外错误: %v", c.currency, c.decimal, err)
		}
		if m.Amount != c.want {
			t.Errorf("ParseMoney(%s,%s) = %d, want %d", c.currency, c.decimal, m.Amount, c.want)
		}
	}
}

func TestParseMoneyErrors(t *testing.T) {
	bad := []struct {
		currency string
		decimal  string
	}{
		{"XXX", "1.00"},  // 未知币种
		{"CNY", ""},      // 空
		{"CNY", "1.234"}, // 超过币种精度
		{"CNY", "abc"},   // 非数字
		{"CNY", "1.2.3"}, // 多个小数点
	}
	for _, c := range bad {
		_, err := ParseMoney(c.currency, c.decimal)
		if err == nil {
			t.Errorf("ParseMoney(%s,%q) 应当失败", c.currency, c.decimal)
			continue
		}
		if !IsInvalidArgument(err) {
			t.Errorf("ParseMoney(%s,%q) 错误 %v 应归类为入参错误", c.currency, c.decimal, err)
		}
	}

	if _, err := NewMoney("CNY", -1); !IsInvalidArgument(err) {
		t.Errorf("负金额应归类为入参错误，got %v", err)
	}
}

func TestMoneyStringAndAdd(t *testing.T) {
	m := MustParseMoney("CNY", "12.34")
	if m.String() != "CNY 12.34" {
		t.Errorf("String = %q", m.String())
	}
	jpy := MustParseMoney("JPY", "1234")
	if jpy.String() != "JPY 1234" {
		t.Errorf("String = %q", jpy.String())
	}
	if _, err := m.Add(jpy); err == nil {
		t.Error("不同币种相加应报错")
	}
	sum, err := m.Add(MustParseMoney("CNY", "0.66"))
	if err != nil {
		t.Fatal(err)
	}
	if sum.Amount != 1300 {
		t.Errorf("sum = %d, want 1300", sum.Amount)
	}
}
