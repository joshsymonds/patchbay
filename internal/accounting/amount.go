// Package accounting provides exact arithmetic for provider-reported USD amounts.
package accounting

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const (
	nanoUSDPerUSD         NanoUSD = 1000000000
	nanoUSDPerCent                = nanoUSDPerUSD / 100
	tokensPerMillion      int64   = 1000000
	halfDivisor           int64   = 2
	nanoUSDDecimalPlaces  int64   = 9
	maxInt64DecimalDigits         = 19
	maxNanoUSD            NanoUSD = 1<<63 - 1
	minNanoUSD            NanoUSD = -1 << 63
)

// NanoUSD is a signed number of billionths of one US dollar.
type NanoUSD int64

type providerDecimal struct {
	digits           string
	fractionalDigits int
	exponent         *big.Int
}

// ParseUSD parses a nonnegative JSON number and rounds it to nano-USD using half-even rounding.
func ParseUSD(text string) (NanoUSD, error) {
	decimal, err := parseProviderDecimal(text)
	if err != nil {
		return 0, err
	}

	digits := strings.TrimLeft(decimal.digits, "0")
	if digits == "" {
		return 0, nil
	}

	power := new(big.Int).Set(decimal.exponent)
	power.Add(power, big.NewInt(nanoUSDDecimalPlaces-int64(decimal.fractionalDigits)))

	amount, err := decimalToNanoUSD(digits, power)
	if err != nil {
		return 0, fmt.Errorf("parse USD %q: %w", text, err)
	}

	return amount, nil
}

// Add returns the exact sum of two nano-USD amounts or an error if the sum overflows.
func Add(left, right NanoUSD) (NanoUSD, error) {
	if right > 0 && left > maxNanoUSD-right {
		return 0, errors.New("add nano-USD: overflow")
	}

	if right < 0 && left < minNanoUSD-right {
		return 0, errors.New("add nano-USD: overflow")
	}

	return left + right, nil
}

// MultiplyPerMillion multiplies a nano-USD-per-million-token rate by a token count using half-even rounding.
func MultiplyPerMillion(rate NanoUSD, tokens int64) (NanoUSD, error) {
	if rate < 0 {
		return 0, errors.New("multiply per million: rate must be nonnegative")
	}

	if tokens < 0 {
		return 0, errors.New("multiply per million: tokens must be nonnegative")
	}

	product := new(big.Int).Mul(big.NewInt(int64(rate)), big.NewInt(tokens))
	divisor := big.NewInt(tokensPerMillion)
	quotient := new(big.Int)
	remainder := new(big.Int)
	quotient.QuoRem(product, divisor, remainder)
	roundPositiveHalfEven(quotient, remainder, divisor)

	if !quotient.IsInt64() {
		return 0, errors.New("multiply per million: overflow")
	}

	return NanoUSD(quotient.Int64()), nil
}

// RoundCents rounds a signed nano-USD amount to cents using half-even rounding.
func RoundCents(amount NanoUSD) int64 {
	divisor := int64(nanoUSDPerCent)
	quotient := int64(amount) / divisor
	remainder := int64(amount) % divisor
	half := divisor / halfDivisor

	if remainder > half || remainder == half && quotient%2 != 0 {
		return quotient + 1
	}

	if remainder < -half || remainder == -half && quotient%2 != 0 {
		return quotient - 1
	}

	return quotient
}

// TruncateCents truncates a signed nano-USD amount to whole cents toward zero.
func TruncateCents(amount NanoUSD) int64 {
	return int64(amount) / int64(nanoUSDPerCent)
}

func parseProviderDecimal(text string) (providerDecimal, error) {
	if text == "" {
		return providerDecimal{}, errors.New("parse USD: empty input")
	}

	if text[0] == '-' {
		return providerDecimal{}, errors.New("parse USD: negative values are not allowed")
	}

	position := 0
	integerEnd, ok := consumeInteger(text, &position)
	if !ok {
		return providerDecimal{}, fmt.Errorf("parse USD %q: invalid JSON number", text)
	}

	fraction := ""
	if position < len(text) && text[position] == '.' {
		position++
		fractionStart := position
		consumeDigits(text, &position)
		if position == fractionStart {
			return providerDecimal{}, fmt.Errorf("parse USD %q: invalid JSON number", text)
		}

		fraction = text[fractionStart:position]
	}

	exponent := new(big.Int)
	if position < len(text) && (text[position] == 'e' || text[position] == 'E') {
		var exponentOK bool
		exponent, exponentOK = consumeExponent(text, &position)
		if !exponentOK {
			return providerDecimal{}, fmt.Errorf("parse USD %q: invalid JSON number", text)
		}
	}

	if position != len(text) {
		return providerDecimal{}, fmt.Errorf("parse USD %q: invalid JSON number", text)
	}

	return providerDecimal{
		digits:           text[:integerEnd] + fraction,
		fractionalDigits: len(fraction),
		exponent:         exponent,
	}, nil
}

