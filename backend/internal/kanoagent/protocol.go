// Package kanoagent owns host deployment operations. The application imports
// only kanoupdate's shared protocol/client, never this privileged executor.
package kanoagent

import "github.com/Wei-Shaw/sub2api/internal/kanoupdate"

type Manifest = kanoupdate.Manifest
type ValidationPolicy = kanoupdate.ValidationPolicy
type ColumnRule = kanoupdate.ColumnRule
type Release = kanoupdate.Release
type ReleaseSource = kanoupdate.ReleaseSource
type StartRequest = kanoupdate.StartRequest
type Job = kanoupdate.Job
type Status = kanoupdate.Status
type Session = kanoupdate.Session
type Availability = kanoupdate.Availability

const (
	AgentVersion     = kanoupdate.AgentVersion
	ProtocolVersion  = kanoupdate.ProtocolVersion
	Repository       = kanoupdate.Repository
	ImageRepository  = kanoupdate.ImageRepository
	Queued           = kanoupdate.Queued
	Pulling          = kanoupdate.Pulling
	Preflight        = kanoupdate.Preflight
	BackingUp        = kanoupdate.BackingUp
	Restoring        = kanoupdate.Restoring
	Verifying        = kanoupdate.Verifying
	Switching        = kanoupdate.Switching
	Checking         = kanoupdate.Checking
	RollingBack      = kanoupdate.RollingBack
	Succeeded        = kanoupdate.Succeeded
	Failed           = kanoupdate.Failed
	FailedRolledBack = kanoupdate.FailedRolledBack
	NeedsAttention   = kanoupdate.NeedsAttention
)

var CompareVersions = kanoupdate.CompareVersions
var ParseVersion = kanoupdate.ParseVersion
var ProtectedSetting = kanoupdate.ProtectedSetting
var ValidReleaseVersion = kanoupdate.ValidReleaseVersion
var ValidDigest = kanoupdate.ValidDigest
var CookieName = kanoupdate.CookieName
