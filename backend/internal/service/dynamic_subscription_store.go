package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/google/uuid"
)

var (
	ErrDynamicQuotaUnavailable = infraerrors.ServiceUnavailable("DYNAMIC_QUOTA_UNAVAILABLE", "Dynamic quota is awaiting a fresh, verified upstream snapshot")
	ErrDynamicQuotaExhausted   = infraerrors.TooManyRequests("DYNAMIC_QUOTA_EXHAUSTED", "Current dynamic quota is exhausted or reserved by in-flight requests")
	ErrDynamicQuotaBinding     = infraerrors.Conflict("DYNAMIC_QUOTA_BINDING_CONFLICT", "The subscription's upstream quota binding does not match this request")
	ErrDynamicQuotaChanged     = infraerrors.Conflict("DYNAMIC_QUOTA_CHANGED", "Dynamic quota settings changed; reload before saving")
)

type DynamicSubscriptionQuota struct {
	GroupManaged                          bool                `json:"group_managed,omitempty"`
	Enabled                               bool                `json:"enabled"`
	Revision                              int64               `json:"revision"`
	AccountID                             int64               `json:"account_id,omitempty"` // Removed from user-facing DTOs.
	Weight                                float64             `json:"weight"`
	MaxLimitUSD                           float64             `json:"max_limit_usd"`
	FloorLimitUSD                         *float64            `json:"floor_limit_usd"`
	RequestedEnabled                      bool                `json:"requested_enabled"`
	ActivationPending                     bool                `json:"activation_pending"`
	NextAdjustmentPercent                 int                 `json:"next_adjustment_percent,omitempty"`
	AllocationBudgetConflict              bool                `json:"allocation_budget_conflict,omitempty"`
	LastAllocationAt                      *time.Time          `json:"last_allocation_at,omitempty"`
	LastChange                            *DynamicQuotaChange `json:"last_change,omitempty"`
	Cycle                                 int64               `json:"cycle"`
	Status                                string              `json:"status"`
	LimitUSD                              float64             `json:"limit_usd"`
	UsedUSD                               float64             `json:"used_usd"`
	RemainingUSD                          float64             `json:"remaining_usd"`
	ReservedUSD                           float64             `json:"reserved_usd"`
	StartedAt                             time.Time           `json:"started_at"`
	ConfirmedAt                           *time.Time          `json:"confirmed_at,omitempty"`
	SyncedAt                              *time.Time          `json:"synced_at,omitempty"`
	ExpectedResetAt                       *time.Time          `json:"expected_reset_at,omitempty"`
	UpdatedAt                             time.Time           `json:"updated_at"`
	CapacityEstimateUSD                   float64             `json:"capacity_estimate_usd,omitempty"` // Admin-only diagnostic.
	SampleCount                           int                 `json:"sample_count,omitempty"`
	GrowthFrozen                          bool                `json:"growth_frozen,omitempty"`
	usedStandard, allocatedStandard, rate float64
	userID, groupID                       int64
	pool                                  DynamicQuotaPoolState
}

func (q *DynamicSubscriptionQuota) Public() *DynamicSubscriptionQuota {
	if q == nil || !q.Enabled {
		return nil
	}
	cp := *q
	cp.AccountID = 0
	cp.CapacityEstimateUSD = 0
	cp.SampleCount = 0
	cp.AllocationBudgetConflict = false
	return &cp
}

func (q *DynamicSubscriptionQuota) checkReady() error {
	if q == nil || !q.Enabled || q.Status == "active" || q.Status == "learning" {
		return nil
	}
	if q.Status == "upstream_reserve" {
		return ErrDynamicQuotaExhausted
	}
	return ErrDynamicQuotaUnavailable
}

type DynamicSubscriptionInput struct {
	Enabled       bool     `json:"enabled"`
	Revision      int64    `json:"revision"`
	AccountID     int64    `json:"account_id"`
	Weight        float64  `json:"weight"`
	MaxLimitUSD   float64  `json:"max_limit_usd"`
	FloorLimitUSD *float64 `json:"floor_limit_usd"`
}

type DynamicSubscriptionService struct {
	db            *sql.DB
	accounts      AccountRepository
	quota         *OpenAIQuotaService
	fetch         func(context.Context, int64) (DynamicQuotaObservation, error)
	subscriptions *SubscriptionService
	stop          chan struct{}
	done          chan struct{}
	stopOnce      sync.Once
	disabled      bool // Simple mode has no canonical billing and cannot use dynamic quotas.
	workerID      string
	stopping      atomic.Bool
	recoveryDone  chan struct{}
	replay        func(context.Context, *UsageBillingCommand) error
	active        sync.Map // Reservation IDs still owned by a running forward call.
}

// One membership definition for polling, allocation and shared-source admission.
const dynamicActiveMemberSQL = `p.enabled AND NOT p.activation_pending AND ` + dynamicEligibleMemberSQL
const dynamicEligibleMemberSQL = `EXISTS(SELECT 1 FROM user_subscriptions us
 JOIN users u ON u.id=us.user_id JOIN groups g ON g.id=us.group_id
 WHERE us.id=p.subscription_id AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW()
 AND u.deleted_at IS NULL AND u.status='active' AND g.deleted_at IS NULL AND g.status='active'
 AND NOT(us.admin_debug AND u.role='admin')
 AND g.platform='openai' AND g.subscription_type='subscription'
 AND EXISTS(SELECT 1 FROM account_groups ag WHERE ag.account_id=p.account_id AND ag.group_id=us.group_id))`

func NewDynamicSubscriptionService(db *sql.DB, accounts AccountRepository, quota *OpenAIQuotaService, subscriptions *SubscriptionService) *DynamicSubscriptionService {
	s := &DynamicSubscriptionService{db: db, accounts: accounts, quota: quota, subscriptions: subscriptions, stop: make(chan struct{}), done: make(chan struct{}), workerID: uuid.NewString(), recoveryDone: make(chan struct{})}
	if quota != nil {
		s.fetch = quota.QueryDynamicUsage
	}
	return s
}

type DynamicQuotaSource struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type DynamicQuotaAdminStatus struct {
	Policy  *DynamicSubscriptionQuota `json:"policy"`
	Sources []DynamicQuotaSource      `json:"sources"`
}

func (s *DynamicSubscriptionService) AdminStatus(ctx context.Context, id int64) (*DynamicQuotaAdminStatus, error) {
	var groupID int64
	var ceiling sql.NullFloat64
	if err := s.db.QueryRowContext(ctx, `SELECT us.group_id,g.weekly_limit_usd FROM user_subscriptions us
 JOIN groups g ON g.id=us.group_id WHERE us.id=$1 AND us.deleted_at IS NULL AND g.deleted_at IS NULL`, id).Scan(&groupID, &ceiling); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrSubscriptionNotFound
		}
		return nil, err
	}
	q, err := s.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if q == nil {
		q = &DynamicSubscriptionQuota{Weight: 1, MaxLimitUSD: ceiling.Float64, Status: "disabled"}
	}
	out := &DynamicQuotaAdminStatus{Policy: q, Sources: []DynamicQuotaSource{}}
	out.Sources, err = s.groupSources(ctx, groupID)
	return out, err
}

