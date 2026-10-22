// Package risk decides whether a withdrawal can leave custody automatically,
// needs a human to review it, or must be refused.
//
// The engine is a pure function of its input so every rule can be unit
// tested without a database. The ledger gathers the input (recent withdrawal
// history, when the destination address was added) under a row lock on the
// account, so two concurrent withdrawals cannot both slip under a limit.
package risk

import (
	"fmt"
	"time"
)

type Outcome int

const (
	Approve Outcome = iota
	Review
	Reject
)

func (o Outcome) String() string {
	switch o {
	case Approve:
		return "approve"
	case Review:
		return "review"
	default:
		return "reject"
	}
}

// Policy holds limits for one asset, in that asset's smallest unit.
type Policy struct {
	// DailyLimit caps the total withdrawn in any rolling 24 hours,
	// counting withdrawals that are approved or still in review.
	DailyLimit int64
	// MaxPerHour caps how many withdrawals an account may request in an
	// hour. A burst of requests is a common sign of a taken over account.
	MaxPerHour int
	// ReviewAbove sends any single withdrawal larger than this to review.
	ReviewAbove int64
	// NewAddressHold sends withdrawals to an address added more recently
	// than this to review, giving the real owner time to notice.
	NewAddressHold time.Duration
	// ReviewApprovals is how many distinct operators must approve a held
	// withdrawal before it settles (four-eyes when 2). Zero means 1.
	ReviewApprovals int
}

// Approvals returns the effective number of approvals required.
func (p Policy) Approvals() int {
	return max(p.ReviewApprovals, 1)
}

// DefaultPolicy is a conservative fallback for assets with no policy set.
var DefaultPolicy = Policy{
	DailyLimit:      1_000_000,
	MaxPerHour:      5,
	ReviewAbove:     250_000,
	NewAddressHold:  24 * time.Hour,
	ReviewApprovals: 2,
}

type Input struct {
	Amount int64
	// WithdrawnLast24h and CountLastHour describe earlier withdrawals by
	// the same account, excluding this one.
	WithdrawnLast24h int64
	CountLastHour    int
	// AddressKnown is false when the address is not on the account's list.
	AddressKnown   bool
	AddressAddedAt time.Time
	Now            time.Time
}

type Decision struct {
	Outcome Outcome
	Reasons []string
}

// Evaluate applies every rule and returns the most severe outcome along with
// all the reasons, so a reviewer sees the whole picture at once.
func (p Policy) Evaluate(in Input) Decision {
	d := Decision{Outcome: Approve}
	flag := func(o Outcome, format string, args ...any) {
		if o > d.Outcome {
			d.Outcome = o
		}
		d.Reasons = append(d.Reasons, fmt.Sprintf(format, args...))
	}

	if in.Amount <= 0 {
		flag(Reject, "amount must be positive")
		return d
	}
	if p.DailyLimit > 0 && in.WithdrawnLast24h+in.Amount > p.DailyLimit {
		flag(Reject, "exceeds 24h limit: %d already withdrawn, limit %d",
			in.WithdrawnLast24h, p.DailyLimit)
	}
	if p.MaxPerHour > 0 && in.CountLastHour >= p.MaxPerHour {
		flag(Reject, "velocity: %d withdrawals in the last hour, limit %d",
			in.CountLastHour, p.MaxPerHour)
	}
	if !in.AddressKnown {
		flag(Review, "destination address is not on the account's address list")
	} else if age := in.Now.Sub(in.AddressAddedAt); age < p.NewAddressHold {
		flag(Review, "destination address was added %s ago, hold is %s",
			age.Round(time.Minute), p.NewAddressHold)
	}
	if p.ReviewAbove > 0 && in.Amount > p.ReviewAbove {
		flag(Review, "amount %d is above the review threshold %d", in.Amount, p.ReviewAbove)
	}
	return d
}
