package orders

import (
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-encore/internal/platform/messaging"
)

func TestValidate(t *testing.T) {
	cases := []struct {
		ref   string
		seats int64
		bad   string
	}{
		{"FAN-1", 1, ""},
		{"FAN-1", 6, ""},
		{"", 1, "reference is required"},
		{"   ", 1, "reference is required"},
		{strings.Repeat("x", 41), 1, "at most 40"},
		{strings.Repeat("x", 40), 1, ""},
		{"FAN-1", 0, "at least 1 seat"},
		{"FAN-1", -3, "at least 1 seat"},
		{"FAN-1", 7, "at most 6 seats"},
		{"FAN-1", 4294967297, "at most 6 seats"}, // would wrap to 1 if narrowed to 32 bits
		{"FAN-1", math.MaxInt64, "at most 6 seats"},
	}
	for _, c := range cases {
		got := Validate(c.ref, c.seats)
		if (c.bad == "") != (got == "") || (c.bad != "" && !strings.Contains(got, c.bad)) {
			t.Errorf("Validate(%q, %d) = %q, want it to contain %q", c.ref, c.seats, got, c.bad)
		}
	}
}

func TestTotalIsFaceValuePlusTwelvePercentTruncated(t *testing.T) {
	for _, c := range []struct {
		price int64
		seats int
		want  int64
	}{{5000, 2, 11200}, {1, 1, 1}, {7, 1, 7}, {100, 1, 112}, {5000, 4, 22400}} {
		got, err := TotalCents(c.price, c.seats)
		if err != nil || got != c.want {
			t.Errorf("TotalCents(%d,%d) = %d (%v), want %d", c.price, c.seats, got, err, c.want)
		}
	}
}

func TestTotalThrowsInsteadOfWrapping(t *testing.T) {
	for _, c := range []struct {
		price int64
		seats int
	}{{math.MaxInt64, 2}, {math.MaxInt64 / 2, 2}, {math.MaxInt64 / 1000, 1}, {-1, 1}} {
		if _, err := TotalCents(c.price, c.seats); err == nil {
			t.Errorf("TotalCents(%d,%d) must overflow", c.price, c.seats)
		}
	}
}

func TestShortReasonsSayWhy(t *testing.T) {
	id := uuid.New()
	cases := map[string]string{
		"cap":   "Cap: at most 2 seats per fan",
		"stock": "Short: 3 seats wanted, 1 left.",
	}
	for reason, want := range cases {
		got := ShortReason(messaging.SeatsShort{OrderID: id, Reason: reason, PerFanLimit: 2, RequestedSeats: 3, SeatsLeft: 1})
		if !strings.Contains(got, want) {
			t.Errorf("%s: %q should contain %q", reason, got, want)
		}
	}
}
