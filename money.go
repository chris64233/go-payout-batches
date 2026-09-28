package gopayoutbatches

import (
	"errors"
	"fmt"
	"strings"
)

// 金额一律以最小货币单位（分）表示，杜绝浮点误差。

// 常见币种的小数位数。
var currencyExponents = map[string]int{
	"CNY": 2,
	"USD": 2,
	"EUR": 2,
	"JPY": 0,
}

// Money 精确金额：Amount 为按币种小数位换算后的最小单位整数，例如
// CNY 12.34 -> Amount == 1234；JPY 100 -> Amount == 100。
type Money struct {
	Currency string
	Amount   int64
}

// NewMoney 以最小货币单位构造金额。
func NewMoney(currency string, amountMinor int64) (Money, error) {
	m := Money{Currency: strings.ToUpper(strings.TrimSpace(currency)), Amount: amountMinor}
	if err := m.Validate(); err != nil {
		return Money{}, err
	}
	return m, nil
}

// ParseMoney 将十进制字符串解析为精确金额，例如 "12.34" / "12" / "-0.50"。
func ParseMoney(currency, decimal string) (Money, error) {
	cur := strings.ToUpper(strings.TrimSpace(currency))
	exp, ok := currencyExponents[cur]
	if !ok {
		return Money{}, fmt.Errorf("%w: 未知币种 %q", ErrInvalidAmount, currency)
	}

	s := strings.TrimSpace(decimal)
	if s == "" {
		return Money{}, fmt.Errorf("%w: 空金额", ErrInvalidAmount)
	}
	neg := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		neg = true
		s = s[1:]
	}
	if s == "" {
		return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, decimal)
	}

	intPart, fracPart := s, ""
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		intPart, fracPart = s[:dot], s[dot+1:]
		if strings.IndexByte(fracPart, '.') >= 0 {
			return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, decimal)
		}
	}
	if intPart == "" {
		intPart = "0"
	}
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, decimal)
		}
	}
	var amount int64
	for _, c := range intPart {
		amount = amount*10 + int64(c-'0')
	}
	for i := 0; i < exp; i++ {
		d := int64(0)
		if i < len(fracPart) {
			c := fracPart[i]
			if c < '0' || c > '9' {
				return Money{}, fmt.Errorf("%w: %q", ErrInvalidAmount, decimal)
			}
			d = int64(c - '0')
		}
		amount = amount*10 + d
	}
	if len(fracPart) > exp {
		// 超出币种精度的尾数不允许静默截断。
		for _, c := range fracPart[exp:] {
			if c != '0' {
				return Money{}, fmt.Errorf("%w: %q 超过币种 %s 的精度位数 %d", ErrInvalidAmount, decimal, cur, exp)
			}
		}
	}
	if neg {
		amount = -amount
	}
	return Money{Currency: cur, Amount: amount}, nil
}

// MustParseMoney 在解析失败时 panic，适合测试与常量构造。
func MustParseMoney(currency, decimal string) Money {
	m, err := ParseMoney(currency, decimal)
	if err != nil {
		panic(err)
	}
	return m
}

// Validate 校验币种合法、金额非负。
func (m Money) Validate() error {
	if _, ok := currencyExponents[m.Currency]; !ok {
		return fmt.Errorf("%w: 未知币种 %q", ErrInvalidAmount, m.Currency)
	}
	if m.Amount < 0 {
		return fmt.Errorf("%w: 金额不能为负", ErrInvalidAmount)
	}
	return nil
}

// IsZero 判断金额是否为零。
func (m Money) IsZero() bool { return m.Amount == 0 }

// SameCurrency 判断两笔金额币种是否相同。
func (m Money) SameCurrency(other Money) bool { return m.Currency == other.Currency }

// Add 两笔同币种金额相加，币种不同返回错误。
func (m Money) Add(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, fmt.Errorf("%w: %s 与 %s 不能混算", ErrInvalidAmount, m.Currency, other.Currency)
	}
	return Money{Currency: m.Currency, Amount: m.Amount + other.Amount}, nil
}

// String 以十进制展示，如 CNY 1234 -> "CNY 12.34"。
func (m Money) String() string {
	exp := currencyExponents[m.Currency]
	amt := m.Amount
	neg := ""
	if amt < 0 {
		neg = "-"
		amt = -amt
	}
	if exp == 0 {
		return fmt.Sprintf("%s %s%d", m.Currency, neg, amt)
	}
	div := int64(1)
	for i := 0; i < exp; i++ {
		div *= 10
	}
	return fmt.Sprintf("%s %s%d.%0*d", m.Currency, neg, amt/div, exp, amt%div)
}

// 业务错误。所有错误均为哨兵错误，调用方使用 errors.Is 分类判断。

var (
	// ErrInvalidAmount 金额非法（币种未知、负数、超过币种精度等），属于入参错误的子类。
	ErrInvalidAmount = fmt.Errorf("%w: invalid amount", ErrInvalidArgument)
	// ErrInvalidArgument 入参不合法（空商户、空明细等）。
	ErrInvalidArgument = errors.New("invalid argument")
	// ErrNotFound 应付款/批次/结算记录不存在。
	ErrNotFound = errors.New("not found")
	// ErrConflict 状态冲突：明细不可结算、批次状态不允许该操作等。
	ErrConflict = errors.New("conflict")
	// ErrIdempotencyConflict 同一外部提交号对应不同的批次或金额内容。
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	// ErrTerminalState 批次已处于终态（成功/失败/已取消），不能被旧操作或旧回执覆盖。
	ErrTerminalState = errors.New("terminal state")
)
