package gigs

import "testing"

func TestTheStepsAreAQuarterAtEightyPercentAndAQuarterAtAHundredAndTwentyFive(t *testing.T) {
	// a gig of 20: seats 0-4 early bird, 5-14 standard, 15-19 last seats
	counts := map[PriceTier]int{}
	for i := 0; i < 20; i++ {
		counts[TierOf(20, i)]++
	}
	if counts[EarlyBird] != 5 || counts[Standard] != 10 || counts[LastSeats] != 5 {
		t.Fatalf("5 / 10 / 5: %v", counts)
	}
	for tier, want := range map[PriceTier]int64{EarlyBird: 4000, Standard: 5000, LastSeats: 6250} {
		got, err := SeatPriceCents(5000, tier)
		if err != nil || got != want {
			t.Errorf("%s: %d, %v; want %d", tier, got, err, want)
		}
	}
}

func TestAnOrderThatSpansAStepPaysEachSeatItsOwnPrice(t *testing.T) {
	// seats 3,4 are early bird (4000 each), seat 5 is standard (5000)
	got, err := SubtotalCents(5000, 20, 3, 3)
	if err != nil || got != 4000+4000+5000 {
		t.Fatalf("%d, %v", got, err)
	}
	if all, _ := SubtotalCents(5000, 20, 0, 20); all != 5*4000+10*5000+5*6250 {
		t.Fatalf("selling every seat pays the whole ladder once: %d", all)
	}
}

func TestSmallGigsRoundTheStepsDown(t *testing.T) {
	for _, total := range []int{1, 2, 3} {
		if EarlyBirdSeats(total) != 0 || LastSeatsFrom(total) != total {
			t.Errorf("a gig of %d has no steps: %d %d", total, EarlyBirdSeats(total), LastSeatsFrom(total))
		}
	}
	if EarlyBirdSeats(4) != 1 || LastSeatsFrom(4) != 3 {
		t.Fatal("a gig of 4: one seat at each end")
	}
}

func TestNextSeatSaysWhatTheNextBuyerPays(t *testing.T) {
	price, tier, ok := NextSeat(5000, 20, 20)
	if !ok || price != 4000 || tier != EarlyBird {
		t.Fatalf("first seat: %d %s", price, tier)
	}
	price, tier, ok = NextSeat(5000, 20, 1)
	if !ok || price != 6250 || tier != LastSeats {
		t.Fatalf("last seat: %d %s", price, tier)
	}
	if _, _, ok := NextSeat(5000, 20, 0); ok {
		t.Fatal("sold out: no next seat")
	}
}

func TestPricingOverflowIsAnErrorNotAWrap(t *testing.T) {
	if _, err := SubtotalCents(1<<62, 20, 15, 5); err == nil {
		t.Fatal("must overflow")
	}
	if _, err := SeatPriceCents(-1, EarlyBird); err == nil {
		t.Fatal("negative price")
	}
}
