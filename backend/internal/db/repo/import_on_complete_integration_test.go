//go:build integration

package repo

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/artyomsv/marauder/backend/internal/domain"
)

// TestSonarrImportOnCompleteMigrationAndRoundTrip pins migration 0021: the
// column defaults to false for rows written without it, and Create/Update
// write it through the real statements.
func TestSonarrImportOnCompleteMigrationAndRoundTrip(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	owner := seedUser(t, pool)
	r := NewSonarrInstances(pool)
	master := testMasterKey(t)

	var legacyID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO sonarr_instances (name, url, owner_user_id) VALUES ('TEST-import-legacy', 'http://sonarr.invalid:8989', $1) RETURNING id`, owner).Scan(&legacyID); err != nil {
		t.Fatal(err)
	}
	created, err := r.Create(ctx, master, domain.SonarrInstance{
		Name: "TEST-import", URL: "http://sonarr.invalid:8989", APIKey: "TEST-key",
		ImportOnComplete: true, OwnerUserID: &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []uuid.UUID{legacyID, created.ID} {
			if _, err := pool.Exec(ctx, `DELETE FROM sonarr_instances WHERE id = $1`, id); err != nil {
				t.Errorf("cleanup instance: %v", err)
			}
		}
	})

	legacy, err := r.Get(ctx, master, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ImportOnComplete {
		t.Error("an instance written without the column must default to off")
	}
	if !created.ImportOnComplete {
		t.Error("Create did not persist import_on_complete")
	}

	created.ImportOnComplete = false
	created.APIKey = "" // keep the stored key
	got, err := r.Update(ctx, master, created.ID, *created)
	if err != nil {
		t.Fatal(err)
	}
	if got.ImportOnComplete {
		t.Error("Update did not clear import_on_complete")
	}
	if got.APIKey != "TEST-key" {
		t.Errorf("empty-key update must keep the stored key, got %q", got.APIKey)
	}
}
