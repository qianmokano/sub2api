import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import LoginView from '@/views/auth/LoginView.vue'

const { getPublicSettingsMock, pushMock, loginSSOMock, completeSSOMFAMock, localLoginMock } = vi.hoisted(() => ({
  getPublicSettingsMock: vi.fn(),
  pushMock: vi.fn(), loginSSOMock: vi.fn(), completeSSOMFAMock: vi.fn(), localLoginMock: vi.fn()
}))

const publicSettings = {
  registration_enabled: true,
  turnstile_enabled: false,
  turnstile_site_key: '',
  tencent_captcha_enabled: false,
  tencent_captcha_app_id: '',
  aliyun_captcha_enabled: false,
  aliyun_captcha_scene_id: '',
  aliyun_captcha_prefix: '',
  linuxdo_oauth_enabled: false,
  dingtalk_oauth_enabled: false,
  wechat_oauth_enabled: false,
  backend_mode_enabled: false,
  oidc_oauth_enabled: false,
  oidc_oauth_provider_name: 'OIDC',
  github_oauth_enabled: false,
  google_oauth_enabled: false,
  password_reset_enabled: false,
  passkey_enabled: false,
  login_agreement_enabled: false,
  login_agreement_documents: []
}

vi.mock('vue-router', () => ({
  useRouter: () => ({
    push: pushMock,
    currentRoute: { value: { query: {} } }
  })
}))

vi.mock('vue-i18n', () => ({
  createI18n: () => ({
    global: {
      t: (key: string) => key
    }
  }),
  useI18n: () => ({
    t: (key: string) => key
  })
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => ({
    login: localLoginMock,
    loginSSO: loginSSOMock,
    completeSSOMFA: completeSSOMFAMock,
    loginWithPasskey: vi.fn(),
    login2FA: vi.fn()
  }),
  useAppStore: () => ({
    showError: vi.fn(),
    showSuccess: vi.fn(),
    showWarning: vi.fn()
  })
}))

vi.mock('@/api/auth', () => ({
  buildOAuthLoginStartURL: vi.fn(),
  getPublicSettings: (...args: unknown[]) => getPublicSettingsMock(...args),
  isTotp2FARequired: (response: { requires_2fa?: boolean }) => response.requires_2fa === true,
  isWeChatWebOAuthEnabled: vi.fn(() => false),
  startOAuthLogin: vi.fn()
}))

function mountLogin() {
  return mount(LoginView, {
    global: {
      stubs: {
        AuthLayout: { template: '<div><slot /><slot name="footer" /></div>' },
        DingTalkOAuthSection: true,
        EmailOAuthButtons: true,
        Icon: true,
        LinuxDoOAuthSection: true,
        LoginAgreementPrompt: true,
        OidcOAuthSection: true,
        RouterLink: { template: '<a><slot /></a>' },
        TotpLoginModal: true,
        TurnstileWidget: true,
        WechatOAuthSection: true,
        transition: false
      }
    }
  })
}

describe('LoginView registration entry', () => {
  beforeEach(() => {
    getPublicSettingsMock.mockReset()
    pushMock.mockReset()
    getPublicSettingsMock.mockResolvedValue(publicSettings)
    loginSSOMock.mockReset()
    completeSSOMFAMock.mockReset()
    localLoginMock.mockReset()
  })

  it('shows the registration entry when registration is enabled', async () => {
    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.text()).toContain('auth.signUp')
  })

  it('hides the registration entry when registration is disabled', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      registration_enabled: false
    })

    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.text()).not.toContain('auth.signUp')
  })

  it.each([
    ['https://auth.example.com/forget/sub2api', 'https://auth.example.com/forget/sub2api'],
    ['', 'https://auth.example.com/login/kano']
  ])('links unified password recovery to %j with an account fallback', async (resetURL, expectedURL) => {
    getPublicSettingsMock.mockResolvedValueOnce({
      ...publicSettings,
      sso_enabled: true,
      sso_account_url: 'https://auth.example.com/login/kano',
      sso_password_reset_url: resetURL
    })
    const wrapper = mountLogin()
    await flushPromises()

    expect(wrapper.get(`a[href="${expectedURL}"]`).text()).toBe('auth.forgotPassword')
    wrapper.unmount()
  })

  it('uses Passport account input and keeps MFA retry in the page', async () => {
    getPublicSettingsMock.mockResolvedValueOnce({ ...publicSettings, sso_enabled: true, sso_only_enabled: true, sso_registration_enabled: true, registration_enabled: false })
    loginSSOMock.mockResolvedValue({ requires_sso_mfa: true, challenge: { token: 'challenge', methods: [{ mfa_type: 'otp' }] } })
    completeSSOMFAMock.mockRejectedValueOnce(new Error('Incorrect code')).mockResolvedValueOnce({ requires_2fa: true, temp_token: 'local-totp', user_email_masked: 'u***r@example.com' })
    const wrapper = mountLogin()
    await flushPromises()
    expect(wrapper.get('#email').attributes('type')).toBe('text')
    expect(wrapper.text()).toContain('auth.signUp')
    await wrapper.get('#email').setValue('username')
    await wrapper.get('#password').setValue('password')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(loginSSOMock).toHaveBeenCalledWith(expect.objectContaining({ account: 'username', password: 'password' }))
    expect(localLoginMock).not.toHaveBeenCalled()
    expect(pushMock).not.toHaveBeenCalled()
    await wrapper.get('#sso-code').setValue('wrong')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(wrapper.text()).toContain('Incorrect code')
    await wrapper.get('#sso-code').setValue('123456')
    await wrapper.get('form').trigger('submit.prevent')
    await flushPromises()
    expect(completeSSOMFAMock).toHaveBeenLastCalledWith({ challenge: 'challenge', mfa_type: 'otp', passcode: '123456' })
    expect(wrapper.findComponent({ name: 'TotpLoginModal' }).props('tempToken')).toBe('local-totp')
    expect(pushMock).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('resumes OIDC local MFA and removes its temporary token from the URL', async () => {
    window.history.replaceState(null, '', '/login#sso_totp_token=temporary&email_masked=user&redirect=%2Fkeys')
    const wrapper = mountLogin()
    await flushPromises()
    expect(window.location.hash).toBe('')
    expect(wrapper.findComponent({ name: 'TotpLoginModal' }).props('tempToken')).toBe('temporary')
    expect(pushMock).not.toHaveBeenCalled()
    wrapper.unmount()
    window.history.replaceState(null, '', '/')
  })
})
