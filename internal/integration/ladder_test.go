package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-encore/internal/platform/messaging"
)

func (e *env) holdSubtotals(gig string) []int64 {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT h.subtotal_cents FROM gigs.seat_holds h JOIN gigs.gigs g ON g.id = h.gig_id WHERE g.code = $1 ORDER BY h.subtotal_cents`, gig)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestTwentyConcurrentSingleSeatOrdersPayTheLadderExactlyOnce(t *testing.T) {
	e := newEnv(t)
	gig := e.openGigCap(20, 4)
	var gigID uuid.UUID
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM gigs.gigs WHERE code = $1`, gig).Scan(&gigID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := messaging.ReserveSeats{OrderID: uuid.New(), OwnerID: uuid.New(), GigID: gigID, Seats: 1, Reference: "R"}
			if _, err := e.app.Gigs.Reserve(context.Background(), uuid.New(), cmd); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got := e.holdSubtotals(gig)
	counts := map[int64]int{}
	for _, v := range got {
		counts[v]++
	}
	if len(got) != 20 || counts[4000] != 5 || counts[5000] != 10 || counts[6250] != 5 {
		t.Fatalf("twenty seats: five at 80%%, ten at the list price, five at 125%%, each place paid once: %v", counts)
	}
}

func TestAnOrderSpanningAStepPaysEachSeatItsOwnPriceAndTheTotalIncludesTheFee(t *testing.T) {
	e := newEnv(t)
	gig := e.openGigCap(20, 6)
	ana, bob := e.register(), e.register()
	first := e.enter(ana, gig, "FAN-A", 4).str("id") // seats 0-3: all early bird, 4 x 4000
	e.drain()
	second := e.enter(bob, gig, "FAN-B", 3).str("id") // seats 4-6: one early bird (4000) and two standard (5000)
	e.drain()
	a, b := e.order(ana, first), e.order(bob, second)
	if a.Body["totalCents"] != float64(17920) { // 16000 + 12%
		t.Fatalf("the first order: %s", a.Raw)
	}
	if b.Body["totalCents"] != float64(15680) { // 14000 + 12%
		t.Fatalf("the second order spans the step: %s", b.Raw)
	}
	g := e.get("/api/gigs/"+gig, ana.Token)
	if g.Body["nextSeatPriceCents"] != float64(5000) || g.Body["tier"] != "Standard" {
		t.Fatalf("the next seat is a standard one: %s", g.Raw)
	}
}

func TestARefundPutsTheSeatsBackAndTheNextBuyerPaysThePriceOfThosePlaces(t *testing.T) {
	e := newEnv(t)
	gig := e.openGigCap(20, 4)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, gig, "FAN-A", 2).str("id") // two early-bird seats
	e.drain()
	if r := e.post("/api/orders/"+a+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	b := e.enter(bob, gig, "FAN-B", 2).str("id")
	e.drain()
	if o := e.order(bob, b); o.Body["totalCents"] != float64(8960) { // 2 x 4000 + 12%: the early-bird places are on sale again
		t.Fatalf("the next buyer pays the early-bird price again: %s", o.Raw)
	}
}

func TestTheNextSeatPriceAndTierFollowTheSeatsSold(t *testing.T) {
	e := newEnv(t)
	gig := e.openGigCap(4, 4)
	ana := e.register()
	check := func(price float64, tier string) {
		t.Helper()
		g := e.get("/api/gigs/"+gig, ana.Token)
		if g.Body["nextSeatPriceCents"] != price || g.Body["tier"] != tier {
			t.Fatalf("expected %v at %s: %s", price, tier, g.Raw)
		}
	}
	check(4000, "EarlyBird")
	e.enter(ana, gig, "FAN-1", 1)
	e.drain()
	check(5000, "Standard")
	e.enter(ana, gig, "FAN-2", 2)
	e.drain()
	check(6250, "LastSeats")
	e.enter(ana, gig, "FAN-3", 1)
	e.drain()
	g := e.get("/api/gigs/"+gig, ana.Token)
	if g.Body["nextSeatPriceCents"] != nil || g.Body["tier"] != nil {
		t.Fatalf("sold out: no next seat: %s", g.Raw)
	}
	subtotals := e.holdSubtotals(gig)
	if len(subtotals) != 3 || subtotals[0] != 4000 || subtotals[1] != 6250 || subtotals[2] != 10000 {
		t.Fatalf("one early-bird seat, one last seat and two standard seats in one order: %v", subtotals)
	}
}
