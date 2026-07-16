package store_test

import (
	"testing"

	"gitlab.orac.local/agentic/gonk-project/internal/meter/store"
	"gitlab.orac.local/agentic/gonk-project/internal/meter/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func() store.Store { return store.NewMemory() })
}
