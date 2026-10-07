import { beforeEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import VersionBadge from '../VersionBadge.vue'

const { authStore, appStore } = vi.hoisted(() => ({
  authStore: { isAdmin: true },
  appStore: {
    versionLoading: false,
    currentVersion: '',
    latestVersion: '',
    hasUpdate: false,
    releaseInfo: null,
    buildType: 'kano-container',
    fetchVersion: vi.fn(),
    clearVersionCache: vi.fn()
  }
}))

vi.mock('@/stores', () => ({
  useAuthStore: () => authStore,
  useAppStore: () => appStore
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copied: false, copyToClipboard: vi.fn() })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string) => key })
  }
})

describe('VersionBadge version labels', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    authStore.isAdmin = true
    appStore.currentVersion = ''
    appStore.latestVersion = ''
    appStore.hasUpdate = false
  })

  it.each(['v0.2.14-1', 'v0.2.14-kano.1', '0.2.14'])(
    'renders the admin badge and dropdown with one v for %s', async (version) => {
      appStore.currentVersion = version
      const expected = version.startsWith('v') ? version : `v${version}`
      const wrapper = mount(VersionBadge)

      expect(wrapper.find('button').text()).toBe(expected)
      await wrapper.find('button').trigger('click')
      expect(wrapper.find('.text-2xl').text()).toBe(expected)
      expect(wrapper.text()).not.toContain('vv')
      expect(wrapper.text()).not.toContain('version.updateNow')
      wrapper.unmount()
    }
  )

  it('formats a prefixed latest release in the update hint', async () => {
    appStore.currentVersion = 'v0.2.14-1'
    appStore.latestVersion = 'v0.2.14-2'
    appStore.hasUpdate = true
    const wrapper = mount(VersionBadge)
    await wrapper.find('button').trigger('click')

    expect(wrapper.text()).toContain('version.latestVersion: v0.2.14-2')
    expect(wrapper.text()).not.toContain('vv')
    expect(wrapper.text()).not.toContain('version.updateNow')
    wrapper.unmount()
  })

  it('uses the public version prop for a non-admin', () => {
    authStore.isAdmin = false
    const wrapper = mount(VersionBadge, { props: { version: 'v0.2.14-1' } })

    expect(wrapper.text()).toBe('v0.2.14-1')
    expect(appStore.fetchVersion).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('uses the version prop before the admin version is loaded', () => {
    const wrapper = mount(VersionBadge, { props: { version: 'v0.2.14-1' } })

    expect(wrapper.find('button').text()).toBe('v0.2.14-1')
    wrapper.unmount()
  })

  it('keeps the loading placeholder when no version is available', () => {
    const wrapper = mount(VersionBadge)

    expect(wrapper.find('button').text()).toBe('')
    expect(wrapper.find('.animate-pulse').exists()).toBe(true)
    wrapper.unmount()
  })
})
