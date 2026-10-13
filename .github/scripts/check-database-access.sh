#!/usr/bin/env bash
# Enforces the Enterprise Platform Database Access Standard: every database
# connection, configuration, and operation in this service passes through
# platform-pgcommon (pgcommon.NewPool, pgcommon.RunInTx*,
# pgcommon.ConfigFromEnv, pgcommon.MigrationDSNFromEnv).
#
# This script is a belt-and-suspenders complement to golangci-lint's
# forbidigo rule (.golangci.yml), which bans pgxpool.New/Connect and
# pgx.Connect at the call-expression level. That rule gates on the linter
# passing; this script gates independently on a plain grep/AST scan so the
# enforcement survives a linter version upgrade that changes how forbidigo
# resolves qualified identifiers.
#
# Rules enforced:
#   1. No database/sql import in production code — this service uses pgx/
#      pgcommon exclusively; sql.Open would bypass the entire pgcommon stack.
#   2. No pgxpool.New / pgxpool.NewWithConfig / pgxpool.Connect calls —
#      pools must be created via pgcommon.NewPool (postgres/db.go,
#      test/dbseed/pool.go); verified zero call sites before adding
#      (belt-and-suspenders to forbidigo).
#   3. No pgx.Connect / pgx.ConnectConfig calls — same rationale.
#   4. No bare pgcommon.Config{ DSN: ... } struct literals for the system
#      pool — must use pgadapter.SystemPoolConfig so PGBouncerMode is
#      forced, GUCProvider is cleared, and pool sizing is inherited from
#      ConfigFromEnv. A bare literal silently drops PGBouncerMode=true,
#      breaking transaction pooling with "prepared statement already
#      exists" (pgcommon architecture note, cmd/server/main.go wiring
#      comment).
#   5. Every pgcommon.RunInTx / pgcommon.RunInTxWithRetryOpts call in the
#      postgres adapter (db.go) returns through wrapConnErr — the domain
#      error translation layer that maps connectivity/resource SQLSTATE
#      classes to domain.ErrDBUnavailable (503) and deadlock/serialization
#      failures to domain.ErrConflict (409 concurrent_update) so handlers
#      never see raw *pgconn.PgError shapes.
#
# Scope: internal/, cmd/, pkg/ only. Tests are excluded: integration tests
# legitimately use pgxpool.Conn / pgx.Tx types as parameter types
# (test/dbseed/pool.go, test/e2e/security_test.go) without constructing
# pools themselves, and that is architecturally correct.
set -euo pipefail

python3 - <<'PYEOF'
import re
import sys
from pathlib import Path

ROOTS = ["internal", "cmd", "pkg"]
errors = []

def production_go_files():
    for root in ROOTS:
        for path in Path(root).rglob("*.go"):
            if not path.as_posix().endswith("_test.go"):
                yield path

# ── Rule 1: no database/sql import ──────────────────────────────────────────
SQL_IMPORT_RE = re.compile(r'"database/sql"')
for path in production_go_files():
    text = path.read_text(encoding="utf-8")
    for i, line in enumerate(text.splitlines(), 1):
        if SQL_IMPORT_RE.search(line):
            errors.append(
                f"{path}:{i}: 'database/sql' import — "
                "this service uses pgx/pgcommon only; "
                "sql.Open bypasses the pgcommon pool, GUC binding, and tracing stack"
            )

# ── Rule 2: no pgxpool.New / NewWithConfig / Connect calls ──────────────────
PGXPOOL_CALL_RE = re.compile(r'\bpgxpool\.(New|NewWithConfig|Connect)\s*\(')
for path in production_go_files():
    text = path.read_text(encoding="utf-8")
    for i, line in enumerate(text.splitlines(), 1):
        if PGXPOOL_CALL_RE.search(line):
            errors.append(
                f"{path}:{i}: direct pgxpool.{PGXPOOL_CALL_RE.search(line).group(1)}() — "
                "construct pools only via pgcommon.NewPool "
                "(internal/adapter/outbound/postgres/db.go or test/dbseed/pool.go)"
            )

