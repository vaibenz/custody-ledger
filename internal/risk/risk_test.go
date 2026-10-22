package risk

import (
	"testing"
	"time"
)

func TestEvaluate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	p := Policy{
		DailyLimit:     1_000,
		MaxPerHour:     3,
		ReviewAbove:    500,
		NewAddressHold: 24 * time.Hour,
	}
	trusted := func(in Input) Input {
		in.AddressKnown = true
		in.AddressAddedAt = now.Add(-72 * time.Hour)
		in.Now = now
		return in
	}

	cases := []struct {
		name    string
		in      Input
		want    Outcome
		reasons int
	}{
		{"small to trusted address", trusted(Input{Amount: 100}), Approve, 0},
		{"exactly at review threshold", trusted(Input{Amount: 500}), Approve, 0},
		{"above review threshold", trusted(Input{Amount: 501}), Review, 1},
		{"unknown address", Input{Amount: 100, Now: now}, Review, 1},
		{"address added an hour ago", Input{
			Amount: 100, AddressKnown: true, AddressAddedAt: now.Add(-time.Hour), Now: now,
		}, Review, 1},
		{"exactly at daily limit", trusted(Input{Amount: 400, WithdrawnLast24h: 600}), Approve, 0},
		{"over daily limit", trusted(Input{Amount: 401, WithdrawnLast24h: 600}), Reject, 1},
		{"velocity", trusted(Input{Amount: 10, CountLastHour: 3}), Reject, 1},
		{"reject outranks review and keeps all reasons", Input{
			Amount: 900, WithdrawnLast24h: 500, CountLastHour: 3, Now: now,
		}, Reject, 4},
		{"non positive", trusted(Input{Amount: 0}), Reject, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := p.Evaluate(tc.in)
			if d.Outcome != tc.want {
				t.Errorf("outcome = %s, want %s (reasons %v)", d.Outcome, tc.want, d.Reasons)
			}
			if len(d.Reasons) != tc.reasons {
				t.Errorf("got %d reasons %v, want %d", len(d.Reasons), d.Reasons, tc.reasons)
			}
		})
	}
}

func TestZeroLimitsDisableRules(t *testing.T) {
	now := time.Now()
	d := Policy{}.Evaluate(Input{
		Amount: 1 << 40, WithdrawnLast24h: 1 << 40, CountLastHour: 1000,
		AddressKnown: true, AddressAddedAt: now, Now: now,
	})
	if d.Outcome != Approve {
		t.Errorf("empty policy should approve, got %s %v", d.Outcome, d.Reasons)
	}
}

func TestOutcomeNamesAndApprovals(t *testing.T) {
	if Approve.String() != "approve" || Review.String() != "review" || Reject.String() != "reject" {
		t.Error("outcome names")
	}
	if (Policy{}).Approvals() != 1 || (Policy{ReviewApprovals: 3}).Approvals() != 3 {
		t.Error("approvals default")
	}
}
