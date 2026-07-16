package litellm

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gitlab.orac.local/agentic/gonk-project/pkg/spend"
)

// Fake implements Admin and SpendSource in memory. It is the ONLY LiteLLM the
// tests in this plan see: rung policy and budget accounting are pure functions
// of (config, spend, attempts), so nothing here needs a live proxy (spec 10.2).
type Fake struct {
	mu   sync.Mutex
	Keys map[string]KeyInfo
	Rows []spend.Row
	Now  time.Time

	// Failure injection: the failure-mode matrix is a test, not a paragraph.
	AdminErr error
	SpendErr error
	// ClockOffset skews the source clock relative to Now, to drive SkewOK.
	ClockOffset time.Duration

	n int
}

func NewFake() *Fake { return &Fake{Keys: map[string]KeyInfo{}} }

func (f *Fake) EnsureKey(_ context.Context, spec KeySpec) (KeyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AdminErr != nil {
		return KeyInfo{}, f.AdminErr
	}
	if k, ok := f.Keys[spec.Alias]; ok {
		return k, nil // idempotent by alias
	}
	f.n++
	k := KeyInfo{Alias: spec.Alias, Token: fmt.Sprintf("sk-fake-%d", f.n)}
	f.Keys[spec.Alias] = k
	return k, nil
}

func (f *Fake) RotateKey(_ context.Context, spec KeySpec) (KeyInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AdminErr != nil {
		return KeyInfo{}, f.AdminErr
	}
	f.n++
	k := KeyInfo{Alias: spec.Alias, Token: fmt.Sprintf("sk-fake-%d", f.n)}
	f.Keys[spec.Alias] = k
	return k, nil
}

func (f *Fake) DeleteKey(_ context.Context, alias string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.AdminErr != nil {
		return f.AdminErr
	}
	delete(f.Keys, alias)
	return nil
}

func (f *Fake) Since(_ context.Context, t time.Time) ([]spend.Row, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SpendErr != nil {
		return nil, time.Time{}, f.SpendErr
	}
	var out []spend.Row
	for _, r := range f.Rows {
		if !r.At.Before(t) {
			out = append(out, r)
		}
	}
	return out, f.Now.Add(f.ClockOffset), nil
}

// AddRows is how a test says "LiteLLM billed this".
func (f *Fake) AddRows(rows ...spend.Row) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Rows = append(f.Rows, rows...)
}

var (
	_ Admin       = (*Fake)(nil)
	_ SpendSource = (*Fake)(nil)
)
