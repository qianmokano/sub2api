package kanoupdate

import (
	"encoding/json"
	"strings"
	"time"
)

type Status string

const (
	Queued           Status = "queued"
	Pulling          Status = "pulling"
	Preflight        Status = "preflight"
	BackingUp        Status = "backing_up"
	Restoring        Status = "restoring"
	Verifying        Status = "verifying"
	Switching        Status = "switching"
	Checking         Status = "checking"
	RollingBack      Status = "rolling_back"
	Succeeded        Status = "succeeded"
	Failed           Status = "failed"
	FailedRolledBack Status = "failed_rolled_back"
	NeedsAttention   Status = "needs_attention"
)

func (s Status) Active() bool { return s != Succeeded && s != Failed && s != FailedRolledBack }

func (s Status) Valid() bool {
	switch s {
	case Queued, Pulling, Preflight, BackingUp, Restoring, Verifying, Switching, Checking, RollingBack, Succeeded, Failed, FailedRolledBack, NeedsAttention:
		return true
	}
	return false
}

type Step struct {
	Status    Status    `json:"status"`
	StartedAt time.Time `json:"started_at"`
}

type StartRequest struct {
	Version        string `json:"version"`
	ExpectedDigest string `json:"expected_digest"`
	IdempotencyKey string `json:"idempotency_key"`
	Actor          string `json:"actor"`
}

type Job struct {
	ID            string          `json:"operation_id"`
	SourceVersion string          `json:"source_version"`
	TargetVersion string          `json:"target_version"`
	ImageDigest   string          `json:"image_digest"`
	Status        Status          `json:"status"`
	Message       string          `json:"message"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
	BackupID      string          `json:"backup_id,omitempty"`
	Manifest      Manifest        `json:"-"`
	State         json.RawMessage `json:"-"`
	Actor         string          `json:"-"`
	Steps         []Step          `json:"steps,omitempty"`
}

type Availability struct {
	AgentVersion    string   `json:"agent_version"`
	ProtocolVersion int      `json:"protocol_version"`
	Ready           bool     `json:"ready"`
	Busy            bool     `json:"busy"`
	CurrentVersion  string   `json:"current_version,omitempty"`
	Release         *Release `json:"release,omitempty"`
	Warning         string   `json:"warning,omitempty"`
}

type Session struct {
	Job         *Job   `json:"job"`
	Capability  string `json:"capability"`
	ProgressURL string `json:"progress_url"`
}

func CookieName(jobID string) string { return "kano_update_" + strings.TrimPrefix(jobID, "up-") }
