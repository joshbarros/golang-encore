// Package orders is the HTTP side of the saga and the handler that moves an order forward as events arrive.
package orders

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Order rules.
const (
	MaxSeatsPerOrder      = 6
	MaxReferenceLength    = 40
	ServiceFeeBasisPoints = 1_200
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// Validate returns an error message, or "" when the request is acceptable. The seat count is compared as int64 on purpose:
// narrowing 4294967297 to int32 would wrap to 1 and pass.
func Validate(reference string, seats int64) string {
	trimmed := strings.TrimSpace(reference)
	switch {
	case trimmed == "":
		return "Fan reference is required."
	case len([]rune(trimmed)) > MaxReferenceLength:
		return fmt.Sprintf("Fan reference must be at most %d characters.", MaxReferenceLength)
	case seats < 1:
		return "An order needs at least 1 seat."
	case seats > MaxSeatsPerOrder:
		return fmt.Sprintf("An order takes at most %d seats.", MaxSeatsPerOrder)
	}
	return ""
}

// WithFee is the subtotal plus the 12% service fee, truncated to a whole cent. Overflow is an error, not a wrap.
func WithFee(subtotalCents int64) (int64, error) {
	if subtotalCents < 0 || subtotalCents > math.MaxInt64/ServiceFeeBasisPoints {
		return 0, ErrOverflow
	}
	fee := subtotalCents * ServiceFeeBasisPoints / 10_000
	if subtotalCents > math.MaxInt64-fee {
		return 0, ErrOverflow
	}
	return subtotalCents + fee, nil
}

// TotalCents is face value plus a 12% service fee truncated to a whole cent. Overflow is an error, not a wrap.
func TotalCents(priceCents int64, seats int) (int64, error) {
	if priceCents < 0 || seats < 0 {
		return 0, ErrOverflow
	}
	if seats != 0 && priceCents > math.MaxInt64/int64(seats) {
		return 0, ErrOverflow
	}
	face := priceCents * int64(seats)
	if face > math.MaxInt64/ServiceFeeBasisPoints {
		return 0, ErrOverflow
	}
	fee := face * ServiceFeeBasisPoints / 10_000
	if face > math.MaxInt64-fee {
		return 0, ErrOverflow
	}
	return face + fee, nil
}
