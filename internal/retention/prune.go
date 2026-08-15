// Package retention prunes expired signed derivatives (Phase E).
//
// Original IPAs are immutable and kept forever. A signed derivative, however,
// embeds a provisioning profile that expires, so it is only useful for a
// bounded time after its last profile refat. Rows whose profile expired beyond
// a retention window are deleted, and their content-addressed files are
// removed when no remaining signed or original artifact references them.
package retention

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"sidey/internal/artifacts"
	"sidey/internal/audit"
)

// PruneSignedArtifacts deletes signed_artifacts rows whose embedded profile
// expired more than `retention` ago, then removes the stored IPA bytes unless
// another signed artifact or an original artifact still references them. It
// returns the number of rows pruned.
//
// Row deletion and file removal are serialized with artifact uploads through
// a per-hash advisory lock: an upload publishes its blob and inserts its row
// in one transaction holding `pg_advisory_xact_lock(hashtext($sha))`, so
// retention can never decide a hash is unreferenced while an upload holds the
// bytes, and never delete bytes an upload is about to make freshly visible.
func PruneSignedArtifacts(ctx context.Context, pool *pgxpool.Pool, auditClient *audit.Client, store *artifacts.Store, retention time.Duration) (int, error) {
	if retention == 0 {
		return 0, nil
	}

	rows, err := pool.Query(ctx, `
		SELECT DISTINCT signed_ipa_sha256
		FROM signed_artifacts
		WHERE profile_expiry_at IS NOT NULL
		  AND profile_expiry_at < now() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(retention.Seconds())))
	if err != nil {
		return 0, err
	}
	var shas []string
	for rows.Next() {
		var sha string
		if err := rows.Scan(&sha); err != nil {
			rows.Close()
			return 0, err
		}
		shas = append(shas, sha)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	pruned := 0
	removedFiles := 0
	for _, sha := range shas {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return pruned, err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, sha); err != nil {
			tx.Rollback(ctx)
			return pruned, err
		}
		res, err := tx.Exec(ctx, `
			DELETE FROM signed_artifacts
			WHERE signed_ipa_sha256 = $1
			  AND profile_expiry_at IS NOT NULL
			  AND profile_expiry_at < now() - $2::interval`,
			sha, fmt.Sprintf("%d seconds", int64(retention.Seconds())))
		if err != nil {
			tx.Rollback(ctx)
			return pruned, err
		}
		pruned += int(res.RowsAffected())
		// Once no signed or original artifact row references the hash, the
		// bytes are unreachable: drop them. Held lock makes this decision
		// final until commit, so an uploader either sees the file (pre-lock)
		// or recreates it under the same lock afterwards.
		var referenced int
		if err := tx.QueryRow(ctx, `
			SELECT (SELECT COUNT(*) FROM signed_artifacts WHERE signed_ipa_sha256 = $1)
			     + (SELECT COUNT(*) FROM artifacts        WHERE sha256 = $1)`,
			sha).Scan(&referenced); err != nil {
			tx.Rollback(ctx)
			return pruned, err
		}
		if referenced == 0 && store.Exists(sha) {
			store.Remove(sha)
			removedFiles++
		}
		if err := tx.Commit(ctx); err != nil {
			return pruned, err
		}
	}

	if pruned > 0 {
		auditClient.Record(ctx, "retention", "signed_artifacts.pruned",
			audit.WithData(map[string]any{
				"pruned_rows": pruned, "removed_files": removedFiles}))
	}
	return pruned, nil
}