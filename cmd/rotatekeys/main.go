// Command rotatekeys re-encrypts every stored secret (BYOK keys, integration
// credentials) under the current ENCRYPTION_KEY.
//
// Rotation: set ENCRYPTION_KEY to the new key and ENCRYPTION_KEY_PREVIOUS to
// the old one, deploy, run this command, and once it reports nothing left to
// rotate drop ENCRYPTION_KEY_PREVIOUS. Safe to re-run; -dry-run only counts.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/founderstack/api/internal/config"
	"github.com/founderstack/api/internal/pkg/vault"
)

var undecryptable int

type target struct{ table, idCol, secretCol string }

var targets = []target{
	{"api_key_registry", "id", "encrypted_key"},
	{"mcp_connections", "id", "encrypted_credentials"},
}

func main() {
	dry := flag.Bool("dry-run", false, "count values needing rotation without writing")
	flag.Parse()
	if err := run(context.Background(), *dry); err != nil {
		fmt.Fprintln(os.Stderr, "rotatekeys:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, dry bool) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ring, err := vault.DecodeKeyring(cfg.EncryptionKey.Expose(), cfg.EncryptionKeyPrevious.Expose())
	if err != nil {
		return err
	}
	pool, err := pgxpool.New(ctx, cfg.SystemDatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	for _, t := range targets {
		n, err := rotateTable(ctx, pool, t, ring, dry)
		if err != nil {
			return fmt.Errorf("%s: %w", t.table, err)
		}
		fmt.Printf("%s: %d value(s) %s\n", t.table, n, map[bool]string{true: "need rotation", false: "rotated"}[dry])
	}
	if undecryptable > 0 {
		return fmt.Errorf("%d value(s) open under no configured key — add the missing key to ENCRYPTION_KEY_PREVIOUS before dropping it", undecryptable)
	}
	return nil
}

func rotateTable(ctx context.Context, pool *pgxpool.Pool, t target, ring []byte, dry bool) (int, error) {
	rows, err := pool.Query(ctx, fmt.Sprintf("SELECT %s::text, %s FROM %s WHERE %s IS NOT NULL", t.idCol, t.secretCol, t.table, t.secretCol))
	if err != nil {
		return 0, err
	}
	type item struct{ id, old, fresh string }
	var todo []item
	for rows.Next() {
		var id, ct string
		if err := rows.Scan(&id, &ct); err != nil {
			rows.Close()
			return 0, err
		}
		if vault.UsesCurrentKey(ct, ring) {
			continue
		}
		plain, err := vault.Decrypt(ct, ring)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: row %s opens under no configured key (skipped)\n", t.table, id)
			undecryptable++
			continue
		}
		fresh, err := vault.Encrypt(plain, ring)
		if err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, item{id, ct, fresh})
	}
	rows.Close()
	if rows.Err() != nil || dry {
		return len(todo), rows.Err()
	}
	n := 0
	for _, it := range todo {
		// The old value in the WHERE keeps a concurrent token refresh from being overwritten.
		tag, err := pool.Exec(ctx, fmt.Sprintf("UPDATE %s SET %s = $1 WHERE %s::text = $2 AND %s = $3", t.table, t.secretCol, t.idCol, t.secretCol), it.fresh, it.id, it.old)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}
