package gigs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-encore/internal/platform/messaging"
	"github.com/joshbarros/golang-encore/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "gigs"
	ConsumerName = "gigs"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	CapReached
	Duplicate
)

// Service is the gigs module's logic. It is the only writer of seats_left and of the per-fan counts.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "gigs"}, Log: log}
}

// Handle implements messaging.Handler for the gigs queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReserveSeats](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleaseSeats](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("gigs does not handle %q", d.RoutingKey)
}

var errRefused = errors.New("the cap or the stock said no")

// Reserve takes seats from a gig in two atomic steps inside one transaction: a per-fan cap upsert that only moves
// while the new total still fits, then one conditional UPDATE that matches only while the remaining stock covers the
// request. If either says no, the whole attempt rolls back (so a cap increment never outlives a failed reserve) and the
// refusal is recorded in a fresh transaction.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReserveSeats) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		// 1. the per-fan cap: one atomic upsert that only moves while the new total still fits
		capTag, err := tx.Exec(ctx, `
			INSERT INTO gigs.fans (gig_id, owner_id, seats)
			SELECT d.id, $2, $3 FROM gigs.gigs d WHERE d.id = $1 AND d.max_per_fan >= $3
			ON CONFLICT (gig_id, owner_id) DO UPDATE SET seats = gigs.fans.seats + $3
			WHERE gigs.fans.seats + $3 <= (SELECT max_per_fan FROM gigs.gigs WHERE id = $1)`,
			cmd.GigID, cmd.OwnerID, cmd.Seats)
		if err != nil {
			return err
		}
		if capTag.RowsAffected() == 1 {
			// 2. the remaining seats: one conditional UPDATE, the only writer of seats_left
			// The same statement that takes the seats returns how many were sold before this order, which fixes each seat's place
			// in the price steps: two concurrent orders get disjoint places because the row lock serialises them.
			var soldBefore int
			stockErr := tx.QueryRow(ctx, `
				UPDATE gigs.gigs SET seats_left = seats_left - $2 WHERE id = $1 AND seats_left >= $2
				RETURNING seats_total - (seats_left + $2)`, cmd.GigID, cmd.Seats).Scan(&soldBefore)
			if stockErr != nil && !errors.Is(stockErr, pgx.ErrNoRows) {
				return stockErr
			}
			if stockErr == nil {
				var list int64
				var total int
				if err := tx.QueryRow(ctx, `SELECT price_cents, seats_total FROM gigs.gigs WHERE id = $1`, cmd.GigID).Scan(&list, &total); err != nil {
					return err
				}
				subtotal, err := SubtotalCents(list, total, soldBefore, cmd.Seats)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO gigs.seat_holds (order_id, owner_id, gig_id, seats, subtotal_cents, status, created_at) VALUES ($1,$2,$3,$4,$5,'Held',now())`,
					cmd.OrderID, cmd.OwnerID, cmd.GigID, cmd.Seats, subtotal); err != nil {
					return err
				}
				return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.OrderID,
					messaging.SeatsHeld{OrderID: cmd.OrderID, OwnerID: cmd.OwnerID, GigID: cmd.GigID, Seats: cmd.Seats, Reference: cmd.Reference, SubtotalCents: subtotal})
			}
		}
		return errRefused // roll the cap increment back with everything else
	})
	if err == nil {
		return outcome, nil
	}
	if !errors.Is(err, errRefused) {
		return 0, err
	}
	// The refusal is recorded in a fresh transaction (the inbox claim was rolled back with the attempt).
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		var left, limit int
		_ = tx.QueryRow(ctx, `SELECT seats_left, max_per_fan FROM gigs.gigs WHERE id = $1`, cmd.GigID).Scan(&left, &limit)
		var holding int
		_ = tx.QueryRow(ctx, `SELECT seats FROM gigs.fans WHERE gig_id = $1 AND owner_id = $2`, cmd.GigID, cmd.OwnerID).Scan(&holding)
		reason := "stock"
		outcome = Short
		if limit > 0 && holding+cmd.Seats > limit {
			reason = "cap"
			outcome = CapReached
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.OrderID,
			messaging.SeatsShort{OrderID: cmd.OrderID, Reference: cmd.Reference, RequestedSeats: cmd.Seats, SeatsLeft: left, Reason: reason, PerFanLimit: limit})
	})
	return outcome, err
}

// Release gives held seats back exactly once, returning the fan's allowance too. A second release changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleaseSeats) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var gigID, ownerID uuid.UUID
		var seats int
		err = tx.QueryRow(ctx, `
			UPDATE gigs.seat_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE order_id = $1 AND status = 'Held' RETURNING gig_id, owner_id, seats`, cmd.OrderID, cmd.Reason).Scan(&gigID, &ownerID, &seats)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE gigs.gigs SET seats_left = seats_left + $2 WHERE id = $1 AND seats_left + $2 <= seats_total`, gigID, seats); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE gigs.fans SET seats = seats - $3 WHERE gig_id = $1 AND owner_id = $2 AND seats >= $3`, gigID, ownerID, seats); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.SeatsReleased{OrderID: cmd.OrderID, Seats: seats, Reason: cmd.Reason})
	})
	return released, err
}

var _ = time.Second
