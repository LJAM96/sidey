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
func PruneSignedArtifacts(ctx context.Context, pool *pgxpool.Pool, auditClient *audit.Client, store *artifacts.Store, retention time.Duration) (int, error) {
	if retention == 0 {
		return 0, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	pruned := 0
	rows, err := tx.Query(ctx, `
		SELECT signed_ipa_sha256
		FROM signed_artifacts
		WHERE profile_expiry_at IS NOT NULL
		  AND profile_expiry_at < now() - $1::interval
		ORDER BY signed_ipa_sha256`, fmt.Sprintf("%d seconds", int64(retention.Seconds())))
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

	seen := make(map[string]struct{}, len(shas))
	for _, sha := range shas {
		if _, ok := seen[sha]; ok {
			continue
		}
		seen[sha] = struct{}{}
		res, err := tx.Exec(ctx, `
			DELETE FROM signed_artifacts
			WHERE signed_ipa_sha256 = $1
			  AND profile_expiry_at IS NOT NULL
			  AND profile_expiry_at < now() - $2::interval`,
			sha, fmt.Sprintf("%d seconds", int64(retention.Seconds())))
		if err != nil {
			return 0, err
		}
		n := res.RowsAffected()
		pruned += int(n)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}

	// Delete files only after the rows are gone, and only when neither the
	// signed nor the original artifact tables still reference a hash.
	removedFiles := 0
	for sha := range seen {
		var referenced int
		err := pool.QueryRow(ctx, `
			SELECT (SELECT COUNT(*) FROM signed_artifacts WHERE signed_ipa_sha256 = $1)
			     + (SELECT COUNT(*) FROM artifacts        WHERE sha256 = $1)`,
			sha).Scan(&referenced)
		if err != nil {
			continue
		}
		if referenced == 0 && store.Exists(sha) {
			store.Remove(sha)
			removedFiles++
		}
	}

	if pruned > 0 {
		auditClient.Record(ctx, "retention", "signed_artifacts.pruned",
			audit.WithData(map[string]any{
				"pruned_rows": pruned, "removed_files": removedFiles}))
	}
	return pruned, nil
}