package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-encore/internal/passes"
	"github.com/joshbarros/golang-encore/internal/platform/messaging"
)

func (e *env) outbox(schema, key string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+schema+`.outbox_messages WHERE routing_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) count(sql string, args ...any) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) seatsLeft(gigCode string) float64 {
	e.t.Helper()
	v, _ := e.get("/api/gigs/"+gigCode, e.admin().Token).Body["seatsLeft"].(float64)
	return v
}

func status(e *env, s session, id string) string { return e.order(s, id).str("status") }

func TestTheWholeSagaFromOrderToSoldAndThePass(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(10)
	ana := e.register()
	created := e.enter(ana, gig, "FAN-1", 2)
	id := created.str("id")
	if created.Status != 202 || created.str("status") != "Requested" || created.Body["totalCents"] != float64(11200) {
		t.Fatalf("accepted as Requested with the total, fee included: %s", created.Raw)
	}
	if dead := e.drain(); dead != 0 {
		t.Fatalf("nothing should dead-letter: %d", dead)
	}
	o := e.order(ana, id)
	if o.str("status") != "Sold" || !strings.HasPrefix(o.str("passNumber"), "EN-") || o.Body["reason"] != nil {
		t.Fatalf("sold with a pass number and no leftover reason: %s", o.Raw)
	}
	if e.seatsLeft(gig) != 8 {
		t.Fatal("2 of 10 seats are held")
	}
	c := e.get("/api/passes/"+id, ana.Token)
	if c.Status != 200 || c.str("status") != "Issued" || c.str("number") != o.str("passNumber") {
		t.Fatalf("the pass: %d %s", c.Status, c.Raw)
	}
}

func TestStockRunsOutAndTheLateOrderIsToldHowManyAreLeft(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(3)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, gig, "FAN-A", 2).str("id")
	e.drain()
	b := e.enter(bob, gig, "FAN-B", 2).str("id")
	e.drain()
	if status(e, ana, a) != "Sold" {
		t.Fatal("the first order is sold")
	}
	o := e.order(bob, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Short: 2 seats wanted, 1 left." {
		t.Fatalf("the second is short: %s", o.Raw)
	}
	if e.get("/api/passes/"+b, bob.Token).Status != 404 {
		t.Fatal("no pass for a refused order")
	}
	if e.seatsLeft(gig) != 1 {
		t.Fatal("the refused order took nothing")
	}
}

func TestOneFanCannotHoldMoreThanTheCapAndARefusedTryCostsNoAllowance(t *testing.T) {
	e := newEnv(t)
	gig := e.openGigCap(10, 2)
	ana := e.register()
	a := e.enter(ana, gig, "FAN-1", 2).str("id")
	e.drain()
	b := e.enter(ana, gig, "FAN-2", 1).str("id")
	e.drain()
	if status(e, ana, a) != "Sold" {
		t.Fatal("up to the cap is fine")
	}
	o := e.order(ana, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Cap: at most 2 seats per fan in this gig." {
		t.Fatalf("over the cap: %s", o.Raw)
	}
	if e.seatsLeft(gig) != 8 {
		t.Fatal("the capped order took no stock")
	}
	// the refund gives the allowance back: the same fan can order again
	if r := e.post("/api/orders/"+a+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	c := e.enter(ana, gig, "FAN-3", 2).str("id")
	e.drain()
	if status(e, ana, c) != "Sold" || e.seatsLeft(gig) != 8 {
		t.Fatalf("after the refund the allowance is back: %s", status(e, ana, c))
	}
}

func TestPoisonIsRejectedAndTheSeatsComeBack(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(5)
	ana := e.register()
	id := e.enter(ana, gig, "FAN-POISON-1", 2).str("id")
	if dead := e.drain(); dead != 1 {
		t.Fatalf("the poison pass is dead-lettered once: %d", dead)
	}
	o := e.order(ana, id)
	if o.str("status") != "Rejected" || !strings.Contains(o.str("reason"), "poison") || o.Body["seatsReleasedAt"] == nil {
		t.Fatalf("rejected, with the reason, and the release recorded: %s", o.Raw)
	}
	if e.seatsLeft(gig) != 5 {
		t.Fatal("the seats are back")
	}
	if c := e.get("/api/passes/"+id, ana.Token); c.str("status") != "Rejected" {
		t.Fatalf("the rejection is committed on the pass: %s", c.Raw)
	}
	// the fan's allowance came back with them
	again := e.enter(ana, gig, "FAN-2", 2).str("id")
	e.drain()
	if status(e, ana, again) != "Sold" {
		t.Fatal("the allowance is not stuck behind the poison order")
	}
}

func TestRefundAfterSaleReturnsTheSeatsButKeepsThePass(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(5)
	ana := e.register()
	id := e.enter(ana, gig, "FAN-1", 2).str("id")
	e.drain()
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Refunded" || e.seatsLeft(gig) != 5 {
		t.Fatal("refunded and the seats are back on the shelf")
	}
	if c := e.get("/api/passes/"+id, ana.Token); c.str("status") != "Issued" {
		t.Fatal("a refund does not roll back the pass that was issued")
	}
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a second refund is a conflict: %d", r.Status)
	}
}

func TestRefundBeatsAFinalMarkThatArrivesLate(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, gig, "FAN-1", 1).str("id"))
	// run the reserve and the pass, but stop before the order sees pass.issued
	for pass := 0; pass < 4; pass++ {
		for _, row := range e.unsent() {
			if row.d.RoutingKey == messaging.IssuedKey {
				continue
			}
			e.markSent(row)
			_, _ = e.deliver(row.d.RoutingKey, row.d)
		}
	}
	if got := status(e, ana, id.String()); got != "Held" {
		t.Fatalf("held, pass issued, final mark not yet landed: %s", got)
	}
	if r := e.post("/api/orders/"+id.String()+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatal(r.Status)
	}
	e.drain() // now the final mark arrives
	o := e.order(ana, id.String())
	if o.str("status") != "Refunded" || o.Body["passNumber"] != nil {
		t.Fatalf("the late final mark must not undo the refund: %s", o.Raw)
	}
	if c := e.get("/api/passes/"+id.String(), ana.Token); c.str("status") != "Issued" {
		t.Fatal("the issued pass stays")
	}
}

func TestADeliveryRepeatedByTheBrokerHasNoEffectTheSecondTime(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(5)
	ana := e.register()
	e.enter(ana, gig, "FAN-1", 1)
	rows := e.unsent()
	if len(rows) != 1 {
		t.Fatal("one reserve command")
	}
	d := rows[0].d
	for i := 0; i < 3; i++ {
		if _, err := e.deliver(d.RoutingKey, d); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM gigs.seat_holds`) != 1 || e.outbox("gigs", messaging.HeldKey) != 1 || e.seatsLeft(gig) != 4 {
		t.Fatal("at-least-once delivery, exactly-once effect: the inbox absorbs the repeats")
	}
}