# ── Rule 3: no pgx.Connect / pgx.ConnectConfig calls ───────────────────────
PGX_CONNECT_RE = re.compile(r'\bpgx\.Connect(Config)?\s*\(')
for path in production_go_files():
    text = path.read_text(encoding="utf-8")
    for i, line in enumerate(text.splitlines(), 1):
        if PGX_CONNECT_RE.search(line):
            errors.append(
                f"{path}:{i}: direct pgx.Connect() — "
                "construct connections only via pgcommon.NewPool"
            )

# ── Rule 4: no bare pgcommon.Config{DSN: ...} literal for system pool ───────
# Detect multi-line pgcommon.Config{ ... DSN: ... } struct literals where
# the caller has NOT used pgadapter.SystemPoolConfig or ConfigFromEnv.
# Heuristic: pgcommon.Config{ followed by DSN: within 10 lines, without
# pgadapter.SystemPoolConfig or pgcommon.ConfigFromEnv on the same construct.
# We scan backtick and plain string literals inside Go struct literals only.
BARE_SYS_CONFIG_RE = re.compile(
    r'pgcommon\.Config\s*\{[^}]*\bDSN\s*:', re.DOTALL
)

# The two ALLOWED construction sites: SystemPoolConfig (in db.go) and
# ConfigFromEnv wiring (also in db.go). They call ConfigFromEnv first and
# then override DSN — that's the approved pattern.
ALLOWED_CALLERS = {
    "internal/adapter/outbound/postgres/db.go",
    "test/dbseed/pool.go",
}
for path in production_go_files():
    posix = path.as_posix()
    if posix in ALLOWED_CALLERS:
        continue
    text = path.read_text(encoding="utf-8")
    for m in BARE_SYS_CONFIG_RE.finditer(text):
        line = text.count("\n", 0, m.start()) + 1
        errors.append(
            f"{posix}:{line}: bare pgcommon.Config{{DSN: ...}} struct literal — "
            "use pgadapter.SystemPoolConfig(dsn, log) for the BYPASSRLS system pool "
            "(ensures PGBouncerMode=true, clears GUCProvider, inherits ConfigFromEnv sizing) "
            "or pgcommon.ConfigFromEnv() + field overrides for any other pool"
        )

# ── Rule 5: wrapConnErr wraps every pgcommon.RunInTx* in db.go ──────────────
# The TxRunner.RunInTx and withPool helpers in postgres/db.go MUST route
# every pgcommon.RunInTx* return through wrapConnErr so SQLSTATE connectivity/
# resource classes become domain.ErrDBUnavailable instead of raw PgError.
# Verify the pattern: every `pgcommon.RunInTx` call site in db.go appears
# inside a wrapConnErr(...) call.
DB_PATH = Path("internal/adapter/outbound/postgres/db.go")
if DB_PATH.exists():
    db_text = DB_PATH.read_text(encoding="utf-8")
    # Find lines with pgcommon.RunInTx (not just comments)
    for i, line in enumerate(db_text.splitlines(), 1):
        stripped = line.strip()
        if re.search(r'pgcommon\.RunInTx', stripped) and not stripped.startswith("//"):
            # The RunInTx call must be the argument to wrapConnErr — check
            # that the line (or the line that starts the expression) contains
            # wrapConnErr. A call that begins on a line starting with 'return
            # wrapConnErr(' is the canonical form; `return pgcommon.RunInTx`
            # without wrapConnErr is a violation.
            # Look back up to 3 lines for the wrapConnErr( opening.
            lines = db_text.splitlines()
            context_start = max(0, i - 4)
            context = "\n".join(lines[context_start:i])
            if "wrapConnErr(" not in context:
                errors.append(
                    f"{DB_PATH}:{i}: pgcommon.RunInTx* call not wrapped by wrapConnErr — "
                    "every transaction must route through wrapConnErr so SQLSTATE "
                    "connectivity/resource classes become domain.ErrDBUnavailable (503)"
                )

if errors:
    for e in errors:
        print(f"::error::{e}")
    print(f"\n{len(errors)} violation(s). See .github/scripts/check-database-access.sh for rationale.")
    sys.exit(1)

print("  ✔  database access: all connections via pgcommon.NewPool; "
      "no database/sql; no bare pgxpool/pgx construction; "
      "all RunInTx calls wrapped by wrapConnErr")
PYEOF
