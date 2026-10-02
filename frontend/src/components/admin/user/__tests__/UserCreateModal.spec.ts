import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import UserCreateModal from '../UserCreateModal.vue'

const { getSettings, create } = vi.hoisted(() => ({ getSettings: vi.fn(), create: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { settings: { getSettings }, users: { create } } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: vi.fn(), showError: vi.fn() }) }))
vi.mock('@/composables/useStepUp', () => ({
  useStepUp: () => ({ run: (action: () => Promise<unknown>) => action() }),
  isStepUpBlocked: () => false, isStepUpCancelled: () => false, stepUpBlockReason: () => '',
}))
vi.mock('vue-i18n', async (original) => ({
  ...(await original<typeof import('vue-i18n')>()), useI18n: () => ({ t: (key: string) => key }),
}))

const mountModal = () => mount(UserCreateModal, {
  props: { show: true },
  global: { stubs: {
    BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' },
    Icon: true, TotpStepUpDialog: true,
  } },
})

describe('administrator user creation', () => {
  beforeEach(() => { getSettings.mockReset(); create.mockReset(); create.mockResolvedValue({}) })

  it.each([false, true])('uses the permitted role without changing business fields (only=%s)', async (only) => {
    getSettings.mockResolvedValue({ sso_only_enabled: only, sso_admin_url: 'https://auth.example/login/built-in' })
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.find('option[value="user"]').exists()).toBe(!only)
    await wrapper.get('input[type="email"]').setValue('new@example.com')
    await wrapper.get('input[type="text"]').setValue('password')
    await wrapper.get('form').trigger('submit')
    await flushPromises()
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ role: only ? 'admin' : 'user', concurrency: 1, rpm_limit: 0 }))
    if (only) expect(wrapper.get('[data-testid="passport-customer-creation"] a').attributes('href')).toBe('https://auth.example/login/built-in')
  })

  it('keeps creation unavailable after a policy error', async () => {
    getSettings.mockRejectedValue(new Error('offline'))
    const wrapper = mountModal()
    expect(wrapper.find('form').exists()).toBe(false)
    await flushPromises()
    expect(wrapper.text()).toContain('auth.sso.policyUnavailable')
    expect(wrapper.find('form').exists()).toBe(false)
    expect(create).not.toHaveBeenCalled()
  })
})
