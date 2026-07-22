package jobs

import (
	"context"
	"errors"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-org-membership/internal/core/service"
	"github.com/jackc/pgx/v5"
)

// errNoTx is returned when a job expected to run inside a TxRunner.RunInTx
// but no pgx.Tx is present in ctx. Indicates a wiring bug.
var errNoTx = errors.New("reconciler: no pgx.Tx in ctx")

// txFromCtx surfaces the running tx via the shared service-layer key.
// (postgres.TxRunner puts the tx under both its private key and the
// public service.txAccessorKey.)
func txFromCtx(ctx context.Context) (pgx.Tx, bool) {
	// service.WithTx / pgadapterTxFromContext use a private txAccessorKey;
	// we can access it here via a small re-export in the service package.
	return service.TxFromContext(ctx)
}
