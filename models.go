package main

import "time"

type AccountFamily string

const (
	AccountFamilyUnknown AccountFamily = "unknown"
	AccountFamilyWeekly  AccountFamily = "weekly"
	AccountFamilyMonthly AccountFamily = "monthly"
)

type WindowKind string

const (
	WindowFiveHour WindowKind = "five_hour"
	WindowWeekly   WindowKind = "weekly"
	WindowMonthly  WindowKind = "monthly"
)

type QuotaWindow struct {
	Kind               WindowKind `json:"kind"`
	UsedPercent        *float64   `json:"used_percent,omitempty"`
	LimitWindowSeconds *int64     `json:"limit_window_seconds,omitempty"`
	ResetAt            time.Time  `json:"reset_at"`
	Exhausted          bool       `json:"exhausted"`
}

type ResetCredit struct {
	ID        string    `json:"id,omitempty"`
	Status    string    `json:"status,omitempty"`
	GrantedAt time.Time `json:"granted_at,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitempty"`
}

type ParsedQuota struct {
	PlanType                     string        `json:"plan_type,omitempty"`
	Family                       AccountFamily `json:"family"`
	FiveHour                     *QuotaWindow  `json:"five_hour,omitempty"`
	LongWindow                   *QuotaWindow  `json:"long_window,omitempty"`
	CodeReviewWindows            []QuotaWindow `json:"code_review_windows,omitempty"`
	AdditionalWindows            []QuotaWindow `json:"additional_windows,omitempty"`
	ResetCreditsAvailableCount   *int          `json:"reset_credits_available_count,omitempty"`
	ResetCreditsTotalEarnedCount *int          `json:"reset_credits_total_earned_count,omitempty"`
	ResetCredits                 []ResetCredit `json:"reset_credits,omitempty"`
}

type CircuitState string

const (
	CircuitStateClosed   CircuitState = "closed"
	CircuitStateOpen     CircuitState = "open"
	CircuitStateHalfOpen CircuitState = "half_open"
)

type CircuitBreakerState struct {
	State          CircuitState `json:"state,omitempty"`
	EffectiveState CircuitState `json:"effective_state,omitempty"`
	FailureCount   int          `json:"failure_count,omitempty"`
	SuccessCount   int          `json:"success_count,omitempty"`
	OpenedAt       time.Time    `json:"opened_at,omitempty"`
	NextProbeAt    time.Time    `json:"next_probe_at,omitempty"`
	Reason         string       `json:"reason,omitempty"`
	LastFailureAt  time.Time    `json:"last_failure_at,omitempty"`
	LastSuccessAt  time.Time    `json:"last_success_at,omitempty"`
}

type RefreshFailureKind string

const (
	RefreshFailureNone      RefreshFailureKind = ""
	RefreshFailureTransient RefreshFailureKind = "transient"
	RefreshFailureAuth      RefreshFailureKind = "auth"
	RefreshFailureLocal     RefreshFailureKind = "local"
)

type AccountRefreshState struct {
	LastFailureKind RefreshFailureKind
	RetryAttempt    int
	NextRetryAt     time.Time
	AuthFailure     bool
	DueReason       string
	LastFailureAt   time.Time
}

type ResetProbeStatus string

const (
	ResetProbeStatusNone            ResetProbeStatus = ""
	ResetProbeStatusPending         ResetProbeStatus = "pending"
	ResetProbeStatusConfirmedActive ResetProbeStatus = "confirmed_active"
	ResetProbeStatusVerified        ResetProbeStatus = "verified"
	ResetProbeStatusFailed          ResetProbeStatus = "failed"
)

type ResetProbeState struct {
	WindowKind    WindowKind
	WindowSeconds int64
	ResetAt       time.Time
	NextCheckAt   time.Time
	LastProbeAt   time.Time
	VerifiedAt    time.Time
	Attempts      int
	Status        ResetProbeStatus
	Error         string
}

type AccountAnnotation struct {
	Alias             string   `json:"alias,omitempty" yaml:"alias,omitempty"`
	Notes             string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags              []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	GroupID           string   `json:"group_id,omitempty" yaml:"group_id,omitempty"`
	SchedulerPriority int      `json:"scheduler_priority,omitempty" yaml:"scheduler_priority,omitempty"`
}

type GroupAnnotation struct {
	Name  string   `json:"name,omitempty" yaml:"name,omitempty"`
	Notes string   `json:"notes,omitempty" yaml:"notes,omitempty"`
	Tags  []string `json:"tags,omitempty" yaml:"tags,omitempty"`
	Color string   `json:"color,omitempty" yaml:"color,omitempty"`
}

type AnnotationState struct {
	Accounts map[string]AccountAnnotation `json:"accounts,omitempty" yaml:"accounts,omitempty"`
	Groups   map[string]GroupAnnotation   `json:"groups,omitempty" yaml:"groups,omitempty"`
}

type AccountState struct {
	Identity              AccountIdentity
	Instance              AuthInstanceID
	AdmissionEpoch        InstanceAdmissionEpoch
	BindingEpoch          AuthBindingEpoch
	LoginEpoch            LoginEpoch
	TokenEpoch            TokenEpoch
	TierGeneration        TierGeneration
	CredentialFingerprint CredentialFingerprint
	AuthID                string
	AuthIndex             string
	DisplayName           string
	Email                 string
	Provider              string
	Priority              int
	ChatGPTAccountID      string
	Family                AccountFamily
	Quota                 ParsedQuota
	LastRefreshAt         time.Time
	LastSuccessAt         time.Time
	// LastObservedAt records when quota state was last observed inside a real
	// response stream (codex.rate_limits frame or usage-record response
	// headers). It is in-memory only and resets on restart.
	LastObservedAt     time.Time
	LastError          string
	Stale              bool
	TemporaryExhausted bool
	TemporaryResetAt   time.Time
	Circuit            CircuitBreakerState
	Refresh            AccountRefreshState
	ResetProbes        map[WindowKind]ResetProbeState
	Annotation         AccountAnnotation
}

type CPAAdmissionState struct {
	Observed bool
	Priority int
	AuthIDs  map[string]struct{}
	// Tiers lists every admitted CPA priority tier in descending order. It is
	// populated when schedule_across_priorities is enabled and carries the
	// per-tier priority used to keep each account's stored Priority accurate;
	// AuthIDs remains the union of all tiers.
	Tiers []TierAdmission
}

// TierAdmission is one CPA priority tier inside a CPAAdmissionState.
type TierAdmission struct {
	Priority int
	AuthIDs  map[string]struct{}
}

type StateSnapshot struct {
	Config              Config
	Accounts            []AccountState
	CPAAdmission        CPAAdmissionState
	CPAAdmissionVersion uint64
	Annotations         AnnotationState
	Logs                []LogEntry
	LastSelected        string
	LastReason          string
	LastCodexActivityAt time.Time
	LastAuthScanAt      time.Time
	CodexAuthCount      int
	Now                 time.Time
}

type LogEntry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level"`
	Event   string         `json:"event"`
	Message string         `json:"message"`
	Fields  map[string]any `json:"fields,omitempty"`
}