func (s *DynamicSubscriptionService) groupSources(ctx context.Context, groupID int64) ([]DynamicQuotaSource, error) {
	out := []DynamicQuotaSource{}
	rows, err := s.db.QueryContext(ctx, `SELECT a.id,a.name
 FROM accounts a JOIN account_groups ag ON ag.account_id=a.id
 WHERE ag.group_id=$1 AND a.deleted_at IS NULL AND a.platform='openai' AND a.type='oauth' ORDER BY a.id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var source DynamicQuotaSource
		if err = rows.Scan(&source.ID, &source.Name); err != nil {
			return nil, err
		}
		account, e := s.accounts.GetByID(ctx, source.ID)
		if e != nil {
			return nil, e
		}
		if !account.IsShadow() && !account.IsOpenAIAgentIdentity() && account.GetCredential("chatgpt_account_id") != "" {
			out = append(out, source)
		}
	}
	return out, rows.Err()
}

// Fresh DB reads are deliberate: a saved opt-in, bound account and remaining
// quota must affect the next HTTP request/WS turn, including existing keys.
func (s *DynamicSubscriptionService) Load(ctx context.Context, subscriptionID int64) (*DynamicSubscriptionQuota, error) {
	if s == nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	managed, debug, err := s.ensureGroupSubscription(ctx, subscriptionID)
	if err != nil {
		return nil, ErrDynamicQuotaUnavailable.WithCause(err)
	}
	q, err := loadDynamicSubscription(ctx, s.db, subscriptionID, time.Now().UTC())
	if q != nil {
		q.GroupManaged = managed
		if debug {
			q.Enabled = false
			q.RequestedEnabled = false
			q.Status = "disabled"
		}
	}
	return q, err
}

type dynamicQuotaQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// Reuse the scheduler's account-over-global threshold resolver. Fresh reads make
// native setting changes effective at admission, without a second pool setting.
// Settlement deliberately does not call this: configuration cannot block billing.
func loadDynamicNativeCeiling(ctx context.Context, db dynamicQuotaQuerier, accountID int64) (float64, error) {
	var extra, settings []byte
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(a.extra,'{}'::jsonb),
 COALESCE((SELECT value FROM settings WHERE key='ops_advanced_settings'),'{}')
 FROM accounts a WHERE a.id=$1`, accountID).Scan(&extra, &settings); err != nil {
		return 0, err
	}
	var account Account
	var config OpsAdvancedSettings
	if err := json.Unmarshal(extra, &account.Extra); err != nil {
		return 0, err
	}
	if resolveAccountExtraBool(account.Extra, "auto_pause_7d_disabled") {
		return 100, nil
	}
	if err := json.Unmarshal(settings, &config); err != nil {
		return 0, err
	}
	_, threshold := resolveOpenAIQuotaAutoPauseThresholds(withOpenAIQuotaAutoPauseSettings(ctx, config.OpenAIAccountQuotaAutoPause), &account)
	if !validDynamicAmount(threshold) {
		return 0, ErrDynamicQuotaUnavailable
	}
	if threshold == 0 {
		return 100, nil
	}
	return threshold * 100, nil
}

func loadDynamicSubscription(ctx context.Context, db dynamicQuotaQuerier, subscriptionID int64, now time.Time) (*DynamicSubscriptionQuota, error) {
	q := &DynamicSubscriptionQuota{}
	var raw []byte
	var nativeStart sql.NullTime
	var change []byte
	var peak Group
	err := db.QueryRowContext(ctx, `SELECT p.enabled,p.revision,p.account_id,p.weight,p.max_limit_usd,
 p.used_standard_usd,p.allocated_standard_usd,
 GREATEST(p.cycle_used_usd,us.weekly_usage_usd),us.user_id,us.group_id,
 COALESCE(r.rate_multiplier,g.rate_multiplier),g.peak_rate_enabled,g.peak_start,g.peak_end,g.peak_rate_multiplier,
 pool.state,p.updated_at,COALESCE(p.cycle_started_at,us.weekly_window_start),
 COALESCE((SELECT sum(hold_standard_usd) FROM dynamic_quota_requests d WHERE
 (d.subscription_id=us.id OR (d.owner_subscription_id=us.id AND d.account_id=p.account_id))
 AND d.status IN ('pending','uncertain') AND d.operator_absorbed_at IS NULL AND d.review_required_at IS NULL AND d.source_closed_at IS NULL),0),
 p.applied_limit_usd,p.floor_limit_usd,p.activation_pending,p.last_change
 FROM dynamic_subscription_policies p JOIN user_subscriptions us ON us.id=p.subscription_id
 JOIN groups g ON g.id=us.group_id JOIN dynamic_quota_pools pool ON pool.account_id=p.account_id
 LEFT JOIN user_group_rate_multipliers r ON r.user_id=us.user_id AND r.group_id=us.group_id
 WHERE us.id=$1 AND us.deleted_at IS NULL`, subscriptionID).Scan(&q.Enabled, &q.Revision, &q.AccountID, &q.Weight, &q.MaxLimitUSD,
		&q.usedStandard, &q.allocatedStandard, &q.UsedUSD, &q.userID, &q.groupID, &q.rate, &peak.PeakRateEnabled, &peak.PeakStart, &peak.PeakEnd, &peak.PeakRateMultiplier,
		&raw, &q.UpdatedAt, &nativeStart, &q.ReservedUSD, &q.LimitUSD, &q.FloorLimitUSD, &q.ActivationPending, &change)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrDynamicQuotaUnavailable.WithCause(err)
	}
	if json.Unmarshal(raw, &q.pool) != nil {
		return nil, ErrDynamicQuotaUnavailable
	}
	q.RequestedEnabled = q.Enabled
	q.Enabled = q.Enabled && !q.ActivationPending
	if q.Enabled {
		if q.pool.ceilingPercent, err = loadDynamicNativeCeiling(ctx, db, q.AccountID); err != nil {
			return nil, ErrDynamicQuotaUnavailable.WithCause(err)
		}
	}
	q.rate *= peak.PeakMultiplierAt(now)
	if !validDynamicAmount(q.rate) || q.rate <= 0 {
		q.Status = "invalid_billing_rate"
		q.LimitUSD = q.UsedUSD
		return q, nil
	}
	q.Cycle, q.Status, q.StartedAt, q.ConfirmedAt = q.pool.Cycle, q.pool.Status, q.pool.StartedAt, q.pool.ConfirmedAt
	if q.Enabled {
		q.Status = q.pool.accessStatus(now)
	}
	if nativeStart.Valid {
		q.StartedAt = nativeStart.Time
	}
	q.CapacityEstimateUSD, q.SampleCount = q.pool.CapacityUSD, len(q.pool.Samples)
	if q.pool.V2 != nil {
		q.AllocationBudgetConflict = q.pool.V2.BudgetConflict
		q.NextAdjustmentPercent = (q.pool.V2.LastNode + 1) * 10
		if float64(q.NextAdjustmentPercent) >= q.pool.stopPercent() {
			q.NextAdjustmentPercent = 0
		}
	}
	if len(change) > 0 {
		if err = json.Unmarshal(change, &q.LastChange); err != nil {
			return nil, ErrDynamicQuotaUnavailable.WithCause(err)
		}
		if q.LastChange != nil {
			q.LastAllocationAt = &q.LastChange.At
		}
	}
	q.GrowthFrozen = q.pool.growthFrozen(now)
	if q.pool.Snapshot != nil {
		q.SyncedAt = &q.pool.Snapshot.FetchedAt
		t := q.pool.Snapshot.ResetAt
		q.ExpectedResetAt = &t
		if !q.pool.trustedSnapshot(now) {
			q.Status = "quota_unavailable"
		} else if q.pool.Snapshot.UsedPercent >= q.pool.stopPercent() && (q.Status == "active" || q.Status == "learning") {
			q.Status = "upstream_reserve"
		}
	}
	q.ReservedUSD = QuantizeUsageBillingAmount(q.ReservedUSD * q.rate)
	// Keep the published allowance stable across billing/discount/peak changes.
	// Admission still honors BOTH the dollar ceiling and the physical-cost share.
	headroom := math.Min(math.Max(0, math.Min(q.LimitUSD, q.MaxLimitUSD)-q.UsedUSD), math.Max(0, q.allocatedStandard-q.usedStandard)*q.rate)
	q.RemainingUSD = QuantizeUsageBillingAmount(math.Max(0, headroom-q.ReservedUSD))
	if q.Enabled && q.pool.CapacityUSD > 0 && (q.Status == "active" || q.Status == "learning") {
		total, held, _, _, err := dynamicPoolTotals(ctx, db, q.AccountID)
		if err != nil {
			return nil, ErrDynamicQuotaUnavailable.WithCause(err)
		}
		sourceRemaining := q.pool.Available(now, total, held)
		q.RemainingUSD = math.Min(q.RemainingUSD, QuantizeUsageBillingAmount(sourceRemaining*q.rate))
		if sourceRemaining <= 0 {
			q.Status = "upstream_reserve"
		}
	}
	if q.Enabled && q.checkReady() != nil {
		q.RemainingUSD = 0
	}
	if q.ActivationPending {
		q.Status = "activation_pending"
	}
	return q, nil
}

