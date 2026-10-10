package store_test

import (
	"testing"

	"github.com/gonk-factory/gonk/internal/meter/store"
	"github.com/gonk-factory/gonk/internal/meter/store/storetest"
)

func TestMemory(t *testing.T) {
	storetest.Run(t, func(t *testing.T) store.Store { return store.NewMemory() })
}
