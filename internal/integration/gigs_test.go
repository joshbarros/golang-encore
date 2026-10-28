package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-encore/internal/gigs"
	"github.com/joshbarros/golang-encore/internal/platform/messaging"
)

// rawGig inserts a gig directly (below the API) and returns its id.
func (e *env) rawGig(seats, perFan int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO gigs.gigs (id, code, name, stage, band, gig_date, price_cents, seats_total, seats_left, max_per_fan, created_at)
		VALUES ($1,$2,'n','s','a',now(),5000,$3,$3,$4,now())`, id, code(), seats, perFan)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, gig, owner uuid.UUID, seats int) gigs.ReserveOutcome {
	out, err := e.app.Gigs.Reserve(context.Background(), uuid.New(), messaging.ReserveSeats{OrderID: uuid.New(), OwnerID: owner, GigID: gig, Seats: seats, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	gig := e.rawGig(30, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, gig, uuid.New(), 1) == gigs.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM gigs.gigs WHERE id = $1`, gig).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 fans, 30 seats: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM gigs.seat_holds WHERE gig_id = $1`, gig) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestTheSameFanRacingThemselvesCannotPassTheCap(t *testing.T) {
	e := newEnv(t)
	gig := e.rawGig(50, 2)
	owner := uuid.New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[gigs.ReserveOutcome]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := reserve(e, gig, owner, 1)
			mu.Lock()
			outcomes[o]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if outcomes[gigs.Held] != 2 || outcomes[gigs.CapReached] != 10 {
		t.Fatalf("a cap of 2 lets exactly 2 through: %v", outcomes)
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM gigs.gigs WHERE id = $1`, gig).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT seats FROM gigs.fans WHERE gig_id = $1 AND owner_id = $2`, gig, owner).Scan(&holding)
	if left != 48 || holding != 2 {
		t.Fatalf("refused tries leave no trace: %d left, fan holds %d", left, holding)
	}
}

func TestACapRefusalDoesNotConsumeStockAndAStockRefusalDoesNotConsumeAllowance(t *testing.T) {
	e := newEnv(t)
	gig := e.rawGig(3, 2)
	owner := uuid.New()
	if reserve(e, gig, owner, 3) != gigs.CapReached {
		t.Fatal("asking for more than the cap in one go is a cap refusal")
	}
	other := uuid.New()
	if reserve(e, gig, other, 2) != gigs.Held {
		t.Fatal("2 of 3 held")
	}
	if reserve(e, gig, owner, 2) != gigs.Short {
		t.Fatal("1 left, 2 wanted, within the cap: short")
	}
	if reserve(e, gig, owner, 1) != gigs.Held {
		t.Fatal("the allowance was not spent by the short try")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM gigs.gigs WHERE id = $1`, gig).Scan(&left)
	if left != 0 {
		t.Fatalf("all three are held: %d", left)
	}
}

func TestReleaseGivesBackStockAndAllowanceExactlyOnce(t *testing.T) {
	e := newEnv(t)
	gig := e.rawGig(5, 2)
	owner, order := uuid.New(), uuid.New()
	if out, _ := e.app.Gigs.Reserve(context.Background(), uuid.New(), messaging.ReserveSeats{OrderID: order, OwnerID: owner, GigID: gig, Seats: 2, Reference: "R"}); out != gigs.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		_, err := e.app.Gigs.Release(context.Background(), uuid.New(), messaging.ReleaseSeats{OrderID: order, Reason: "refund"})
		if err != nil {
			t.Fatal(err)
		}
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT seats_left FROM gigs.gigs WHERE id = $1`, gig).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT seats FROM gigs.fans WHERE gig_id = $1 AND owner_id = $2`, gig, owner).Scan(&holding)
	if left != 5 || holding != 0 {
		t.Fatalf("given back once, not three times: %d left, fan holds %d", left, holding)
	}
	if e.outbox("gigs", messaging.ReleasedKey) != 1 {
		t.Fatal("one released event")
	}
}

func TestOpeningAGigValidatesItsCap(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := map[string]any{"code": code(), "name": "n", "stage": "s", "band": "a", "priceCents": 100, "seatsTotal": 10}
	if r := e.post("/api/gigs", good, adm.Token); r.Status != 201 || r.Body["maxPerFan"] != float64(4) {
		t.Fatalf("the cap defaults to 4 (or the seats on sale, if fewer): %d %s", r.Status, r.Raw)
	}
	bad := map[string]any{"code": code(), "name": "n", "stage": "s", "band": "a", "priceCents": 100, "seatsTotal": 10, "maxPerFan": 11}
	if r := e.post("/api/gigs", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap above the seats on sale: %d", r.Status)
	}
	bad["maxPerFan"] = 0
	if r := e.post("/api/gigs", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap of zero: %d", r.Status)
	}
	if r := e.post("/api/gigs", good, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/gigs", good, e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a gig: %d", r.Status)
	}
}
