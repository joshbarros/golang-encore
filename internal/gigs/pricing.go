package gigs

import "errors"

// PriceTier is the step a seat's place in the line falls in.
type PriceTier string

// The three steps of a gig.
const (
	EarlyBird PriceTier = "EarlyBird"
	Standard  PriceTier = "Standard"
	LastSeats PriceTier = "LastSeats"
)

// Seat prices step with how many seats have been sold: the first quarter of a gig sells at 80% of the list price, the last
// quarter at 125%, the rest at the list price. The price belongs to the seat's place in the line, so an order that spans a
// step pays each seat its own price.
const (
	EarlyBirdPercent = 80
	LastSeatsPercent = 125
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// EarlyBirdSeats is how many seats (counted from the first sold) are in the early-bird step.
func EarlyBirdSeats(seatsTotal int) int { return seatsTotal / 4 }

// LastSeatsFrom is the index (0-based, among all seats sold) at which the last-seats step begins.
func LastSeatsFrom(seatsTotal int) int { return seatsTotal - seatsTotal/4 }

// TierOf is the step the seat at soldIndex falls in.
func TierOf(seatsTotal, soldIndex int) PriceTier {
	switch {
	case soldIndex < EarlyBirdSeats(seatsTotal):
		return EarlyBird
	case soldIndex >= LastSeatsFrom(seatsTotal):
		return LastSeats
	}
	return Standard
}

// SeatPriceCents is one seat's price in its step.
func SeatPriceCents(listCents int64, tier PriceTier) (int64, error) {
	switch tier {
	case EarlyBird:
		return scale(listCents, EarlyBirdPercent)
	case LastSeats:
		return scale(listCents, LastSeatsPercent)
	}
	return listCents, nil
}

func scale(listCents int64, percent int64) (int64, error) {
	if listCents < 0 || listCents > (1<<62)/percent {
		return 0, ErrOverflow
	}
	return listCents * percent / 100, nil
}

// SubtotalCents is what the seats numbered soldBefore .. soldBefore+seats-1 cost together, before the service fee.
func SubtotalCents(listCents int64, seatsTotal, soldBefore, seats int) (int64, error) {
	var sum int64
	for i := soldBefore; i < soldBefore+seats; i++ {
		p, err := SeatPriceCents(listCents, TierOf(seatsTotal, i))
		if err != nil {
			return 0, err
		}
		if sum > (1<<62)-p {
			return 0, ErrOverflow
		}
		sum += p
	}
	return sum, nil
}

// NextSeat is the price the next seat sells at, given how many are left. ok is false when the gig is sold out.
func NextSeat(listCents int64, seatsTotal, seatsLeft int) (price int64, tier PriceTier, ok bool) {
	if seatsLeft <= 0 {
		return 0, "", false
	}
	tier = TierOf(seatsTotal, seatsTotal-seatsLeft)
	price, err := SeatPriceCents(listCents, tier)
	return price, tier, err == nil
}
