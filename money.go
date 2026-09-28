package gopayoutbatches

import (
	"errors"
	"strconv"
	"strings"
)

// 金额以最小货币单位（minor unit，如人民币“分”）表示，
// 全程使用整数运算，不引入 float，避免浮点误差。
// 最大支持到百亿（按分计约 9.2e18 以内）；Add 会检查溢出。

// Money 是一笔不可变的精确金额。字段可以直接读取，
// 但构造请使用 NewMoney / ParseMoney，以便做范围与币种校验。
type Money struct {
	// Amount 最小货币单位数量（必须 >= 0）。
	Amount int64
	// Currency ISO 4217 三字母币种代码，大写。
	Currency string
}

// NewMoney 构造一笔金额并做合法性检查。
func NewMoney(amount int64, currency string) (Money, error) {
	if amount < 0 {
		return Money{}, errors.New("money: amount must not be negative")
	}
	c, err := normalizeCurrency(currency)
	if err != nil {
		return Money{}, err
	}
	return Money{Amount: amount, Currency: c}, nil
}

// MustNewMoney 与 NewMoney 相同，但参数非法时 panic，适用于常量/测试。
func MustNewMoney(amount int64, currency string) Money {
	m, err := NewMoney(amount, currency)
	if err != nil {
		panic(err)
	}
	return m
}

// ParseMoney 解析十进制表示的金额，如 "12.34 CNY" / "12.34CNY" / "12"。
// 小数位最多两位（第三位非零即报错，如 12.345）；币种可带可不带，
// 不带时使用 defaultCurrency。
func ParseMoney(s, defaultCurrency string) (Money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, errors.New("money: empty amount")
	}

	// 从尾部切出字母币种部分。
	i := len(s)
	for i > 0 && isLetter(s[i-1]) {
		i--
	}
	numPart := strings.TrimSpace(s[:i])
	curPart := strings.TrimSpace(s[i:])
	if numPart == "" {
		return Money{}, errors.New("money: missing numeric part in " + strconv.Quote(s))
	}
	currency := curPart
	if currency == "" {
		currency = defaultCurrency
	}
	c, err := normalizeCurrency(currency)
	if err != nil {
		return Money{}, err
	}

	neg := false
	if numPart[0] == '-' || numPart[0] == '+' {
		neg = numPart[0] == '-'
		numPart = numPart[1:]
	}
	if numPart == "" {
		return Money{}, errors.New("money: invalid number " + strconv.Quote(s))
	}

	intPart, fracPart, _ := strings.Cut(numPart, ".")
	if intPart == "" && fracPart == "" {
		return Money{}, errors.New("money: invalid number " + strconv.Quote(s))
	}
	for _, r := range intPart {
		if r < '0' || r > '9' {
			return Money{}, errors.New("money: invalid digit in " + strconv.Quote(s))
		}
	}
	if len(fracPart) > 2 {
		// 超过最小货币单位精度（如 0.001）无法精确表示。
		return Money{}, errors.New("money: fractional precision exceeds minor unit in " + strconv.Quote(s))
	}
	for _, r := range fracPart {
		if r < '0' || r > '9' {
			return Money{}, errors.New("money: invalid digit in " + strconv.Quote(s))
		}
	}

	whole := int64(0)
	if intPart != "" {
		whole, err = strconv.ParseInt(intPart, 10, 64)
		if err != nil {
			return Money{}, errors.New("money: integer part out of range: " + err.Error())
		}
	}
	if whole > (1 << 62) {
		return Money{}, errors.New("money: amount out of range")
	}
	frac := int64(0)
	if fracPart != "" {
		v, err := strconv.ParseInt(fracPart, 10, 64)
		if err != nil {
			return Money{}, errors.New("money: fraction out of range: " + err.Error())
		}
		if len(fracPart) == 1 {
			frac = v * 10
		} else {
			frac = v
		}
	}
	amount := whole*100 + frac
	if neg {
		if amount > (1<<63 - 1) {
			return Money{}, errors.New("money: amount out of range")
		}
		amount = -amount
	}
	return NewMoney(amount, c)
}

// Add 返回 m+n，要求币种一致且不溢出；两者均不可变。
func (m Money) Add(n Money) (Money, error) {
	if m.Currency != n.Currency {
		return Money{}, errors.New("money: currency mismatch: " + m.Currency + " vs " + n.Currency)
	}
	sum := m.Amount + n.Amount
	if sum < m.Amount || sum < 0 { // int64 溢出或为负
		return Money{}, errors.New("money: amount overflow")
	}
	return Money{Amount: sum, Currency: m.Currency}, nil
}

// Equal 报告金额与币种是否完全一致。
func (m Money) Equal(n Money) bool { return m == n }

// IsPositive 报告金额是否大于 0。
func (m Money) IsPositive() bool { return m.Amount > 0 }

// String 返回如 "12.34 CNY" 的可读表示。
func (m Money) String() string {
	neg := m.Amount < 0
	a := m.Amount
	if neg {
		a = -a
	}
	s := strconv.FormatInt(a/100, 10) + "." + pad2(a%100)
	if neg {
		s = "-" + s
	}
	return s + " " + m.Currency
}

func normalizeCurrency(c string) (string, error) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 3 {
		return "", errors.New("money: currency must be a 3-letter ISO code, got " + strconv.Quote(c))
	}
	for _, r := range c {
		if !isLetter(byte(r)) {
			return "", errors.New("money: currency must be letters, got " + strconv.Quote(c))
		}
	}
	return c, nil
}

func isLetter(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func pad2(v int64) string {
	if v < 10 {
		return "0" + strconv.FormatInt(v, 10)
	}
	return strconv.FormatInt(v, 10)
}
