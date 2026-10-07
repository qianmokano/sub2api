//go:build unit

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAdminSSOCustomerIdentityPolicy(t *testing.T) {
	name, note := "local", "business note"
	cases := []struct {
		name  string
		input UpdateUserInput
	}{
		{"email", UpdateUserInput{Email: "other@example.com"}},
		{"password", UpdateUserInput{Password: "new-password"}},
		{"name", UpdateUserInput{Username: &name}},
		{"mixed business", UpdateUserInput{Username: &name, Notes: &note}},
		{"promotion cannot bypass", UpdateUserInput{Username: &name, Role: RoleAdmin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &userRepoStub{user: &User{ID: 42, Role: RoleUser, Username: "passport", Balance: 12}}
			settings := NewSettingService(&settingRepoStub{values: map[string]string{SettingKeySSOOnlyEnabled: "true"}}, &config.Config{})
			svc := &adminServiceImpl{userRepo: repo, settingService: settings}
			_, err := svc.UpdateUser(context.Background(), 42, &tc.input)
			require.ErrorIs(t, err, ErrSSOOnly)
			require.Empty(t, repo.updated)
			require.Equal(t, "passport", repo.user.Username)
			require.Equal(t, 12.0, repo.user.Balance)
		})
	}
}

func TestAdminSSOKeepsBusinessAndLocalAdministratorEdits(t *testing.T) {
	for _, tc := range []struct {
		name, role, only string
		identity         bool
	}{
		{"customer business", RoleUser, "true", false},
		{"local administrator", RoleAdmin, "true", true},
		{"local mode customer", RoleUser, "false", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &userRepoStub{user: &User{ID: 42, Role: tc.role, Username: "original", Balance: 12}}
			settings := NewSettingService(&settingRepoStub{values: map[string]string{SettingKeySSOOnlyEnabled: tc.only}}, &config.Config{})
			svc := &adminServiceImpl{userRepo: repo, settingService: settings}
			note, name := "business note", "new name"
			input := &UpdateUserInput{Notes: &note}
			if tc.identity {
				input.Username = &name
			}
			user, err := svc.UpdateUser(context.Background(), 42, input)
			require.NoError(t, err)
			require.Equal(t, note, user.Notes)
			require.Equal(t, 12.0, user.Balance)
			require.Len(t, repo.updated, 1)
		})
	}
}

func TestAdminSSOCreationBindingAndSettingFailure(t *testing.T) {
	ctx := context.Background()
	repo := &userRepoStub{user: &User{ID: 42, Role: RoleUser}, nextID: 50}
	settingRepo := &settingRepoStub{values: map[string]string{SettingKeySSOOnlyEnabled: "true"}}
	settings := NewSettingService(settingRepo, &config.Config{})
	svc := &adminServiceImpl{userRepo: repo, settingService: settings}
	for _, role := range []string{"", RoleUser} {
		_, err := svc.CreateUser(ctx, &CreateUserInput{Role: role, Password: "password"})
		require.ErrorIs(t, err, ErrSSOOnly)
	}
	require.Empty(t, repo.created)
	_, err := svc.BindUserAuthIdentity(ctx, 42, AdminBindAuthIdentityInput{})
	require.ErrorIs(t, err, ErrSSOOnly)
	_, err = svc.CreateUser(ctx, &CreateUserInput{Role: RoleAdmin, Password: "password"})
	require.NoError(t, err)
	require.Len(t, repo.created, 1)
	repo.user = &User{ID: 42, Role: RoleUser}
	settingRepo.err = errors.New("settings unavailable")
	name := "new"
	_, err = svc.UpdateUser(ctx, 42, &UpdateUserInput{Username: &name})
	require.ErrorIs(t, err, ErrServiceUnavailable)
	_, err = svc.CreateUser(ctx, &CreateUserInput{Password: "password"})
	require.ErrorIs(t, err, ErrServiceUnavailable)
	require.Empty(t, repo.updated)
	require.NoError(t, settings.RequireLocalUserIdentity(ctx, RoleAdmin, ""))
	require.ErrorIs(t, settings.RequireLocalUserIdentity(ctx, RoleAdmin, RoleUser), ErrServiceUnavailable)
	require.NoError(t, (*SettingService)(nil).RequireLocalUserIdentity(ctx, RoleUser, ""))
}

func TestSSOAdminURL(t *testing.T) {
	svc := NewSettingService(&settingRepoStub{}, &config.Config{})
	for _, tc := range []struct{ issuer, expected string }{
		{"https://auth.example", "https://auth.example/login/built-in"},
		{"https://auth.example/", "https://auth.example/login/built-in"},
		{"http://auth.example", ""},
		{"https://user:secret@auth.example", ""},
		{"https://auth.example/path", ""},
		{"https://auth.example?redirect=bad", ""},
		{"", ""},
	} {
		require.Equal(t, tc.expected, svc.ssoAdminURL(map[string]string{SettingKeySSOIssuerURL: tc.issuer}))
	}
}
