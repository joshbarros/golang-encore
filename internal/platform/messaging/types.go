// Package messaging is the transactional outbox, the inbox, and the RabbitMQ and Kafka plumbing around them.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Names of the exchanges, the Kafka topic and the routing keys. They are the contract between the modules.
const (
	Exchange        = "encore.work"
	DeadLetterExch  = "encore.dlx"
	RequeueExchange = "encore.requeue"
	KafkaTopic      = "encore.events"
	RetryDelayMs    = 2000
	MaxAttempts     = 3
	ReserveKey      = "seats.reserve"
	ReleaseKey      = "seats.release"
	HeldKey         = "seats.held"
	ShortKey        = "seats.short"
	ReleasedKey     = "seats.released"
	IssuedKey       = "pass.issued"
	RejectedKey     = "pass.rejected"
	GigsQueue       = "gigs.commands"
	PassesQueue     = "passes.work"
	OrdersQueue     = "orders.events"
	PoisonToken     = "POISON"
	attemptHeader   = "x-attempt"
	outboxBatchSize = 50
	relayIdleDelay  = 200 * time.Millisecond
	relayErrorDelay = time.Second
	trimEvery       = 10 * time.Minute
)

// Delivery is one message handed to a Handler.
type Delivery struct {
	MessageID  uuid.UUID
	RoutingKey string
	Body       []byte
}

// Handler consumes one message. Returning ErrPoison dead-letters it at once; any other error retries it.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }

// ErrPoison marks a message whose rejection is already committed: it must be dead-lettered, not retried.
var ErrPoison = errors.New("poison message")

// OutOfOrderError means the message arrived before the state it depends on; it is retried later.
type OutOfOrderError struct{ Reason string }

func (e *OutOfOrderError) Error() string { return e.Reason }

// IsPoison reports whether the reference carries the poison token (a case-sensitive substring).
func IsPoison(reference string) bool { return strings.Contains(reference, PoisonToken) }

// FailureAction is what a consumer does with a message whose handler failed.
type FailureAction int

// The two things a failed message can meet.
const (
	ActionRetry FailureAction = iota
	ActionDeadLetter
)

// Decide: a poison message is dead-lettered at once; anything else is retried until it has been tried MaxAttempts times.
func Decide(priorAttempts int, err error) FailureAction {
	if errors.Is(err, ErrPoison) {
		return ActionDeadLetter
	}
	if priorAttempts+1 >= MaxAttempts {
		return ActionDeadLetter
	}
	return ActionRetry
}

// ---- the messages ----

// ReserveSeats asks the gig to hold seats.
type ReserveSeats struct {
	OrderID   uuid.UUID `json:"orderId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	GigID     uuid.UUID `json:"gigId"`
	Seats     int       `json:"seats"`
	Reference string    `json:"reference"`
}

// SeatsHeld says the seats are held. SubtotalCents is what they cost together at their places in the price steps.
type SeatsHeld struct {
	SubtotalCents int64     `json:"subtotalCents"`
	OrderID       uuid.UUID `json:"orderId"`
	OwnerID       uuid.UUID `json:"ownerId"`
	GigID         uuid.UUID `json:"gigId"`
	Seats         int       `json:"seats"`
	Reference     string    `json:"reference"`
}

// SeatsShort says the reserve was refused. Reason: "stock" or "cap".
type SeatsShort struct {
	OrderID        uuid.UUID `json:"orderId"`
	Reference      string    `json:"reference"`
	RequestedSeats int       `json:"requestedSeats"`
	SeatsLeft      int       `json:"seatsLeft"`
	// PerFanLimit is the cap of the gig.
	PerFanLimit int    `json:"perFanLimit"`
	Reason      string `json:"reason"`
}

// ReleaseSeats asks the gig to give held seats back.
type ReleaseSeats struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// SeatsReleased says seats (possibly none) were given back.
type SeatsReleased struct {
	OrderID uuid.UUID `json:"orderId"`
	Seats   int       `json:"seats"`
	Reason  string    `json:"reason"`
}

// PassIssued says the pass exists.
type PassIssued struct {
	OrderID uuid.UUID `json:"orderId"`
	Number  string    `json:"number"`
}

// PassRejected says the pass was refused (the poison rule).
type PassRejected struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// Parse decodes a message body.
func Parse[T any](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	return v, nil
}
