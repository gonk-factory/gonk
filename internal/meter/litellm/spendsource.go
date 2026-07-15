package litellm

import (
	"context"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// SpendSource is the raw ledger. Since returns every spend row at or after t,
// plus the SOURCE'S OWN CLOCK (LiteLLM's HTTP Date header). The clock is not a
// curiosity: meter compares it to its own before it will trust itself with a
// month-rollover decision (spend.SkewOK). A meter whose clock has jumped
// forward would otherwise roll the window early and hand every project a second
// budget.
type SpendSource interface {
	Since(ctx context.Context, t time.Time) (rows []spend.Row, sourceClock time.Time, err error)
}
