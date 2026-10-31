package store

import (
	"context"
	"errors"

	"github.com/uptrace/bun"

	"github.com/tabloy/keygate/internal/model"
)

// productShapeLockSeed is hashed with the product id to name its shape
// lock (see ProductShapeLockIn), apart from the other advisory locks.
const productShapeLockSeed int64 = 0x6b6773686170 // "kgshap"

// ErrProductNoReleases: the product's type does not have releases (any
// more), checked under the shape lock.
var ErrProductNoReleases = errors.New("the product's type has no releases")

// ProductShapeLockIn takes the product's shape lock for the
// transaction: exclusive for a change of the product's type, shared for
// a write whose validity depends on that type (a plan's activation and
// seat fields, a release made or put back on sale).
//
// A type change checks what the product holds and a writer checks the
// type, each before writing; without the lock both could pass on what
// the other had not yet committed, leaving, say, a desktop product with
// a plan selling seats. Taken as the transaction's first lock by every
// party, it orders them without entering the order of any row lock.
func ProductShapeLockIn(ctx context.Context, db bun.IDB, productID string, exclusive bool) error {
	fn := "pg_advisory_xact_lock_shared"
	if exclusive {
		fn = "pg_advisory_xact_lock"
	}
	_, err := db.ExecContext(ctx, "SELECT "+fn+"(?)", productShapeLockSeed^stringHash(productID))
	return err
}

// ProductTypeIn reads the product's type, under its shape lock.
func ProductTypeIn(ctx context.Context, db bun.IDB, productID string) (string, error) {
	var t string
	err := db.NewRaw("SELECT type FROM products WHERE id = ?", productID).Scan(ctx, &t)
	return t, err
}

// CountLiveReleasesIn counts the product's draft and published releases:
// the ones a type without releases would leave unreachable.
func CountLiveReleasesIn(ctx context.Context, db bun.IDB, productID string) (int, error) {
	return db.NewSelect().Model((*model.Release)(nil)).
		Where("product_id = ? AND status IN (?)", productID,
			bun.In([]string{model.ReleaseStatusDraft, model.ReleaseStatusPublished})).
		Count(ctx)
}

// releasesAllowedIn takes the product's shape lock (shared) and refuses
// with ErrProductNoReleases when its type has no releases.
func releasesAllowedIn(ctx context.Context, db bun.IDB, productID string) error {
	if err := ProductShapeLockIn(ctx, db, productID, false); err != nil {
		return err
	}
	t, err := ProductTypeIn(ctx, db, productID)
	if err != nil {
		return err
	}
	if !model.ProductSupports(t, model.CapReleases) {
		return ErrProductNoReleases
	}
	return nil
}