func TestAFinalMarkBeforeTheHeldMarkIsRetriedNotGigped(t *testing.T) {
	e := newEnv(t)
	gig := e.openGig(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, gig, "FAN-1", 1).str("id"))
	// the pass is issued but the Held mark has not reached the order: the order is still Requested
	_, err := e.app.Orders.MarkSold(context.Background(), uuid.New(), messaging.PassIssued{OrderID: id, Number: "EN-TEST"}, 0)
	var ooo *messaging.OutOfOrderError
	if err == nil || !errorsAs(err, &ooo) {
		t.Fatalf("the final mark must ask to be retried, got %v", err)
	}
	if status(e, ana, id.String()) != "Requested" {
		t.Fatal("and change nothing")
	}
	e.drain()
	if status(e, ana, id.String()) != "Sold" {
		t.Fatal("the normal flow still completes")
	}
}

func TestIssueIsIdempotentAndPoisonIsAFlagNotAnException(t *testing.T) {
	e := newEnv(t)
	id, owner := uuid.New(), uuid.New()
	msg := uuid.New()
	held := messaging.SeatsHeld{OrderID: id, OwnerID: owner, GigID: uuid.New(), Seats: 1, Reference: "FAN-1"}
	first, err := e.app.Passes.Issue(context.Background(), msg, held)
	again, err2 := e.app.Passes.Issue(context.Background(), msg, held)
	if err != nil || err2 != nil || first != passes.Issued || again != passes.Duplicate {
		t.Fatalf("issue then duplicate: %v %v %v %v", first, err, again, err2)
	}
	poison := messaging.SeatsHeld{OrderID: uuid.New(), OwnerID: owner, GigID: uuid.New(), Seats: 1, Reference: "xPOISONx"}
	if out, _ := e.app.Passes.Issue(context.Background(), uuid.New(), poison); out != passes.Rejected {
		t.Fatal("a poison reference is rejected")
	}
	if e.outbox("passes", messaging.ReleaseKey) != 1 || e.outbox("passes", messaging.RejectedKey) != 1 || e.outbox("passes", messaging.IssuedKey) != 1 {
		t.Fatal("one issued, one release command and one rejection")
	}
}
