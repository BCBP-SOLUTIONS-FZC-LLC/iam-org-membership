-- Phase 1 · Migration 000002_indexes — LLD §4.2 (partial + hot-path indexes).
--
-- All CREATE INDEX statements live here so 000001_schema stays focused on
-- structure. Partial indexes (WHERE deleted_at IS NULL / WHERE status='pending')
-- enable the "rejoin bug" fixes (TM-11, DM-3, TAE-1, PI-1) while keeping row
-- existence unique. Hot-path indexes annotated with the endpoint they serve.

-- ─────────────────────────────────────────────────────────────────────────
-- rls_violation_log
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX rls_violation_log_occurred_at_idx ON public.rls_violation_log (occurred_at);
CREATE INDEX idx_rls_violation_log_dedup       ON public.rls_violation_log (table_name, violation_type, occurred_at);

-- ─────────────────────────────────────────────────────────────────────────
-- tenants
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_tenants_status              ON public.tenants (status) WHERE deleted_at IS NULL;
CREATE INDEX idx_tenants_trial_end           ON public.tenants (trial_ends_at) WHERE status = 'trial';
-- T-6: realm reservation must persist through offboarding, so NO deleted_at
-- predicate on this partial unique — a soft-deleted dedicated realm still
-- reserves its name until the Keycloak realm itself is hard-deleted.
CREATE UNIQUE INDEX uq_tenants_realm_id_dedicated ON public.tenants (realm_id) WHERE realm_type = 'dedicated';
CREATE INDEX idx_tenants_ownerless           ON public.tenants (ownerless_since) WHERE ownerless_since IS NOT NULL;   -- T-13
CREATE INDEX idx_tenants_realm_sync_pending  ON public.tenants (updated_at)     WHERE realm_sync_pending;             -- T-15
CREATE INDEX idx_tenants_seat_overage        ON public.tenants (overage_since)  WHERE overage_since IS NOT NULL;      -- SEAT-5

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_departments
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_tenant_departments_tenant     ON public.tenant_departments (tenant_id)     WHERE is_active = true;
CREATE INDEX idx_tenant_departments_department ON public.tenant_departments (department_id) WHERE is_active = true;

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_memberships (uq_tm_id_tenant_user already created in 000001)
-- ─────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX uq_tm_active_user      ON public.tenant_memberships (tenant_id, user_id) WHERE deleted_at IS NULL;   -- TM-1/TM-11 rejoin
CREATE INDEX        idx_tm_tenant_id       ON public.tenant_memberships (tenant_id)          WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_user_id         ON public.tenant_memberships (user_id)            WHERE deleted_at IS NULL;
CREATE INDEX        idx_tm_status          ON public.tenant_memberships (tenant_id, status)  WHERE deleted_at IS NULL;
-- Keyset pagination seek key for P-4 (§21.2). Includes id as tiebreaker.
CREATE INDEX        idx_tm_tenant_created  ON public.tenant_memberships (tenant_id, created_at, id) WHERE deleted_at IS NULL;

