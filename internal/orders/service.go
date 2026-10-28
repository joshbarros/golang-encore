package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-encore/internal/gigs"
	"github.com/joshbarros/golang-encore/internal/platform/httpx"
	"github.com/joshbarros/golang-encore/internal/platform/messaging"
	"github.com/joshbarros/golang-encore/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "orders"
	ConsumerName = "orders"
)

// Order statuses.
const (
	Requested = "Requested"
	Held      = "Held"
	Sold      = "Sold"
	Refunded  = "Refunded"
	Rejected  = "Rejected"
)

// Order is the public view of an order.
type Order struct {
	ID              uuid.UUID  `json:"id"`
	OwnerID         uuid.UUID  `json:"ownerId"`
	GigCode         string     `json:"gigCode"`
	Reference       string     `json:"reference"`
	Seats           int        `json:"seats"`
	TotalCents      int64      `json:"totalCents"`
	Status          string     `json:"status"`
	Reason          *string    `json:"reason"`
	PassNumber      *string    `json:"passNumber"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	SeatsReleasedAt *time.Time `json:"seatsReleasedAt"`
}

// GigCatalog is the one thing Orders may ask the gigs module.
type GigCatalog interface {
	Find(ctx context.Context, code string) (*gigs.Info, error)
}

// Service is the HTTP side of the saga: it writes the order and its first outbox row in one local transaction.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Gigs   GigCatalog
	Log    *slog.Logger

	// FinalMarkDelay is a latency knob for the final mark: Kubernetes sets it so the refund race window is wide enough to hit on purpose.
	FinalMarkDelay time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, catalog GigCatalog, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "orders"}, Gigs: catalog, Log: log}
}

const orderColumns = `id, owner_id, gig_code, reference, seats, total_cents, status, reason, pass_number, created_at, updated_at, seats_released_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.OwnerID, &o.GigCode, &o.Reference, &o.Seats, &o.TotalCents, &o.Status, &o.Reason, &o.PassNumber, &o.CreatedAt, &o.UpdatedAt, &o.SeatsReleasedAt)
	return o, err
}

// CreateRequest is the body of POST /api/orders.
type CreateRequest struct {
	GigCode   string `json:"gigCode"`
	Reference string `json:"reference"`
	Seats     int64  `json:"seats"`
}

// Create reserves seats in the gig for the caller: validates, writes the order and its reserve command in one transaction.
func (s *Service) Create(ctx context.Context, caller *httpx.Principal, req CreateRequest) (Order, error) {
	if caller == nil {
		return Order{}, httpx.Fail(http.StatusUnauthorized, "Sign in to book.")
	}
	if strings.TrimSpace(req.GigCode) == "" {
		return Order{}, httpx.Fail(http.StatusBadRequest, "Gig code is required.")
	}
	if msg := Validate(req.Reference, req.Seats); msg != "" {
		return Order{}, httpx.Fail(http.StatusBadRequest, "%s", msg)
	}
	gig, err := s.Gigs.Find(ctx, strings.TrimSpace(req.GigCode))
	if err != nil {
		return Order{}, err
	}
	if gig == nil {
		return Order{}, httpx.Fail(http.StatusNotFound, "Gig '%s' does not exist.", req.GigCode)
	}
	now := time.Now().UTC()
	seats := int(req.Seats)
	reference := strings.TrimSpace(req.Reference)
	total, err := TotalCents(gig.PriceCents, seats)
	if err != nil {
		return Order{}, httpx.Fail(http.StatusBadRequest, "The total is too large.")
	}
	id, _ := uuid.NewV7()
	o := Order{ID: id, OwnerID: caller.ID, GigCode: gig.Code, Reference: reference, Seats: seats, TotalCents: total, Status: Requested, CreatedAt: now, UpdatedAt: now}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO orders.orders (id, owner_id, gig_id, gig_code, reference, seats, total_cents, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'Requested',$8,$8)`, o.ID, o.OwnerID, gig.ID, o.GigCode, o.Reference, o.Seats, o.TotalCents, now); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReserveKey, o.ID,
			messaging.ReserveSeats{OrderID: o.ID, OwnerID: caller.ID, GigID: gig.ID, Seats: seats, Reference: reference})
	})
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return Order{}, httpx.Fail(http.StatusConflict, "Reference '%s' is already booked on %s.", reference, gig.Code)
		}
		return Order{}, err
	}
	return o, nil
}

// Get returns an order to its owner or to staff. Anyone else gets a 404, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	o, err := scanOrder(s.Pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders.orders WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !caller.CanAccess(o.OwnerID)) {
		return Order{}, httpx.Fail(http.StatusNotFound, "Order not found.")
	}
	return o, err
}

// List returns the caller's orders (staff: everyone's), newest first, a page at a time, with the total.
func (s *Service) List(ctx context.Context, caller *httpx.Principal, gigCode string, page, pageSize int) ([]Order, int, error) {
	if caller == nil {
		return nil, 0, httpx.Fail(http.StatusUnauthorized, "Sign in to see orders.")
	}
	where := []string{"true"}
	args := []any{}
	if !caller.IsStaff() {
		args = append(args, caller.ID)
		where = append(where, "owner_id = $"+strconv.Itoa(len(args)))
	}
	if strings.TrimSpace(gigCode) != "" {
		args = append(args, gigCode)
		where = append(where, "gig_code = $"+strconv.Itoa(len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM orders.orders WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM orders.orders WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		orderColumns, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
	}
	return items, total, rows.Err()
}

// Refund moves Held or Sold to Refunded and asks Gigs to give the seats back. The pass is not touched.
func (s *Service) Refund(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	if _, err := s.Get(ctx, caller, id); err != nil { // 404 unless the caller may see it
		return Order{}, err
	}
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Refunded', reason = 'Refunded by request.', updated_at = now()
			WHERE id = $1 AND status IN ('Held','Sold')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var current string
			if err := tx.QueryRow(ctx, `SELECT status FROM orders.orders WHERE id = $1`, id).Scan(&current); err != nil {
				return err
			}
			return httpx.Fail(http.StatusConflict, "An order in status %s cannot be refunded.", current)
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, id, messaging.ReleaseSeats{OrderID: id, Reason: "refund"})
	})
	if err != nil {
		return Order{}, err
	}
	return scanOrder(s.Pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders.orders WHERE id = $1`, id))
}
