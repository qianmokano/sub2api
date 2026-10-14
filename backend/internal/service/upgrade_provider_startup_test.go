package service

import (
	"context"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/kanoupdate"
	"github.com/stretchr/testify/require"
)

type providerUpgradeStatus struct{ complete atomic.Bool }

func (s *providerUpgradeStatus) Current(context.Context) (*kanoupdate.Session, error) {
	status := kanoupdate.Checking
	if s.complete.Load() {
		status = kanoupdate.Succeeded
	}
	return &kanoupdate.Session{Job: &kanoupdate.Job{Status: status}}, nil
}

func TestUpstreamBillingProviderWaitsForUpgrade(t *testing.T) {
	// The application has one process-wide coordinator. Keep it out of other
	// service tests, including parallel tests, by using an isolated test process.
	if os.Getenv("KANO_UPGRADE_PROVIDER_CHILD") != "1" {
		executable, err := os.Executable()
		require.NoError(t, err)
		command := exec.Command(executable, "-test.run=^TestUpstreamBillingProviderWaitsForUpgrade$", "-test.timeout=15s")
		command.Env = append(os.Environ(), "KANO_UPGRADE_PROVIDER_CHILD=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}

	status := &providerUpgradeStatus{}
	stop := kanoupdate.GuardBackgroundStartup(context.Background(), status)
	defer stop()
	repository := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		1: {ID: 1, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Status: StatusActive, Credentials: map[string]any{"api_key": "fixture-no-request"},
			Extra: map[string]any{UpstreamBillingProbeEnabledExtraKey: true}},
	}}
	// A missing transport produces the normal persisted failure snapshot without
	// making a model, billing or external HTTP request.
	service := ProvideUpstreamBillingProbeService(repository, nil, nil, nil, nil)
	defer service.Stop()
	wroteSnapshot := func() bool {
		repository.mu.Lock()
		defer repository.mu.Unlock()
		return repository.accounts[1].Extra[UpstreamBillingProbeExtraKey] != nil
	}
	require.Never(t, wroteSnapshot, 150*time.Millisecond, 5*time.Millisecond,
		"an enabled due account must remain unchanged during upgrade validation")
	status.complete.Store(true)
	require.Eventually(t, wroteSnapshot, 5*time.Second, 10*time.Millisecond,
		"the original enabled probe must resume after public commit")
}
