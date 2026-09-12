package service

import (
	"context"
	"database/sql"
	"errors"
	"math"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type DynamicGroupPolicy struct {
	DynamicSubscriptionInput
	GroupID int64 `json:"group_id"`
}

type DynamicGroupQuotaStatus struct {
	Policy        DynamicGroupPolicy   `json:"policy"`
	Sources       []DynamicQuotaSource `json:"sources"`
	Members       int                  `json:"members"`
	DebugMembers  int                  `json:"debug_members"`
	LegacyMembers int                  `json:"legacy_members"`
}

func loadDynamicGroup(ctx context.Context, db dynamicQuotaQuerier, groupID int64) (*DynamicGroupPolicy, error) {
	p := &DynamicGroupPolicy{GroupID: groupID}
	err := db.QueryRowContext(ctx, `SELECT account_id,enabled,weight,max_limit_usd,floor_limit_usd,revision FROM dynamic_group_policies WHERE group_id=$1`, groupID).
		Scan(&p.AccountID, &p.Enabled, &p.Weight, &p.MaxLimitUSD, &p.FloorLimitUSD, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

func (s *DynamicSubscriptionService) GroupStatus(ctx context.Context, groupID int64) (*DynamicGroupQuotaStatus, error) {
	var platform, kind string
	var limit sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT platform,subscription_type,weekly_limit_usd FROM groups WHERE id=$1 AND deleted_at IS NULL`, groupID).Scan(&platform, &kind, &limit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrGroupNotFound
		}
		return nil, err
	}
	p, err := loadDynamicGroup(ctx, s.db, groupID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		if platform != PlatformOpenAI || kind != SubscriptionTypeSubscription {
			return nil, ErrGroupNotSubscriptionType
		}
		p = &DynamicGroupPolicy{GroupID: groupID, DynamicSubscriptionInput: DynamicSubscriptionInput{Weight: 1, MaxLimitUSD: limit.Float64}}
		// Only prefill unanimous live settings. Enabling a group is an explicit save.
		var variants int
		var account sql.NullInt64
		var weight, cap, floor sql.NullFloat64
		err = s.db.QueryRowContext(ctx, `SELECT count(DISTINCT (p.account_id,p.weight,p.max_limit_usd,p.floor_limit_usd)),
 min(p.account_id),min(p.weight),min(p.max_limit_usd),min(p.floor_limit_usd)
 FROM dynamic_subscription_policies p JOIN user_subscriptions us ON us.id=p.subscription_id
 WHERE us.group_id=$1 AND us.deleted_at IS NULL AND p.enabled AND us.status='active' AND us.expires_at>NOW()`, groupID).Scan(&variants, &account, &weight, &cap, &floor)
		if err != nil {
			return nil, err
		}
		if variants == 1 && floor.Valid {
			p.AccountID, p.Weight, p.MaxLimitUSD, p.FloorLimitUSD = account.Int64, weight.Float64, cap.Float64, &floor.Float64
		}
	}
	out := &DynamicGroupQuotaStatus{Policy: *p, Sources: []DynamicQuotaSource{}}
	err = s.db.QueryRowContext(ctx, `SELECT count(*) FILTER(WHERE NOT(us.admin_debug AND u.role='admin')),
 count(*) FILTER(WHERE us.admin_debug AND u.role='admin'),
 count(*) FILTER(WHERE p.enabled AND p.group_revision IS NULL)
 FROM user_subscriptions us JOIN users u ON u.id=us.user_id
 LEFT JOIN dynamic_subscription_policies p ON p.subscription_id=us.id
 WHERE us.group_id=$1 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW() AND u.deleted_at IS NULL AND u.status='active'`, groupID).Scan(&out.Members, &out.DebugMembers, &out.LegacyMembers)
	if err != nil {
		return nil, err
	}
	out.Sources, err = s.groupSources(ctx, groupID)
	return out, err
}

func (s *DynamicSubscriptionService) GroupPolicies(ctx context.Context) ([]DynamicGroupPolicy, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.group_id,p.account_id,p.enabled,p.weight,p.max_limit_usd,p.floor_limit_usd,p.revision
 FROM dynamic_group_policies p JOIN groups g ON g.id=p.group_id WHERE g.deleted_at IS NULL ORDER BY p.group_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DynamicGroupPolicy{}
	for rows.Next() {
		var p DynamicGroupPolicy
		if err = rows.Scan(&p.GroupID, &p.AccountID, &p.Enabled, &p.Weight, &p.MaxLimitUSD, &p.FloorLimitUSD, &p.Revision); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func validateDynamicInput(in *DynamicSubscriptionInput) error {
	if in.FloorLimitUSD == nil || !validDynamicAmount(*in.FloorLimitUSD) || QuantizeUsageBillingAmount(*in.FloorLimitUSD) <= 0 || *in.FloorLimitUSD > in.MaxLimitUSD {
		return infraerrors.BadRequest("INVALID_DYNAMIC_QUOTA_PROTECTION", "Set a positive downward protection amount no greater than the allocation cap")
	}
	if in.Revision < 0 || in.AccountID <= 0 || !validDynamicAmount(in.Weight) || in.Weight < 0.0001 || in.Weight > 1000 || !validDynamicAmount(in.MaxLimitUSD) || in.MaxLimitUSD <= 0 || in.MaxLimitUSD > 1e9 {
		return infraerrors.BadRequest("INVALID_DYNAMIC_QUOTA", "Choose an upstream account, weight and positive allocation cap")
	}
	in.MaxLimitUSD = QuantizeUsageBillingAmount(in.MaxLimitUSD)
	in.Weight = math.Round(in.Weight*1e4) / 1e4
	floor := QuantizeUsageBillingAmount(*in.FloorLimitUSD)
	in.FloorLimitUSD = &floor
	return nil
}

func (s *DynamicSubscriptionService) SaveGroup(ctx context.Context, groupID int64, in DynamicSubscriptionInput) error {
	if s.disabled {
		return infraerrors.BadRequest("DYNAMIC_QUOTA_SIMPLE_MODE", "Dynamic quota requires normal billing mode")
	}
	if err := validateDynamicInput(&in); err != nil {
		return err
	}
	if in.Enabled || in.Revision == 0 {
		account, err := s.accounts.GetByID(ctx, in.AccountID)
		if err != nil {
			return err
		}
		if !account.IsOpenAIOAuth() || account.IsShadow() || account.IsOpenAIAgentIdentity() || account.GetCredential("chatgpt_account_id") == "" {
			return ErrDynamicQuotaBinding
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_pools(account_id) VALUES($1) ON CONFLICT DO NOTHING`, in.AccountID); err != nil {
		return err
	}
	pool, err := lockDynamicPool(ctx, tx, in.AccountID)
	if err != nil {
		return err
	}
	var eligible bool
	err = tx.QueryRowContext(ctx, `SELECT platform='openai' AND subscription_type='subscription'
 AND EXISTS(SELECT 1 FROM account_groups WHERE group_id=g.id AND account_id=$2)
 FROM groups g WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, groupID, in.AccountID).Scan(&eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrGroupNotFound
	}
	if err != nil {
		return err
	}
	old, err := loadDynamicGroup(ctx, tx, groupID)
	if err != nil {
		return err
	}
	if (old == nil && in.Revision != 0) || (old != nil && old.Revision != in.Revision) {
		return ErrDynamicQuotaChanged
	}
	if (old != nil && old.AccountID != in.AccountID) || ((in.Enabled || old == nil) && !eligible) {
		return ErrDynamicQuotaBinding
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_group_policies(group_id,account_id,enabled,weight,max_limit_usd,floor_limit_usd)
 VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(group_id) DO UPDATE SET enabled=EXCLUDED.enabled,weight=EXCLUDED.weight,
 max_limit_usd=EXCLUDED.max_limit_usd,floor_limit_usd=EXCLUDED.floor_limit_usd,revision=dynamic_group_policies.revision+1,updated_at=NOW()`, groupID, in.AccountID, in.Enabled, in.Weight, in.MaxLimitUSD, *in.FloorLimitUSD)
	if err != nil {
		return err
	}
	p := &DynamicGroupPolicy{GroupID: groupID, DynamicSubscriptionInput: in}
	p.Revision++
	rows, err := tx.QueryContext(ctx, `SELECT us.id FROM user_subscriptions us JOIN users u ON u.id=us.user_id
 WHERE us.group_id=$1 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW()
 AND u.deleted_at IS NULL AND u.status='active' ORDER BY us.id`, groupID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.applyGroupPolicyTx(ctx, tx, pool, p, id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, id := range ids {
		if err = s.invalidate(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Called with the source locked, so configuration, allocation and settlement
// cannot interleave. Existing usage, holds and immutable bindings are preserved.
func (s *DynamicSubscriptionService) applyGroupPolicyTx(ctx context.Context, tx *sql.Tx, pool *DynamicQuotaPoolState, p *DynamicGroupPolicy, id int64) error {
	var debug bool
	var revision, source int64
	var groupRevision sql.NullInt64
	var enabled sql.NullBool
	err := tx.QueryRowContext(ctx, `SELECT us.admin_debug AND u.role='admin',COALESCE(d.revision,0),COALESCE(d.account_id,0),d.group_revision,d.enabled
 FROM user_subscriptions us JOIN users u ON u.id=us.user_id LEFT JOIN dynamic_subscription_policies d ON d.subscription_id=us.id
 WHERE us.id=$1 AND us.group_id=$2 AND us.deleted_at IS NULL FOR UPDATE OF us`, id, p.GroupID).Scan(&debug, &revision, &source, &groupRevision, &enabled)
	if err != nil {
		return err
	}
	if !debug && source != 0 && source != p.AccountID {
		return ErrDynamicQuotaBinding
	}
	if groupRevision.Valid && groupRevision.Int64 == p.Revision && enabled.Bool == (p.Enabled && !debug) {
		return nil
	}
	if debug || !p.Enabled {
		// Debug subscriptions have no share or source binding; retain any old ledger.
		_, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET enabled=false,activation_pending=false,
 group_revision=$2,revision=revision+1,updated_at=NOW() WHERE subscription_id=$1`, id, p.Revision)
		return err
	}
	in := p.DynamicSubscriptionInput
	in.Revision = revision
	if err = s.savePolicyTx(ctx, tx, pool, id, in); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET group_revision=$2 WHERE subscription_id=$1`, id, p.Revision)
	return err
}

// Conversion keeps the original policy and receipts for late settlement. Lock
// sources before the subscription, like admission, group saves and billing.
func (s *DynamicSubscriptionService) ConvertToAdminDebug(ctx context.Context, id, actorID int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actorAdmin bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id=$1 AND role='admin' AND status='active' AND deleted_at IS NULL)`, actorID).Scan(&actorAdmin); err != nil {
		return err
	}
	if !actorAdmin {
		return infraerrors.Forbidden("ADMIN_DEBUG_SUBSCRIPTION", "An administrator must perform this conversion")
	}
	rows, err := tx.QueryContext(ctx, `SELECT account_id FROM dynamic_subscription_policies WHERE subscription_id=$1
 UNION SELECT g.account_id FROM dynamic_group_policies g JOIN user_subscriptions us ON us.group_id=g.group_id WHERE us.id=$1 ORDER BY account_id`, id)
	if err != nil {
		return err
	}
	var sources []int64
	for rows.Next() {
		var source int64
		if err = rows.Scan(&source); err != nil {
			rows.Close()
			return err
		}
		sources = append(sources, source)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	locked := map[int64]bool{}
	for _, source := range sources {
		if _, err = lockDynamicPool(ctx, tx, source); err != nil {
			return err
		}
		locked[source] = true
	}
	var admin, debug bool
	var source int64
	err = tx.QueryRowContext(ctx, `SELECT u.role='admin' AND u.status='active',us.admin_debug
 FROM user_subscriptions us JOIN users u ON u.id=us.user_id
 WHERE us.id=$1 AND us.deleted_at IS NULL AND u.deleted_at IS NULL FOR UPDATE OF us`, id).Scan(&admin, &debug)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrSubscriptionNotFound
	}
	if err != nil {
		return err
	}
	if !admin {
		return infraerrors.BadRequest("ADMIN_DEBUG_SUBSCRIPTION", "Debug subscriptions require an administrator account")
	}
	// Use a new statement snapshot after waiting for the subscription row: a
	// concurrent save may have inserted its first policy while we were waiting.
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT account_id FROM dynamic_subscription_policies WHERE subscription_id=$1),0)`, id).Scan(&source); err != nil {
		return err
	}
	if source != 0 && !locked[source] {
		return ErrDynamicQuotaChanged // A concurrent opt-in must be retried in source lock order.
	}
	if !debug {
		if _, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET admin_debug=true,updated_at=NOW() WHERE id=$1`, id); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET enabled=false,activation_pending=false,revision=revision+1,updated_at=NOW()
 WHERE subscription_id=$1 AND (enabled OR activation_pending)`, id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.invalidate(ctx, id)
}

// Enrollment uses only local state and the existing V2 save. It also covers
// assignments made through purchases/redeems and the first turn of an old WS.
func (s *DynamicSubscriptionService) ensureGroupSubscription(ctx context.Context, id int64) (managed, debug bool, err error) {
	if s == nil || s.disabled {
		return false, false, nil
	}
	var groupID, source int64
	var changed bool
	err = s.db.QueryRowContext(ctx, `SELECT g.group_id,g.account_id,us.admin_debug AND u.role='admin',
 (p.group_revision IS DISTINCT FROM g.revision OR p.enabled IS DISTINCT FROM g.enabled)
 AND NOT(us.admin_debug AND u.role='admin' AND NOT COALESCE(p.enabled,false))
 FROM user_subscriptions us JOIN users u ON u.id=us.user_id JOIN dynamic_group_policies g ON g.group_id=us.group_id
 LEFT JOIN dynamic_subscription_policies p ON p.subscription_id=us.id
 WHERE us.id=$1 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW()
 AND u.deleted_at IS NULL AND u.status='active' AND (g.enabled OR p.subscription_id IS NOT NULL)`, id).Scan(&groupID, &source, &debug, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if !changed {
		return true, debug, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback()
	pool, err := lockDynamicPool(ctx, tx, source)
	if err != nil {
		return false, false, err
	}
	p, err := loadDynamicGroup(ctx, tx, groupID)
	if err != nil {
		return false, false, err
	}
	if err = s.applyGroupPolicyTx(ctx, tx, pool, p, id); err != nil {
		return false, false, err
	}
	if err = tx.Commit(); err != nil {
		return false, false, err
	}
	return true, debug, s.invalidate(ctx, id)
}

func (s *DynamicSubscriptionService) syncGroupMembers(ctx context.Context, accountID int64) error {
	rows, err := s.db.QueryContext(ctx, `SELECT us.id FROM dynamic_group_policies g JOIN user_subscriptions us ON us.group_id=g.group_id
 JOIN users u ON u.id=us.user_id LEFT JOIN dynamic_subscription_policies p ON p.subscription_id=us.id
 WHERE g.account_id=$1 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW()
 AND u.deleted_at IS NULL AND u.status='active' AND NOT(us.admin_debug AND u.role='admin')
 AND (p.group_revision IS DISTINCT FROM g.revision OR p.enabled IS DISTINCT FROM g.enabled) ORDER BY us.id`, accountID)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, _, err = s.ensureGroupSubscription(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// Debug is an administrative assignment option, never a user-controlled bypass.
func (s *SubscriptionService) validateAdminDebug(ctx context.Context, in *AssignSubscriptionInput) error {
	if !in.AdminDebug {
		return nil
	}
	if s.entClient == nil || in.AssignedBy <= 0 {
		return infraerrors.BadRequest("ADMIN_DEBUG_SUBSCRIPTION", "Debug subscriptions require an administrator account")
	}
	u, err := s.entClient.User.Get(ctx, in.UserID)
	if err != nil {
		return err
	}
	if u.Role != RoleAdmin {
		return infraerrors.BadRequest("ADMIN_DEBUG_SUBSCRIPTION", "Debug subscriptions require an administrator account")
	}
	return nil
}