func (s *DynamicSubscriptionService) Hydrate(ctx context.Context, sub *UserSubscription) error {
	if s == nil || sub == nil {
		return nil
	}
	q, err := s.Load(ctx, sub.ID)
	if err != nil {
		return err
	}
	sub.DynamicQuota = q
	if q != nil && q.Enabled {
		sub.WeeklyUsageUSD = q.UsedUSD
		sub.WeeklyWindowStart = &q.StartedAt
	}
	return nil
}

func (s *DynamicSubscriptionService) Save(ctx context.Context, subscriptionID int64, in DynamicSubscriptionInput) error {
	if s.disabled {
		return infraerrors.BadRequest("DYNAMIC_QUOTA_SIMPLE_MODE", "Dynamic quota requires normal billing mode")
	}
	if err := validateDynamicInput(&in); err != nil {
		return err
	}
	// Validate before even creating a pool or querying upstream. Recheck under
	// lock below; failed/stale forms must not mutate another subscriber's pool.
	var err error
	var eligible bool
	var revisionBefore int64
	var oldSource int64
	var groupManaged bool
	if err = s.db.QueryRowContext(ctx, `SELECT g.platform='openai' AND g.subscription_type='subscription'
 AND NOT EXISTS(SELECT 1 FROM users u WHERE u.id=us.user_id AND u.role='admin' AND us.admin_debug)
 AND EXISTS(SELECT 1 FROM account_groups WHERE account_id=$2 AND group_id=g.id),
 COALESCE(p.revision,0),COALESCE(p.account_id,0),EXISTS(SELECT 1 FROM dynamic_group_policies WHERE group_id=g.id)
 FROM user_subscriptions us JOIN groups g ON g.id=us.group_id
 LEFT JOIN dynamic_subscription_policies p ON p.subscription_id=us.id
 WHERE us.id=$1 AND us.deleted_at IS NULL AND g.deleted_at IS NULL`, subscriptionID, in.AccountID).Scan(&eligible, &revisionBefore, &oldSource, &groupManaged); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrSubscriptionNotFound
		}
		return err
	}
	if groupManaged {
		return infraerrors.Conflict("DYNAMIC_QUOTA_GROUP_MANAGED", "Configure dynamic quota on the subscription group")
	}
	if (in.Enabled && !eligible) || (oldSource != 0 && oldSource != in.AccountID) {
		return ErrDynamicQuotaBinding
	}
	if revisionBefore != in.Revision {
		return ErrDynamicQuotaChanged
	}
	// Turning off an existing binding must work after source removal or loss of
	// credentials; it still verifies the immutable source and policy revision.
	if in.Enabled || oldSource == 0 {
		account, err := s.accounts.GetByID(ctx, in.AccountID)
		if err != nil {
			return err
		}
		if !eligible || !account.IsOpenAIOAuth() || account.IsShadow() || account.IsOpenAIAgentIdentity() || account.GetCredential("chatgpt_account_id") == "" {
			return infraerrors.BadRequest("UNSUPPORTED_DYNAMIC_QUOTA_ACCOUNT", "Dynamic quota requires a directly authorized OpenAI OAuth account with a global weekly window")
		}
	}
	// Saving never waits for a network query or recovery of existing bills.
	if _, err = s.db.ExecContext(ctx, `INSERT INTO dynamic_quota_pools(account_id) VALUES($1) ON CONFLICT DO NOTHING`, in.AccountID); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	pool, err := lockDynamicPool(ctx, tx, in.AccountID)
	if err != nil {
		return err
	}
	var managed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dynamic_group_policies g JOIN user_subscriptions us ON us.group_id=g.group_id WHERE us.id=$1)`, subscriptionID).Scan(&managed); err != nil {
		return err
	}
	if managed {
		return infraerrors.Conflict("DYNAMIC_QUOTA_GROUP_MANAGED", "Configure dynamic quota on the subscription group")
	}
	if err = s.savePolicyTx(ctx, tx, pool, subscriptionID, in); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.invalidate(ctx, subscriptionID)
}

// The group and individual paths share the same accounting-preserving save.
// The caller holds the source pool lock and owns commit/cache invalidation.
func (s *DynamicSubscriptionService) savePolicyTx(ctx context.Context, tx *sql.Tx, pool *DynamicQuotaPoolState, subscriptionID int64, in DynamicSubscriptionInput) error {
	var err error
	var groupID int64
	var used, rate float64
	var weeklyStart sql.NullTime
	var platform, kind, status string
	var expires time.Time
	var peak Group
	var debug bool
	err = tx.QueryRowContext(ctx, `SELECT us.group_id,us.weekly_usage_usd,us.weekly_window_start,
 COALESCE(r.rate_multiplier,g.rate_multiplier),g.platform,g.subscription_type,us.status,us.expires_at,
 g.peak_rate_enabled,g.peak_start,g.peak_end,g.peak_rate_multiplier,
 (us.admin_debug AND EXISTS(SELECT 1 FROM users WHERE id=us.user_id AND role='admin'))
 FROM user_subscriptions us JOIN groups g ON g.id=us.group_id
 LEFT JOIN user_group_rate_multipliers r ON r.user_id=us.user_id AND r.group_id=us.group_id
 WHERE us.id=$1 AND us.deleted_at IS NULL AND g.deleted_at IS NULL FOR UPDATE OF us`, subscriptionID).
		Scan(&groupID, &used, &weeklyStart, &rate, &platform, &kind, &status, &expires,
			&peak.PeakRateEnabled, &peak.PeakStart, &peak.PeakEnd, &peak.PeakRateMultiplier, &debug)
	if err != nil {
		return err
	}
	if platform != PlatformOpenAI || kind != SubscriptionTypeSubscription || !validDynamicAmount(rate) || rate <= 0 {
		return infraerrors.BadRequest("UNSUPPORTED_DYNAMIC_QUOTA_SUBSCRIPTION", "A positive-rate OpenAI subscription is required")
	}
	if in.Enabled && (status != SubscriptionStatusActive || !expires.After(time.Now())) {
		return ErrSubscriptionExpired
	}
	if in.Enabled && debug {
		return ErrDynamicQuotaBinding
	}
	var bound bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM account_groups WHERE account_id=$1 AND group_id=$2)`, in.AccountID, groupID).Scan(&bound); err != nil {
		return err
	}
	if in.Enabled && !bound {
		return ErrDynamicQuotaBinding
	}
	var oldAccount, revision int64
	var oldEnabled bool
	var oldLimit, oldAllocation, oldStandard float64
	var oldUsed float64
	var oldPending bool
	var oldFloor sql.NullFloat64
	err = tx.QueryRowContext(ctx, `SELECT account_id,revision,enabled,applied_limit_usd,allocated_standard_usd,used_standard_usd,activation_pending,cycle_used_usd,floor_limit_usd FROM dynamic_subscription_policies WHERE subscription_id=$1 FOR UPDATE`, subscriptionID).Scan(&oldAccount, &revision, &oldEnabled, &oldLimit, &oldAllocation, &oldStandard, &oldPending, &oldUsed, &oldFloor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if revision != in.Revision {
		return ErrDynamicQuotaChanged
	}
	// Moving a previously configured policy is deliberately explicit, not a
	// backdoor reset via off/on. A separate reviewed migration is required.
	if oldAccount != 0 && oldAccount != in.AccountID {
		return ErrDynamicQuotaBinding
	}
	if pool.V2 == nil {
		pool.startV2()
	}
	// OFF traffic is not a share member, but on re-enabling its existing native
	// usage must not disappear. Use recorded standard costs, never a new grant.
	var recordedStandard float64
	if weeklyStart.Valid {
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(sum(total_cost),0) FROM usage_logs WHERE subscription_id=$1 AND created_at>=$2`, subscriptionID, weeklyStart.Time).Scan(&recordedStandard); err != nil {
			return err
		}
	}
	seed := math.Max(recordedStandard, oldStandard)
	if !oldFloor.Valid {
		seed = math.Max(seed, used/rate) // Initial native usage may predate retained logs.
	}
	used = math.Max(used, oldUsed)
	rate *= peak.PeakMultiplierAt(time.Now().UTC())
	if !validDynamicAmount(rate) || rate <= 0 {
		return ErrDynamicQuotaUnavailable
	}
	ready := pool.Snapshot != nil && pool.Snapshot.Valid(time.Now()) && (pool.Status == "active" || pool.Status == "learning")
	waiting := in.Enabled && (!oldEnabled || oldPending) && !ready
	limit := in.MaxLimitUSD
	// Before the first learned allocation, the saved cap is the allowance.
	if oldFloor.Valid && (pool.CapacityUSD > 0 || !pool.LastAllocationAt.IsZero()) {
		limit = math.Min(in.MaxLimitUSD, math.Max(*in.FloorLimitUSD, oldLimit))
	}
	allocation := seed + math.Max(0, limit-used)/rate
	if oldFloor.Valid && limit == oldLimit {
		allocation = oldAllocation // Repeated save/off-on cannot refill physical shares.
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_subscription_policies
 (subscription_id,account_id,enabled,weight,max_limit_usd,floor_limit_usd,activation_pending,
 used_standard_usd,allocated_standard_usd,applied_limit_usd,cycle_used_usd,cycle_started_at,last_change)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$11,$9,$10,COALESCE($12::timestamptz,NOW()),
 jsonb_build_object('previous_usd',0,'current_usd',$9::numeric,'reason','initial','at',NOW()))
 ON CONFLICT(subscription_id) DO UPDATE SET enabled=EXCLUDED.enabled,weight=EXCLUDED.weight,
 max_limit_usd=EXCLUDED.max_limit_usd,floor_limit_usd=EXCLUDED.floor_limit_usd,
 activation_pending=EXCLUDED.activation_pending,applied_limit_usd=EXCLUDED.applied_limit_usd,
 last_change=CASE WHEN dynamic_subscription_policies.floor_limit_usd IS NULL
 THEN jsonb_build_object('previous_usd',dynamic_subscription_policies.applied_limit_usd,
 'current_usd',EXCLUDED.applied_limit_usd,'reason','initial','at',NOW())
 WHEN dynamic_subscription_policies.applied_limit_usd IS DISTINCT FROM EXCLUDED.applied_limit_usd
 THEN jsonb_build_object('previous_usd',dynamic_subscription_policies.applied_limit_usd,
 'current_usd',EXCLUDED.applied_limit_usd,'reason','bounds','at',NOW()) ELSE dynamic_subscription_policies.last_change END,
 cycle_used_usd=GREATEST(dynamic_subscription_policies.cycle_used_usd,EXCLUDED.cycle_used_usd),
 cycle_started_at=COALESCE(dynamic_subscription_policies.cycle_started_at,EXCLUDED.cycle_started_at),
 used_standard_usd=GREATEST(dynamic_subscription_policies.used_standard_usd,EXCLUDED.used_standard_usd),
 allocated_standard_usd=EXCLUDED.allocated_standard_usd,
 revision=dynamic_subscription_policies.revision+1,updated_at=NOW()`,
		subscriptionID, in.AccountID, in.Enabled, in.Weight, in.MaxLimitUSD, *in.FloorLimitUSD, waiting, seed, limit, used, allocation, weeklyStart)

	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details) VALUES($1,$2,'policy_updated',jsonb_build_object('subscription_id',$3::bigint,'enabled',$4::boolean,'revision',$5::bigint))`, in.AccountID, pool.Cycle, subscriptionID, in.Enabled, revision+1); err != nil {
		return err
	}
	if err = writeDynamicPool(ctx, tx, in.AccountID, pool); err != nil {
		return err
	}
	return nil
}

func lockDynamicPool(ctx context.Context, tx *sql.Tx, accountID int64) (*DynamicQuotaPoolState, error) {
	var raw []byte
	p := &DynamicQuotaPoolState{}
	if err := tx.QueryRowContext(ctx, `SELECT state FROM dynamic_quota_pools WHERE account_id=$1 FOR UPDATE`, accountID).Scan(&raw); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, err
	}
	return p, nil
}

func writeDynamicPool(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_pools SET state=$2::jsonb,revision=revision+1,updated_at=NOW() WHERE account_id=$1`, accountID, string(raw))
	return err
}

