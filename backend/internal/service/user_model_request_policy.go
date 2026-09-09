package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

const userModelPolicySetting = "user_model_request_policy_enabled"

// Rules are user-global: API keys, groups and transports do not own counters.
// An absent rule imposes no extra model restriction. A limited rule with zero
// quota is still invalid; it never silently becomes unlimited.
type UserModelRequestRule struct {
	Model        string `json:"model"`
	Mode         string `json:"mode"` // deny, unlimited, limited
	RequestLimit int64  `json:"request_limit"`
	WindowHours  int    `json:"window_hours"`
	WindowMode   string `json:"window_mode,omitempty"` // daily (Beijing midnight) or hours; absent means legacy hours
}

type UserModelRequestWindow struct {
	Model    string     `json:"model"`
	Used     int64      `json:"used"`
	ResetsAt *time.Time `json:"resets_at"`
}

type UserModelRequestPolicy struct {
	Enabled  bool                     `json:"enabled"`
	Revision int64                    `json:"revision"`
	Rules    []UserModelRequestRule   `json:"rules"`
	Windows  []UserModelRequestWindow `json:"windows"`
}

type UserModelPolicyService struct{ db *sql.DB }

func NewUserModelPolicyService(db *sql.DB) *UserModelPolicyService {
	return &UserModelPolicyService{db: db}
}

var userModelIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/+\[\]-]{0,199}$`)

// Only established equivalents share a bucket. Do not use the permissive
// upstream fallback normalizer: unknown names must not enter another quota bucket.
// codex-auto-review stays a distinct public selector, not a promise of Luna.
func CanonicalUserModel(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	model = strings.TrimPrefix(model, "models/")
	model = strings.TrimPrefix(model, "openai/")
	for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "extrahigh", "max"} {
		if base, ok := strings.CutSuffix(model, "-"+effort); ok {
			known := base == "gpt-5.6" || base == "gpt-6"
			for _, id := range openai.DefaultModelIDs() {
				known = known || base == id
			}
			if known && strings.HasPrefix(base, "gpt-") {
				model = base
				break
			}
		}
	}
	switch model {
	case "gpt-5.6":
		return "gpt-5.6-sol"
	case "gpt-6":
		return "gpt-6-astra"
	}
	return model
}

func NormalizeUserModelRequestRules(rules []UserModelRequestRule) ([]UserModelRequestRule, error) {
	if len(rules) > 200 {
		return nil, infraerrors.BadRequest("INVALID_MODEL_RULES", "At most 200 model rules are allowed")
	}
	out := make([]UserModelRequestRule, 0, len(rules))
	seen := map[string]bool{}
	for _, rule := range rules {
		rule.Model = CanonicalUserModel(rule.Model)
		if !userModelIDPattern.MatchString(rule.Model) || seen[rule.Model] {
			return nil, infraerrors.BadRequest("INVALID_MODEL_RULES", "Model names must be explicit, unique identifiers (aliases share one rule); wildcards are not allowed")
		}
		seen[rule.Model] = true
		switch rule.Mode {
		case "deny", "unlimited":
			rule.RequestLimit, rule.WindowHours = 0, 0
			rule.WindowMode = ""
		case "limited":
			if rule.RequestLimit < 1 || rule.RequestLimit > 1000000 {
				return nil, infraerrors.BadRequest("INVALID_MODEL_RULES", "Limited rules require 1–1000000 requests")
			}
			switch rule.WindowMode {
			case "daily":
				rule.WindowHours = 0
			case "", "hours":
				rule.WindowMode = "hours" // Preserve already saved hourly rules.
				if rule.WindowHours < 1 || rule.WindowHours > 720 {
					return nil, infraerrors.BadRequest("INVALID_MODEL_RULES", "Hourly rules require 1–720 hours")
				}
			default:
				return nil, infraerrors.BadRequest("INVALID_MODEL_RULES", "Unknown model quota window mode")
			}
		default:
			return nil, infraerrors.BadRequest("INVALID_MODEL_RULES", "Unknown model permission mode")
		}
		out = append(out, rule)
	}
	return out, nil
}

func (p *UserModelRequestPolicy) Rule(model string) (UserModelRequestRule, bool) {
	model = CanonicalUserModel(model)
	for _, rule := range p.Rules {
		if rule.Model == model {
			return rule, true
		}
	}
	return UserModelRequestRule{}, false
}

func (p *UserModelRequestPolicy) Allows(model string) bool {
	if p == nil || !p.Enabled {
		return true
	}
	rule, ok := p.Rule(model)
	return !ok || rule.Mode != "deny"
}

func (p *UserModelRequestPolicy) Allowlist() GroupModelAllowlist {
	a := GroupModelAllowlist{Enabled: true, UserPolicy: p}
	for _, r := range p.Rules {
		if r.Mode != "deny" {
			a.Models = append(a.Models, r.Model)
		}
	}
	return a
}

// Direct PostgreSQL reads deliberately avoid a second permission cache. At the
// current traffic level this is inexpensive and makes the next WS turn observe
// revocations. Database failures never silently remove an enforced restriction.
func (s *UserModelPolicyService) Load(ctx context.Context, userID int64) (*UserModelRequestPolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if s == nil || s.db == nil {
		return nil, modelPolicyUnavailable(nil)
	}
	p := &UserModelRequestPolicy{Rules: []UserModelRequestRule{}, Windows: []UserModelRequestWindow{}}
	var raw []byte
	var mode string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT value FROM settings WHERE key=$2),''),
	 COALESCE(p.revision,0), COALESCE(p.rules,'[]'::jsonb)
	 FROM users u LEFT JOIN user_model_request_policies p ON p.user_id=u.id
	 WHERE u.id=$1 AND u.deleted_at IS NULL`, userID, userModelPolicySetting).Scan(&mode, &p.Revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	if mode != "true" && mode != "false" {
		return nil, modelPolicyUnavailable(nil)
	}
	p.Enabled = mode == "true"
	if err = json.Unmarshal(raw, &p.Rules); err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	p.Rules, err = NormalizeUserModelRequestRules(p.Rules)
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	return p, nil
}

