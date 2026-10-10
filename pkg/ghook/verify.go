// Package ghook is gonk's GitLab webhook trust boundary: token verification,
// hardened receipt, event parsing, and duplicate suppression. Every input here
// is treated as hostile — the endpoint is, by design, reachable from anything
// that can route to the ingress.
package ghook

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
)

// MinSecretLen is the shortest webhook token gonk accepts. GitLab enforces no
// length at all, and this token is the only thing standing between the internet
// and the component that decides what may spend money.
const MinSecretLen = 32

var (
	ErrMissingToken = errors.New("ghook: missing X-Gitlab-Token header")
	ErrBadToken     = errors.New("ghook: X-Gitlab-Token matches no configured secret")
)

// Verifier checks X-Gitlab-Token against one or more accepted secrets. Multiple
// secrets are the rotation slots of spec 9: during a rotation both the new and
// the previous secret verify, so hooks can be re-provisioned without dropping
// events.
type Verifier struct {
	secrets [][]byte
}

// NewVerifier rejects a configuration that would silently disable verification.
// An empty slot is skipped (an unused rotation slot is normal); zero usable
// secrets, or a secret shorter than MinSecretLen, is a fatal config error.
func NewVerifier(secrets ...string) (*Verifier, error) {
	v := &Verifier{}
	for i, s := range secrets {
		if s == "" {
			continue
		}
		if len(s) < MinSecretLen {
			return nil, fmt.Errorf("ghook: webhook secret in slot %d is %d bytes, want >= %d", i, len(s), MinSecretLen)
		}
		v.secrets = append(v.secrets, []byte(s))
	}
	if len(v.secrets) == 0 {
		return nil, errors.New("ghook: no webhook secret configured")
	}
	return v, nil
}

// Verify compares against every slot and ORs the results: it does not
// short-circuit on the first match, so timing reveals neither which slot matched
// nor how many are configured. Errors never contain secret material.
func (v *Verifier) Verify(r *http.Request) error {
	got := []byte(r.Header.Get("X-Gitlab-Token"))
	if len(got) == 0 {
		return ErrMissingToken
	}
	var ok int
	for _, s := range v.secrets {
		ok |= subtle.ConstantTimeCompare(got, s)
	}
	if ok != 1 {
		return ErrBadToken
	}
	return nil
}