func dynamicPoolTotals(ctx context.Context, db dynamicQuotaQuerier, accountID int64) (total, held, maxCost float64, pending int, err error) {
	// Archived debt never carries forward. A genuinely executing cross-boundary
	// turn still needs a physical hold until it finishes or its process lease dies.
	err = db.QueryRowContext(ctx, `SELECT p.standard_total_usd,COALESCE(h.held,0),p.max_request_usd,COALESCE(h.pending,0)
 FROM dynamic_quota_pools p LEFT JOIN LATERAL
 (SELECT sum(COALESCE(operator_absorbed_standard_usd,review_standard_usd,hold_standard_usd)) AS held,count(*) AS pending FROM dynamic_quota_requests
  WHERE account_id=p.account_id AND status IN ('pending','uncertain')
  AND (source_closed_at IS NULL OR (finished_at IS NULL AND lease_until>NOW()))) h ON true WHERE p.account_id=$1`, accountID).Scan(&total, &held, &maxCost, &pending)
	return
}

// Refresh performs one independent metadata fetch, then commits only if newer
// than the locked snapshot. It never sends model requests or consumes reset cards.
func (s *DynamicSubscriptionService) Refresh(ctx context.Context, accountID int64) error {
	if err := s.syncGroupMembers(ctx, accountID); err != nil {
		return err
	}
	attemptAt := time.Now().UTC()
	if s.fetch == nil {
		return ErrDynamicQuotaUnavailable
	}
	if err := s.recoverBillingReceipts(ctx, accountID); err != nil {
		return err
	}
	// Consumption settling while the network query is in flight may not yet be
	// included upstream. Keep it outside this snapshot's local watermark.
	var totalBefore float64
	if err := s.db.QueryRowContext(ctx, `SELECT standard_total_usd FROM dynamic_quota_pools WHERE account_id=$1`, accountID).Scan(&totalBefore); err != nil {
		return err
	}
	o, err := s.fetch(ctx, accountID)
	if err != nil || !o.Valid(time.Now().UTC()) {
		if guardErr := s.recordRefreshFailure(ctx, accountID, attemptAt); guardErr != nil {
			return ErrDynamicQuotaUnavailable.WithCause(guardErr)
		}
		if err != nil {
			return ErrDynamicQuotaUnavailable.WithCause(err)
		}
		return ErrDynamicQuotaUnavailable
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := lockDynamicPool(ctx, tx, accountID)
	if err != nil {
		return err
	}
	o.LocalStandardTotal = totalBefore
	now := time.Now().UTC()
	confirmed := p.Observe(o, now)
	var resetIDs []int64
	if confirmed {
		if err = absorbDynamicRequests(ctx, tx, accountID, p.Cycle, true); err != nil {
			return err
		}
		rows, e := tx.QueryContext(ctx, `SELECT p.subscription_id,us.weekly_usage_usd FROM dynamic_subscription_policies p
 JOIN user_subscriptions us ON us.id=p.subscription_id WHERE p.account_id=$1 AND `+dynamicActiveMemberSQL+`
 ORDER BY p.subscription_id FOR UPDATE OF us,p`, accountID)
		if e != nil {
			return e
		}
		previous := map[string]float64{}
		for rows.Next() {
			var id int64
			var used float64
			if e = rows.Scan(&id, &used); e != nil {
				rows.Close()
				return e
			}
			resetIDs = append(resetIDs, id)
			previous[fmt.Sprint(id)] = used
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		p.Confirm(now)
		for _, id := range resetIDs {
			if _, err = tx.ExecContext(ctx, `SELECT set_config('sub2api.dynamic_quota_reset',$1,true)`, fmt.Sprint(id)); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE user_subscriptions SET weekly_usage_usd=0,weekly_window_start=$2,updated_at=NOW() WHERE id=$1`, id, now); err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET used_standard_usd=0,allocated_standard_usd=0,applied_limit_usd=0,
 last_change=jsonb_build_object('previous_usd',applied_limit_usd,'current_usd',max_limit_usd,'reason','reset','at',NOW()),
 cycle_used_usd=0,cycle_started_at=NOW(),updated_at=NOW() WHERE subscription_id=$1 AND account_id=$2 AND enabled`, id, accountID); err != nil {
				return err
			}
		}
		details, _ := json.Marshal(map[string]any{"previous_usage_usd": previous, "confirmed_at": now, "expected_reset_at": o.ResetAt})
		if _, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details) VALUES($1,$2,'reset_confirmed',$3::jsonb)`, accountID, p.Cycle, string(details)); err != nil {
			return err
		}
	}
	if p.Snapshot != nil && p.Snapshot.Valid(now) && (p.Status == "active" || p.Status == "learning") {
		if err = activateDynamicV2(ctx, tx, accountID, confirmed, now); err != nil {
			return err
		}
	}
	// Publish once at a new verified upstream 10% node.
	if err = s.reallocateV2(ctx, tx, accountID, p, now); err != nil {
		return err
	}
	if err = recordDynamicGuardTransition(ctx, tx, accountID, p, now); err != nil {
		return err
	}
	if err = writeDynamicPool(ctx, tx, accountID, p); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, id := range resetIDs {
		if err = s.invalidate(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

func dynamicQuotaGuardSignal(p *DynamicQuotaPoolState, now time.Time) string {
	if p.Snapshot == nil {
		return ""
	}
	if status := p.accessStatus(now); status != "active" && status != "learning" {
		return "paused"
	}
	if !p.Snapshot.Valid(now) || p.Health.Failures >= dynamicQuotaGuardChecks ||
		(p.V2 != nil && p.V2.CandidateSamples > 0) {
		return "frozen"
	}
	if p.Health.Failures > 0 {
		return "warning"
	}
	return "recovered"
}

func recordDynamicGuardTransition(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState, now time.Time) error {
	after := dynamicQuotaGuardSignal(p, now)
	before := p.GuardSignal
	p.GuardSignal = after // Compare with the last committed signal, including across clock expiry/restarts.
	if after == before || after == "" || (before == "" && after == "recovered") {
		return nil
	}
	return recordDynamicGuardEvent(ctx, tx, accountID, p, "quota_guard_"+after)
}

func recordDynamicGuardEvent(ctx context.Context, tx *sql.Tx, accountID int64, p *DynamicQuotaPoolState, kind string) error {
	details := map[string]any{"trusted_capacity_usd": p.CapacityUSD, "failures": p.Health.Failures}
	if p.V2 != nil && p.V2.CandidateSamples > 0 {
		details["proposed_capacity_usd"] = p.V2.CandidateUSD
		details["independent_intervals"] = p.V2.CandidateSamples
	}
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_events(account_id,cycle,kind,details) VALUES($1,$2,$3,$4::jsonb)`, accountID, p.Cycle, kind, string(raw))
	return err
}

