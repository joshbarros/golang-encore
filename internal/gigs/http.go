package gigs

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-encore/internal/platform/httpx"
)

const (
	maxSeatsTotal = 5000
	maxPriceCents = 1_000_000
	defaultPerFan = 4
)

// Gig is the public view of a gig.
type Gig struct {
	ID         uuid.UUID `json:"id"`
	Code       string    `json:"code"`
	Name       string    `json:"name"`
	Stage      string    `json:"stage"`
	Band       string    `json:"band"`
	GigDate    time.Time `json:"gigDate"`
	PriceCents int64     `json:"priceCents"`
	SeatsTotal int       `json:"seatsTotal"`
	SeatsLeft  int       `json:"seatsLeft"`
	MaxPerFan  int       `json:"maxPerFan"`
	// NextSeatPriceCents and Tier say what the next seat sells at; both are null when the gig is sold out.
	NextSeatPriceCents *int64  `json:"nextSeatPriceCents"`
	Tier               *string `json:"tier"`
}

// Info is what other modules may ask the gigs module (Orders never reads this schema).
type Info struct {
	ID         uuid.UUID
	Code       string
	PriceCents int64
}

// Catalog answers the one cross-module read: a gig's existence and price.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a gig up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code, price_cents FROM gigs.gigs WHERE code = $1`, code).Scan(&i.ID, &i.Code, &i.PriceCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the gigs HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the gig endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/gigs", authed(a.list))
	mux.HandleFunc("GET /api/gigs/{code}", authed(a.get))
	mux.HandleFunc("POST /api/gigs", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const gigSelect = `SELECT id, code, name, stage, band, gig_date, price_cents, seats_total, seats_left, max_per_fan FROM gigs.gigs`

func scanGig(row pgx.Row) (Gig, error) {
	var d Gig
	err := row.Scan(&d.ID, &d.Code, &d.Name, &d.Stage, &d.Band, &d.GigDate, &d.PriceCents, &d.SeatsTotal, &d.SeatsLeft, &d.MaxPerFan)
	if err == nil {
		if price, tier, ok := NextSeat(d.PriceCents, d.SeatsTotal, d.SeatsLeft); ok {
			t := string(tier)
			d.NextSeatPriceCents, d.Tier = &price, &t
		}
	}
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), gigSelect+` ORDER BY gig_date, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Gig{}
	for rows.Next() {
		d, err := scanGig(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanGig(a.Pool.QueryRow(r.Context(), gigSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code       string     `json:"code"`
	Name       string     `json:"name"`
	Stage      string     `json:"stage"`
	Band       string     `json:"band"`
	GigDate    *time.Time `json:"gigDate"`
	SeatsTotal int64      `json:"seatsTotal"`
	PriceCents int64      `json:"priceCents"`
	MaxPerFan  *int64     `json:"maxPerFan"`
}

// create opens a gig. Staff only. This is the only place seats_left is initialised; after that only the reserve and
// release handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Stage) == "" || strings.TrimSpace(req.Band) == "" {
		return httpx.Fail(http.StatusBadRequest, "Name, stage and band are required.")
	}
	if req.SeatsTotal < 1 || req.SeatsTotal > maxSeatsTotal {
		return httpx.Fail(http.StatusBadRequest, "Seats on sale must be between 1 and %d seats.", maxSeatsTotal)
	}
	if req.PriceCents < 1 || req.PriceCents > maxPriceCents {
		return httpx.Fail(http.StatusBadRequest, "Price must be between 1 and %d cents per seat.", maxPriceCents)
	}
	limit := min(int64(defaultPerFan), req.SeatsTotal)
	if req.MaxPerFan != nil {
		limit = *req.MaxPerFan
	}
	if limit < 1 || limit > req.SeatsTotal {
		return httpx.Fail(http.StatusBadRequest, "Per-fan cap must be at least 1 and no more than the seats on sale.")
	}
	release := time.Now().AddDate(0, 0, 14)
	if req.GigDate != nil {
		release = *req.GigDate
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO gigs.gigs (id, code, name, stage, band, gig_date, price_cents, seats_total, seats_left, max_per_fan, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,now())`,
		id, code, strings.TrimSpace(req.Name), strings.TrimSpace(req.Stage), strings.TrimSpace(req.Band), release, req.PriceCents, req.SeatsTotal, limit)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Gig '%s' already exists.", code)
		}
		return err
	}
	d, err := scanGig(a.Pool.QueryRow(r.Context(), gigSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/gigs/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}
