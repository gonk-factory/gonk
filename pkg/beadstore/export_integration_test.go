//go:build integration

package beadstore

import "context"

// ShowSetting reads a session parameter on one of the store's own pooled
// connections. Test-only (package beadstore's export to its external tests).
func ShowSetting(ctx context.Context, p *Postgres, name string) (string, error) {
	var v string
	err := p.pool.QueryRow(ctx, `SELECT current_setting($1)`, name).Scan(&v)
	return v, err
}

// PoolMaxConns is the pool's configured ceiling. Test-only (gonk-upxg).
func PoolMaxConns(p *Postgres) int32 { return p.pool.Config().MaxConns }