func (s *DynamicSubscriptionService) recordRefreshFailure(ctx context.Context, accountID int64, at time.Time) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := lockDynamicPool(ctx, tx, accountID)
	if err != nil {
		return err
	}
	p.recordFailure(at)
	if err = recordDynamicGuardTransition(ctx, tx, accountID, p, time.Now().UTC()); err != nil {
		return err
	}
	if err = writeDynamicPool(ctx, tx, accountID, p); err != nil {
		return err
	}
	return tx.Commit()
}

type DynamicQuotaReservation struct {
	ID               string
	AccountID, Cycle int64
	service          *DynamicSubscriptionService
	dispatched       atomic.Bool
}

func (r *DynamicQuotaReservation) MarkDispatched() error {
	if r == nil {
		return nil
	}
	if r.service.stopping.Load() {
		return ErrDynamicQuotaUnavailable
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := r.service.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET dispatched_at=COALESCE(dispatched_at,NOW())
 WHERE id=$1 AND status='pending' AND operator_absorbed_at IS NULL`, r.ID)
	if err != nil {
		return ErrDynamicQuotaUnavailable.WithCause(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return ErrDynamicQuotaUnavailable
	}
	r.dispatched.Store(true)
	return nil
}

func (s *DynamicSubscriptionService) Begin(ctx context.Context, apiKeyID, accountID int64) (_ *DynamicQuotaReservation, returnErr error) {
	defer func() {
		if returnErr != nil && !strings.HasPrefix(infraerrors.Reason(returnErr), "DYNAMIC_QUOTA_") {
			returnErr = ErrDynamicQuotaUnavailable.WithCause(returnErr)
		}
	}()
	if s == nil || s.disabled || apiKeyID <= 0 {
		return nil, nil
	}
	if s.stopping.Load() {
		return nil, ErrDynamicQuotaUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Track OAuth requests even before opt-in. Activation cannot overtake an
	// untracked HTTP request or WS turn that will bill later. OFF traffic remains
	// unlimited by this feature and never consumes a subscriber allocation.
	if _, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_pools(account_id)
 SELECT id FROM accounts WHERE id=$1 AND deleted_at IS NULL AND platform='openai' AND type='oauth'
 ON CONFLICT DO NOTHING`, accountID); err != nil {
		return nil, err
	}
	p, err := lockDynamicPool(ctx, tx, accountID)
	untracked := errors.Is(err, sql.ErrNoRows)
	if err != nil && !untracked {
		return nil, err
	}
	// Load binding even if the selected account is unprotected: otherwise a
	// fallback to an unrelated account would evade both accounting and isolation.
	var subID sql.NullInt64
	var source sql.NullInt64
	var subscriptionActive bool
	var groupID int64
	var debug bool
	err = tx.QueryRowContext(ctx, `SELECT us.id,us.status='active' AND us.expires_at>NOW(),us.group_id,
 (us.admin_debug AND EXISTS(SELECT 1 FROM users WHERE id=us.user_id AND role='admin')) FROM api_keys k
 JOIN user_subscriptions us ON us.user_id=k.user_id AND us.group_id=k.group_id AND us.deleted_at IS NULL
 WHERE k.id=$1 AND k.deleted_at IS NULL FOR SHARE OF us`, apiKeyID).Scan(&subID, &subscriptionActive, &groupID, &debug)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDynamicQuotaUnavailable.WithCause(err)
	}
	if subID.Valid {
		groupPolicy, err := loadDynamicGroup(ctx, tx, groupID)
		if err != nil {
			return nil, err
		}
		if groupPolicy != nil && subscriptionActive && !debug {
			if groupPolicy.Enabled && groupPolicy.AccountID != accountID {
				return nil, ErrDynamicQuotaBinding
			}
			// Recheck under the source lock: a subscription can be created while
			// a group save is committing, after its member list was collected.
			if groupPolicy.AccountID == accountID {
				if untracked {
					return nil, ErrDynamicQuotaUnavailable
				}
				if err = s.applyGroupPolicyTx(ctx, tx, p, groupPolicy, subID.Int64); err != nil {
					return nil, err
				}
			}
		}
	}
	if subID.Valid && !debug {
		err = tx.QueryRowContext(ctx, `SELECT account_id FROM dynamic_subscription_policies WHERE subscription_id=$1 AND enabled AND NOT activation_pending`, subID.Int64).Scan(&source)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
	}
	if source.Valid && source.Int64 != accountID {
		return nil, ErrDynamicQuotaBinding
	}
	if source.Valid && !subscriptionActive {
		return nil, infraerrors.Forbidden("DYNAMIC_QUOTA_SUBSCRIPTION_INACTIVE", "Dynamic subscription is no longer active")
	}
	if untracked {
		if source.Valid {
			return nil, ErrDynamicQuotaUnavailable
		}
		return nil, nil
	}
	var protected bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM dynamic_subscription_policies p WHERE p.account_id=$1 AND `+dynamicActiveMemberSQL+`)`, accountID).Scan(&protected); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	if status := p.accessStatus(now); protected && status != "active" && status != "learning" {
		return nil, ErrDynamicQuotaUnavailable
	}
	if protected {
		if p.ceilingPercent, err = loadDynamicNativeCeiling(ctx, tx, accountID); err != nil {
			return nil, err
		}
	}
	if protected && p.Snapshot.UsedPercent >= p.stopPercent() {
		return nil, ErrDynamicQuotaExhausted
	}
	var identity string
	var bound bool
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(credentials->>'chatgpt_account_id',''),
 EXISTS(SELECT 1 FROM api_keys k JOIN account_groups ag ON ag.group_id=k.group_id WHERE k.id=$2 AND ag.account_id=a.id)
 FROM accounts a WHERE a.id=$1 AND a.deleted_at IS NULL`, accountID, apiKeyID).Scan(&identity, &bound); err != nil {
		return nil, err
	}
	if (protected && shortOpenAIAutoResetHash(identity) != p.Snapshot.Identity) || !bound {
		return nil, ErrDynamicQuotaBinding
	}
	total, held, maxCost, _, err := dynamicPoolTotals(ctx, tx, accountID)
	if err != nil {
		return nil, err
	}
	// Conservative observed maximum, not an asserted bound on arbitrary prompts.
	hold := math.Max(0.01, maxCost)
	if protected && p.CapacityUSD > 0 {
		hold = math.Max(hold, p.CapacityUSD*0.0001)
		if p.Available(now, total, held) < hold {
			return nil, ErrDynamicQuotaExhausted
		}
	}
	var billSub any
	if source.Valid {
		q, e := loadDynamicSubscription(ctx, tx, subID.Int64, now)
		if e != nil {
			return nil, e
		}
		if q == nil || !q.Enabled || q.AccountID != accountID {
			return nil, ErrDynamicQuotaBinding
		}
		if q.rate <= 0 || q.RemainingUSD < hold*q.rate {
			return nil, ErrDynamicQuotaExhausted
		}
		billSub = subID.Int64
	}
	r := &DynamicQuotaReservation{ID: uuid.NewString(), AccountID: accountID, Cycle: p.Cycle, service: s}
	metadata, _ := ctx.Value(dynamicQuotaMetadataKey{}).(dynamicQuotaMetadata)
	metadata.RequestID = resolveUsageBillingRequestID(ctx, "")
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO dynamic_quota_requests(id,account_id,cycle,subscription_id,api_key_id,hold_standard_usd,
 owner_user_id,owner_subscription_id,worker_id,lease_until,request_context)
 SELECT $1,$2,$3,$4,NULLIF($5,0),$6,k.user_id,$7,$8,NOW()+INTERVAL '2 minutes',$9::jsonb FROM api_keys k WHERE k.id=$5`,
		r.ID, accountID, p.Cycle, billSub, apiKeyID, hold, subID, s.workerID, string(raw))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	s.active.Store(r.ID, struct{}{})
	return r, nil
}

func (r *DynamicQuotaReservation) Finish(result *OpenAIForwardResult, err error, wroteOutput bool) {
	if r == nil {
		return
	}
	defer r.service.active.Delete(r.ID)
	if result == nil && !r.dispatched.Load() && !wroteOutput {
		r.RejectBeforeForward()
		return
	}
	if result != nil {
		result.DynamicQuotaReservationID = r.ID
	}
	status := "uncertain"
	outcome := "missing_usage"
	if result.HasBillableUsage() {
		status, outcome = "pending", "usage_received"
	} else if dynamicQuotaKnownRejection(err) && !wroteOutput {
		status = "rejected"
		outcome = "upstream_rejected"
		if result != nil {
			result.DynamicQuotaReservationID = ""
		}
	}
	if result != nil {
		result.DynamicQuotaUncertain = status == "uncertain"
	}
	// This is evidence, not a bill: no estimates, prompts, headers or credentials.
	evidence := map[string]any{"wrote_output": wroteOutput, "had_error": err != nil}
	if result != nil {
		evidence["request_id"], evidence["model"] = result.RequestID, result.Model
		evidence["usage"], evidence["terminal_event"] = result.Usage, result.UpstreamTerminalEvent
		evidence["image_count"], evidence["video_count"] = result.ImageCount, result.VideoCount
	}
	raw, _ := json.Marshal(evidence)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, e := r.service.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET status=$2,finished_at=NOW(),outcome=$3,evidence=$4::jsonb
 WHERE id=$1 AND status IN ('pending','uncertain') AND billing_receipt IS NULL AND operator_absorbed_at IS NULL`, r.ID, status, outcome, string(raw))
	if e != nil {
		logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_settlement_unavailable account=%d", r.AccountID)
		return
	}
	// A late result belongs to its closed/waived request, never the new customer
	// window. Keep separate metadata evidence and stop renewing its live hold.
	if n, _ := res.RowsAffected(); n == 0 {
		if _, e := r.service.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET lease_until=NULL,late_evidence=COALESCE(late_evidence,$2::jsonb)
 WHERE id=$1 AND operator_absorbed_at IS NOT NULL`, r.ID, string(raw)); e != nil {
			logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_late_evidence_unavailable account=%d", r.AccountID)
		}
	} else if status == "uncertain" {
		logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_accounting_review_required account=%d reservation=%s", r.AccountID, r.ID)
	}
}

func dynamicQuotaKnownRejection(err error) bool {
	var rejected *UpstreamFailoverError
	if !errors.As(err, &rejected) {
		return false
	}
	// 5xx and transport-generated 502 do not prove that upstream did no work.
	switch rejected.StatusCode {
	case 400, 401, 403, 404, 405, 413, 422, 429:
		return true
	default:
		return false
	}
}

func (r *DynamicQuotaReservation) RejectBeforeForward() {
	if r == nil {
		return
	}
	defer r.service.active.Delete(r.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := r.service.db.ExecContext(ctx, `UPDATE dynamic_quota_requests SET status='rejected',outcome='not_forwarded',finished_at=NOW()
 WHERE id=$1 AND status='pending' AND billing_receipt IS NULL AND operator_absorbed_at IS NULL`, r.ID); err != nil {
		logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_release_unavailable account=%d", r.AccountID)
	}
}

func (s *DynamicSubscriptionService) invalidate(ctx context.Context, id int64) error {
	if s.subscriptions == nil {
		return nil
	}
	var userID, groupID int64
	if err := s.db.QueryRowContext(ctx, `SELECT user_id,group_id FROM user_subscriptions WHERE id=$1`, id).Scan(&userID, &groupID); err != nil {
		return err
	}
	return s.subscriptions.invalidateSubscriptionCaches(userID, groupID)
}

// SettleDynamicQuota runs INSIDE the existing billing dedup transaction and
// locks the pool before subscription rows, just like reset/configuration writes.
// A late result is never charged into another account or a freshly reset cycle.
func SettleDynamicQuota(ctx context.Context, tx *sql.Tx, cmd *UsageBillingCommand) error {
	if !validDynamicAmount(cmd.DynamicStandardCost) || !validDynamicAmount(cmd.SubscriptionCost) || !validDynamicAmount(cmd.BalanceCost) {
		return ErrDynamicQuotaUnavailable
	}
	p, err := lockDynamicPool(ctx, tx, cmd.AccountID)
	if err != nil {
		return err
	}
	var accountID, cycle int64
	var subID, keyID sql.NullInt64
	var ownerSubID sql.NullInt64
	var status string
	var windows []byte
	var absorbed bool
	var reviewed bool
	var claim sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT account_id,cycle,subscription_id,api_key_id,status,billing_windows,operator_absorbed_at IS NOT NULL,owner_subscription_id,
 review_required_at IS NOT NULL,review_claim_id FROM dynamic_quota_requests WHERE id=$1 FOR UPDATE`, cmd.DynamicQuotaReservationID).
		Scan(&accountID, &cycle, &subID, &keyID, &status, &windows, &absorbed, &ownerSubID, &reviewed, &claim)
	if err != nil {
		return err
	}
	baseline := cycle == 0 && p.Cycle == 1 && p.ConfirmedAt == nil
	if accountID != cmd.AccountID || (cycle != p.Cycle && !baseline) || !keyID.Valid || keyID.Int64 != cmd.APIKeyID ||
		(subID.Valid && (cmd.SubscriptionID == nil || subID.Int64 != *cmd.SubscriptionID)) {
		return ErrDynamicQuotaBinding
	}
	if absorbed || (status != "pending" && status != "uncertain") {
		return ErrUsageBillingRequestConflict
	}
	if reviewed && (!claim.Valid || claim.String != ctx.Value(dynamicAccountingClaimKey{})) {
		return ErrUsageBillingRequestConflict
	}
	if cmd.SubscriptionID != nil && len(windows) > 0 {
		var current []byte
		if err = tx.QueryRowContext(ctx, `SELECT jsonb_build_array(to_jsonb(us)->'daily_window_start',
 to_jsonb(us)->'weekly_window_start',to_jsonb(us)->'monthly_window_start')
 FROM user_subscriptions us WHERE us.id=$1 FOR UPDATE`, *cmd.SubscriptionID).Scan(&current); err != nil {
			return err
		}
		if string(current) != string(windows) {
			return ErrDynamicQuotaBinding // Never replay an old bill into a new native billing window.
		}
	}
	if subID.Valid {
		res, e := tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET used_standard_usd=used_standard_usd+$3,
 cycle_used_usd=cycle_used_usd+$4,updated_at=NOW()
 WHERE subscription_id=$1 AND account_id=$2`, subID.Int64, accountID, cmd.DynamicStandardCost, cmd.SubscriptionCost)
		if e != nil {
			return e
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			return ErrDynamicQuotaBinding
		}
	} else if ownerSubID.Valid && cmd.SubscriptionID != nil && ownerSubID.Int64 == *cmd.SubscriptionID {
		// A request admitted before opt-in remains owned by the original source.
		// Account for it once even when activation/toggling overtook its response.
		if _, err = tx.ExecContext(ctx, `UPDATE dynamic_subscription_policies SET used_standard_usd=used_standard_usd+$3,
 cycle_used_usd=cycle_used_usd+$4,updated_at=NOW() WHERE subscription_id=$1 AND account_id=$2`,
			ownerSubID.Int64, accountID, cmd.DynamicStandardCost, cmd.SubscriptionCost); err != nil {
			return err
		}
	}
	// Retain the recovery marker until post-commit cache reconciliation succeeds.
	_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET status='settled',outcome='billed',
 billing_retry_at=CASE WHEN billing_receipt IS NOT NULL THEN COALESCE(billing_retry_at,NOW()) END,
 standard_cost_usd=$2,actual_cost_usd=$3,finished_at=NOW() WHERE id=$1`,
		cmd.DynamicQuotaReservationID, cmd.DynamicStandardCost, cmd.SubscriptionCost+cmd.BalanceCost)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE dynamic_quota_pools SET standard_total_usd=standard_total_usd+$2,max_request_usd=GREATEST(max_request_usd,$2) WHERE account_id=$1`, cmd.AccountID, cmd.DynamicStandardCost)
	return err
}

func RejectDuplicateDynamicQuota(ctx context.Context, tx *sql.Tx, cmd *UsageBillingCommand) error {
	if _, err := lockDynamicPool(ctx, tx, cmd.AccountID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE dynamic_quota_requests SET status='rejected',outcome='already_billed',
 billing_retry_at=CASE WHEN billing_receipt IS NOT NULL THEN COALESCE(billing_retry_at,NOW()) END,finished_at=NOW()
 WHERE id=$1 AND account_id=$2 AND api_key_id=$3 AND status IN ('pending','uncertain') AND operator_absorbed_at IS NULL`, cmd.DynamicQuotaReservationID, cmd.AccountID, cmd.APIKeyID)
	return err
}