func consumeInteger(text string, position *int) (int, bool) {
	if text[*position] == '0' {
		*position++
		if *position < len(text) && isDigit(text[*position]) {
			return 0, false
		}

		return *position, true
	}

	if !isNonzeroDigit(text[*position]) {
		return 0, false
	}

	consumeDigits(text, position)

	return *position, true
}

func consumeExponent(text string, position *int) (*big.Int, bool) {
	*position++
	negative := false
	if *position < len(text) && (text[*position] == '+' || text[*position] == '-') {
		negative = text[*position] == '-'
		*position++
	}

	exponentStart := *position
	consumeDigits(text, position)
	if *position == exponentStart {
		return nil, false
	}

	exponent, ok := new(big.Int).SetString(text[exponentStart:*position], 10)
	if !ok {
		return nil, false
	}

	if negative {
		exponent.Neg(exponent)
	}

	return exponent, true
}

func consumeDigits(text string, position *int) {
	for *position < len(text) && isDigit(text[*position]) {
		*position++
	}
}

func isDigit(character byte) bool {
	return character >= '0' && character <= '9'
}

func isNonzeroDigit(character byte) bool {
	return character >= '1' && character <= '9'
}

func decimalToNanoUSD(digits string, power *big.Int) (NanoUSD, error) {
	if power.Sign() >= 0 {
		return scaledIntegerNanoUSD(digits, power)
	}

	droppedDigits := new(big.Int).Neg(power)
	if !droppedDigits.IsInt64() || droppedDigits.Int64() > int64(len(digits)) {
		return 0, nil
	}

	dropCount := int(droppedDigits.Int64())
	keptCount := len(digits) - dropCount
	if keptCount > maxInt64DecimalDigits {
		return 0, errors.New("nano-USD overflow")
	}

	amount := new(big.Int)
	if keptCount > 0 {
		if _, ok := amount.SetString(digits[:keptCount], 10); !ok {
			return 0, errors.New("invalid decimal digits")
		}
	}

	if shouldRoundDecimalUp(digits[keptCount:], amount) {
		amount.Add(amount, big.NewInt(1))
	}

	return checkedNanoUSD(amount)
}

func scaledIntegerNanoUSD(digits string, power *big.Int) (NanoUSD, error) {
	if !power.IsInt64() || power.Int64() > maxInt64DecimalDigits {
		return 0, errors.New("nano-USD overflow")
	}

	scale := int(power.Int64())
	if len(digits)+scale > maxInt64DecimalDigits {
		return 0, errors.New("nano-USD overflow")
	}

	amount, ok := new(big.Int).SetString(digits, 10)
	if !ok {
		return 0, errors.New("invalid decimal digits")
	}

	if scale > 0 {
		factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
		amount.Mul(amount, factor)
	}

	return checkedNanoUSD(amount)
}

func shouldRoundDecimalUp(dropped string, amount *big.Int) bool {
	if dropped[0] > '5' {
		return true
	}

	if dropped[0] < '5' {
		return false
	}

	if strings.IndexFunc(dropped[1:], func(character rune) bool { return character != '0' }) >= 0 {
		return true
	}

	return amount.Bit(0) == 1
}

func roundPositiveHalfEven(quotient, remainder, divisor *big.Int) {
	twiceRemainder := new(big.Int).Lsh(new(big.Int).Set(remainder), 1)
	comparison := twiceRemainder.Cmp(divisor)
	if comparison > 0 || comparison == 0 && quotient.Bit(0) == 1 {
		quotient.Add(quotient, big.NewInt(1))
	}
}

func checkedNanoUSD(amount *big.Int) (NanoUSD, error) {
	if !amount.IsInt64() {
		return 0, errors.New("nano-USD overflow")
	}

	return NanoUSD(amount.Int64()), nil
}
