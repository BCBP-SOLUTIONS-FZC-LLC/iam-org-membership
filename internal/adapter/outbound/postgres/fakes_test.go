package postgres

import (
	"context"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Shared fake pgx.Tx / pgx.Rows / pgx.Row helpers for repository unit tests.
// Embeds the pgx interface so any un-overridden method nil-panics — proof
// the SUT only touches the small set we've stubbed.

// ── fakeTx ─────────────────────────────────────────────────────────────

type fakeTx struct {
	pgx.Tx
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (f *fakeTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if f.queryFn != nil {
		return f.queryFn(ctx, sql, args...)
	}
	return nil, errors.New("fakeTx.Query not stubbed")
}

func (f *fakeTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if f.queryRowFn != nil {
		return f.queryRowFn(ctx, sql, args...)
	}
	return &fakeRow{err: errors.New("fakeTx.QueryRow not stubbed")}
}

func (f *fakeTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.execFn != nil {
		return f.execFn(ctx, sql, args...)
	}
	return pgconn.CommandTag{}, nil
}

// injectTx puts fakeTx into ctx so withPool picks it up instead of the pool.
func injectTx(ctx context.Context, tx pgx.Tx) context.Context {
	return WithTx(ctx, tx)
}

// ── fakeRow ────────────────────────────────────────────────────────────

// fakeRow is a scripted pgx.Row — Scan copies values via reflection into
// the dest pointers, or returns err.
type fakeRow struct {
	values []any
	err    error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	return scanInto(dest, r.values)
}

// ── fakeRows ───────────────────────────────────────────────────────────

// fakeRows plays a scripted sequence of rows. Each element in scripted is
// one row's values in the SAME ORDER as the SELECT columns.
type fakeRows struct {
	pgx.Rows
	scripted [][]any
	idx      int
	scanErr  error
	iterErr  error
	closed   bool
}

func (r *fakeRows) Next() bool {
	if r.iterErr != nil {
		return false
	}
	return r.idx < len(r.scripted)
}

func (r *fakeRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.scripted[r.idx]
	r.idx++
	return scanInto(dest, row)
}

func (r *fakeRows) Err() error { return r.iterErr }
func (r *fakeRows) Close()     { r.closed = true }
func (r *fakeRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

// ── scanInto — reflection-based value copy from srcs into dest pointers ─

// scanInto mirrors pgx's row.Scan behavior for the primitives + wrapper
// types our repositories use: uuid.UUID, string, int64, time.Time,
// *time.Time. If a dest is a *foo and src is a foo (or convertible), it
// copies via reflection; otherwise returns an error the test can assert.
func scanInto(dest []any, srcs []any) error {
	if len(dest) != len(srcs) {
		return errors.New("fake scan: dest/src length mismatch")
	}
	for i, d := range dest {
		dv := reflect.ValueOf(d)
		if dv.Kind() != reflect.Pointer || dv.IsNil() {
			return errors.New("fake scan: dest must be a non-nil pointer")
		}
		sv := reflect.ValueOf(srcs[i])
		dt := dv.Elem().Type()
		if !sv.IsValid() {
			// nil src → leave dest at zero.
			dv.Elem().Set(reflect.Zero(dt))
			continue
		}
		if sv.Type().AssignableTo(dt) {
			dv.Elem().Set(sv)
			continue
		}
		if sv.Type().ConvertibleTo(dt) {
			dv.Elem().Set(sv.Convert(dt))
			continue
		}
		return errors.New("fake scan: incompatible types at index " +
			dt.String() + " ← " + sv.Type().String())
	}
	return nil
}