func (s *DynamicSubscriptionService) Start() {
	go s.runAccountingRecovery()
	go func() {
		defer close(s.done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-s.stop:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
				rows, err := s.db.QueryContext(ctx, `SELECT p.account_id FROM dynamic_subscription_policies p WHERE p.enabled AND `+dynamicEligibleMemberSQL+`
 UNION SELECT g.account_id FROM dynamic_group_policies g WHERE g.enabled AND EXISTS(
 SELECT 1 FROM user_subscriptions us JOIN users u ON u.id=us.user_id WHERE us.group_id=g.group_id
 AND us.deleted_at IS NULL AND us.status='active' AND us.expires_at>NOW() AND u.deleted_at IS NULL AND u.status='active'
 AND NOT(us.admin_debug AND u.role='admin'))
 UNION SELECT account_id FROM dynamic_quota_requests WHERE source_closed_at IS NULL AND status IN ('pending','uncertain') ORDER BY account_id`)
				var ids []int64
				if err == nil {
					for rows.Next() {
						var id int64
						if rows.Scan(&id) == nil {
							ids = append(ids, id)
						}
					}
					err = rows.Err()
					rows.Close()
				}
				if err == nil {
					for _, id := range ids {
						if e := s.Refresh(ctx, id); e != nil {
							logger.LegacyPrintf("service.dynamic_quota", "dynamic_quota_refresh_unavailable account=%d", id)
						}
					}
				}
				cancel()
			}
		}
	}()
}

func (s *DynamicSubscriptionService) Stop() {
	if s != nil {
		s.BeginShutdown()
		s.stopOnce.Do(func() { close(s.stop) })
		select {
		case <-s.done:
		case <-time.After(5 * time.Second):
		}
		select {
		case <-s.recoveryDone:
		case <-time.After(time.Second):
		}
	}
}

// Called before http.Server.Shutdown: hijacked WS sessions can still send turns.
func (s *DynamicSubscriptionService) BeginShutdown() {
	if s != nil {
		s.stopping.Store(true)
	}
}