-- ─────────────────────────────────────────────────────────────────────────
-- tenant_roles
-- ─────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX uq_tenant_roles_active     ON public.tenant_roles (tenant_id, user_id, role_code) WHERE deleted_at IS NULL;  -- TR-2
CREATE INDEX        idx_tenant_roles_tenant_user ON public.tenant_roles (tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_tenant_roles_role      ON public.tenant_roles (tenant_id, role_code) WHERE deleted_at IS NULL;  -- TM-8 last-owner check
CREATE INDEX        idx_tenant_roles_membership ON public.tenant_roles (tenant_membership_id);

-- ─────────────────────────────────────────────────────────────────────────
-- dept_memberships
-- ─────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX uq_dm_active_membership ON public.dept_memberships (tenant_id, user_id, department_id) WHERE deleted_at IS NULL;  -- DM-3
CREATE INDEX        idx_dm_tenant_user      ON public.dept_memberships (tenant_id, user_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_dept      ON public.dept_memberships (tenant_id, department_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_user_role        ON public.dept_memberships (user_id, role_level) WHERE deleted_at IS NULL;
-- AuthZ hot path (I-8): "who in this dept at this level?"
CREATE INDEX        idx_dm_dept_role        ON public.dept_memberships (tenant_id, department_id, role_level) WHERE deleted_at IS NULL;
CREATE INDEX        idx_dm_tenant_membership ON public.dept_memberships (tenant_membership_id);

-- ─────────────────────────────────────────────────────────────────────────
-- dept_role_labels
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_dept_role_labels_tenant ON public.dept_role_labels (tenant_id);

-- ─────────────────────────────────────────────────────────────────────────
-- group_dept_role_mappings / group_tenant_role_mappings / group_dept_mappings
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_gdrm_tenant ON public.group_dept_role_mappings   (tenant_id);
CREATE INDEX idx_gtrm_tenant ON public.group_tenant_role_mappings (tenant_id);
CREATE INDEX idx_gdm_tenant  ON public.group_dept_mappings        (tenant_id);
CREATE INDEX idx_gdm_group   ON public.group_dept_mappings        (tenant_id, keycloak_group_name);

-- ─────────────────────────────────────────────────────────────────────────
-- delegations
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_delegations_tenant        ON public.delegations (tenant_id)                    WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_delegator     ON public.delegations (tenant_id, delegator_id)      WHERE deleted_at IS NULL AND status = 'active';   -- I-8 join
CREATE INDEX idx_delegations_delegate      ON public.delegations (tenant_id, delegate_id)       WHERE deleted_at IS NULL AND status = 'active';
CREATE INDEX idx_delegations_ends_at       ON public.delegations (ends_at)                      WHERE deleted_at IS NULL AND status = 'active' AND ends_at IS NOT NULL;  -- expiry cron
CREATE INDEX idx_delegations_delegator_mem ON public.delegations (delegator_membership_id);
CREATE INDEX idx_delegations_delegate_mem  ON public.delegations (delegate_membership_id);

-- ─────────────────────────────────────────────────────────────────────────
-- tender_acl_entries
-- ─────────────────────────────────────────────────────────────────────────
CREATE UNIQUE INDEX uq_tae_active_entry ON public.tender_acl_entries (tenant_id, tender_id, user_id) WHERE deleted_at IS NULL;  -- TAE-1
CREATE INDEX        idx_tae_tenant_tender ON public.tender_acl_entries (tenant_id, tender_id) WHERE deleted_at IS NULL;
CREATE INDEX        idx_tae_user          ON public.tender_acl_entries (tenant_id, user_id)   WHERE deleted_at IS NULL;
CREATE INDEX        idx_tae_membership    ON public.tender_acl_entries (tenant_membership_id);

-- ─────────────────────────────────────────────────────────────────────────
-- pending_invitations (PI-1)
-- ─────────────────────────────────────────────────────────────────────────
-- Rejoin-friendly partial unique: a terminal (accepted/expired/revoked)
-- invitation does NOT block a fresh 'pending' one for the same email.
CREATE UNIQUE INDEX uq_pi_pending    ON public.pending_invitations (tenant_id, email) WHERE status = 'pending';
CREATE INDEX idx_pi_tenant_pending  ON public.pending_invitations (tenant_id)         WHERE status = 'pending';   -- SEAT-1 pending count / P-30 list
CREATE INDEX idx_pi_expiry          ON public.pending_invitations (expires_at)        WHERE status = 'pending';   -- invitation-expiry CronJob
CREATE INDEX idx_pi_keycloak_user   ON public.pending_invitations (keycloak_user_id)  WHERE status = 'pending';   -- I-3 acceptance match
CREATE INDEX idx_pi_kc_cleanup      ON public.pending_invitations (id)                WHERE kc_cleanup_pending;   -- PI-9 reconciler

-- ─────────────────────────────────────────────────────────────────────────
-- processed_events
-- ─────────────────────────────────────────────────────────────────────────
CREATE INDEX idx_processed_events_prune ON public.processed_events (processed_at);
