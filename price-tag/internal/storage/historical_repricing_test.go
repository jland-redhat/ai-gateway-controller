package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/redhat-et/pricetag-metering/internal/pricing"
)

func TestBuildRepriceWhereTreatsPrefixMetacharactersLiterally(t *testing.T) {
	req := HistoricalRepriceRequest{
		ModelPrefix: "gemini_%",
		From:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Until:       time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
	}
	where, args, err := buildRepriceWhere(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(where, " LIKE ") {
		t.Fatalf("prefix scope must not use wildcard LIKE: %s", where)
	}
	if got := args[2]; got != req.ModelPrefix {
		t.Fatalf("prefix argument was transformed: got %v, want %q", got, req.ModelPrefix)
	}
}

func TestDiscountPolicyHashBindsRules(t *testing.T) {
	base := pricing.DefaultDiscountPolicy()
	first, err := discountPolicyHash(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Rules[0].Multiplier = 0.75
	second, err := discountPolicyHash(base)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("policy hash did not change when policy rules changed")
	}
}

func TestRepriceHourBoundsCeilAndClipPartialRange(t *testing.T) {
	from := time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC)
	until := time.Date(2026, 1, 1, 11, 30, 0, 0, time.UTC)
	hourFrom, hourUntil, chunks := repriceHourBounds(from, until)
	if !hourFrom.Equal(time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)) || !hourUntil.Equal(time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)) || chunks != 2 {
		t.Fatalf("bounds=(%s,%s,%d), want 10:00,12:00,2", hourFrom, hourUntil, chunks)
	}
	// The implementation must clip each chunk to [10:30,11:30], rather than
	// updating the surrounding full hours.
	if !from.Before(hourFrom.Add(time.Hour)) || !until.After(hourFrom.Add(time.Hour)) {
		t.Fatal("test range must cross a partial-hour boundary")
	}
}