func (s *UserModelPolicyService) Status(ctx context.Context, userID int64) (*UserModelRequestPolicy, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := s.Load(ctx, userID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT model, CASE WHEN resets_at>NOW() THEN used ELSE 0 END,
	 CASE WHEN resets_at>NOW() THEN resets_at END FROM user_model_request_windows WHERE user_id=$1 ORDER BY model`, userID)
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var w UserModelRequestWindow
		if err := rows.Scan(&w.Model, &w.Used, &w.ResetsAt); err != nil {
			return nil, modelPolicyUnavailable(err)
		}
		p.Windows = append(p.Windows, w)
	}
	if err := rows.Err(); err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	return p, nil
}

func (s *UserModelPolicyService) Save(ctx context.Context, userID, revision int64, rules []UserModelRequestRule) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rules, err := NormalizeUserModelRequestRules(rules)
	if err != nil {
		return err
	}
	if revision < 0 {
		return infraerrors.BadRequest("INVALID_REVISION", "Invalid policy revision")
	}
	raw, err := json.Marshal(rules)
	if err != nil {
		return err
	}
	// Optimistic concurrency stops an old modal overwriting another admin's edit.
	result, err := s.db.ExecContext(ctx, `INSERT INTO user_model_request_policies (user_id,revision,rules)
	 SELECT id,1,$3::jsonb FROM users WHERE id=$1 AND deleted_at IS NULL AND $2=0
	 ON CONFLICT (user_id) DO NOTHING`, userID, revision, string(raw))
	if err != nil {
		return modelPolicyUnavailable(err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return modelPolicyUnavailable(err)
	}
	if revision > 0 {
		result, err = s.db.ExecContext(ctx, `UPDATE user_model_request_policies SET rules=$3::jsonb,revision=revision+1,updated_at=NOW()
		 WHERE user_id=$1 AND revision=$2 AND EXISTS(SELECT 1 FROM users WHERE id=$1 AND deleted_at IS NULL)`, userID, revision, string(raw))
		if err != nil {
			return modelPolicyUnavailable(err)
		}
		n, err = result.RowsAffected()
		if err != nil {
			return modelPolicyUnavailable(err)
		}
	}
	if n != 1 {
		return infraerrors.Conflict("MODEL_POLICY_CHANGED", "Policy changed or user is unavailable; reload before saving")
	}
	// Changing the rule does not replenish already consumed requests.
	return nil
}

func (s *UserModelPolicyService) Reset(ctx context.Context, userID int64, model string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := s.Load(ctx, userID)
	if err != nil {
		return err
	}
	rule, ok := p.Rule(model)
	if !ok || rule.Mode != "limited" {
		return infraerrors.BadRequest("INVALID_MODEL_RULE", "Only an existing limited rule can be reset")
	}
	_, err = s.db.ExecContext(ctx, `UPDATE user_model_request_windows SET used=0,generation=generation+1,resets_at=NOW() WHERE user_id=$1 AND model=$2`, userID, rule.Model)
	if err != nil {
		return modelPolicyUnavailable(err)
	}
	return nil
}

// Activation switches the single authority and disables the obsolete group
// allowlists in the same transaction. Unconfigured users need no policy row.
func (s *UserModelPolicyService) Activate(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return modelPolicyUnavailable(err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `UPDATE groups SET model_allowlist=jsonb_set(model_allowlist,'{enabled}','false'::jsonb),updated_at=NOW()
	 WHERE deleted_at IS NULL AND model_allowlist->>'enabled'='true'`)
	if err != nil {
		return modelPolicyUnavailable(err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings(key,value,updated_at) VALUES($1,'true',NOW()) ON CONFLICT(key) DO UPDATE SET value='true',updated_at=NOW()`, userModelPolicySetting)
	if err != nil {
		return modelPolicyUnavailable(err)
	}
	return tx.Commit()
}

type ModelRequestQuotaError struct {
	Model      string
	ResetsAt   time.Time
	RetryAfter int
}

func (e *ModelRequestQuotaError) Error() string {
	return fmt.Sprintf("Model %q request quota exhausted; available again at %s", e.Model, e.ResetsAt.UTC().Format(time.RFC3339))
}

func modelPolicyUnavailable(err error) error {
	return infraerrors.ServiceUnavailable("MODEL_POLICY_UNAVAILABLE", "Model permission service unavailable; forwarding paused").WithCause(err)
}

type UserModelRequestReservation struct {
	service    *UserModelPolicyService
	userID     int64
	model      string
	generation int64
	once       sync.Once
}

// Reserve checks the latest policy while holding a shared policy-row lock and
// increments the bounded window atomically BEFORE dispatch. A crash retains the
// reservation until expiry (conservative), never silently restores quota.
func (s *UserModelPolicyService) Reserve(ctx context.Context, userID int64, model string) (*UserModelRequestReservation, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, err := s.Load(ctx, userID)
	if err != nil || !p.Enabled {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT rules FROM user_model_request_policies WHERE user_id=$1 FOR SHARE`, userID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	if err = json.Unmarshal(raw, &p.Rules); err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	p.Rules, err = NormalizeUserModelRequestRules(p.Rules)
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	rule, ok := p.Rule(model)
	if !ok || rule.Mode == "unlimited" {
		return nil, nil
	}
	if rule.Mode == "deny" {
		return nil, modelNotAllowed(model)
	}
	var gen int64
	err = tx.QueryRowContext(ctx, `INSERT INTO user_model_request_windows(user_id,model,used,resets_at)
	 VALUES($1,$2,1,CASE WHEN $5 THEN
	   (date_trunc('day',statement_timestamp() AT TIME ZONE 'Asia/Shanghai')+INTERVAL '1 day') AT TIME ZONE 'Asia/Shanghai'
	   ELSE statement_timestamp()+make_interval(secs=>$4) END)
	 ON CONFLICT(user_id,model) DO UPDATE SET
	 used=CASE WHEN user_model_request_windows.resets_at<=statement_timestamp() THEN 1 ELSE user_model_request_windows.used+1 END,
	 generation=user_model_request_windows.generation+CASE WHEN user_model_request_windows.resets_at<=statement_timestamp() THEN 1 ELSE 0 END,
	 resets_at=CASE WHEN user_model_request_windows.resets_at<=statement_timestamp() THEN EXCLUDED.resets_at ELSE user_model_request_windows.resets_at END
	 WHERE user_model_request_windows.resets_at<=statement_timestamp() OR user_model_request_windows.used<$3
	 RETURNING generation`, userID, rule.Model, rule.RequestLimit, rule.WindowHours*3600, rule.WindowMode == "daily").Scan(&gen)
	if errors.Is(err, sql.ErrNoRows) {
		q := &ModelRequestQuotaError{Model: rule.Model}
		var seconds float64
		err = tx.QueryRowContext(ctx, `SELECT resets_at,GREATEST(1,EXTRACT(EPOCH FROM resets_at-statement_timestamp())) FROM user_model_request_windows WHERE user_id=$1 AND model=$2`, userID, rule.Model).Scan(&q.ResetsAt, &seconds)
		if err != nil {
			return nil, modelPolicyUnavailable(err)
		}
		q.RetryAfter = int(math.Ceil(seconds))
		return nil, q
	}
	if err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	if err = tx.Commit(); err != nil {
		return nil, modelPolicyUnavailable(err)
	}
	return &UserModelRequestReservation{service: s, userID: userID, model: rule.Model, generation: gen}, nil
}

func modelNotAllowed(model string) error {
	return infraerrors.NotFound("USER_MODEL_NOT_ALLOWED", fmt.Sprintf("Model %q is not authorized for this user", model))
}

// Refund only a definitely unexecuted request. Generation fencing prevents a
// late failure from decrementing a new or manually reset window.
func (r *UserModelRequestReservation) Finish(refund bool) {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if !refund {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, err := r.service.db.ExecContext(ctx, `UPDATE user_model_request_windows SET used=GREATEST(0,used-1) WHERE user_id=$1 AND model=$2 AND generation=$3`, r.userID, r.model, r.generation)
		if err != nil { /* conservative: retain the slot on settlement failure */
			logger.LegacyPrintf("service.user_model_quota", "quota_refund_failed user_id=%d model=%s; reservation retained", r.userID, r.model)
		}
	})
}
