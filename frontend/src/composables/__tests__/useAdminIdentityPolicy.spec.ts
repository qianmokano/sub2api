import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useAdminIdentityPolicy } from '../useAdminIdentityPolicy'

const { getSettings } = vi.hoisted(() => ({ getSettings: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { settings: { getSettings } } }))

describe('administrator account policy', () => {
  beforeEach(() => { getSettings.mockReset() })

  it('closes identity editing until a valid policy has loaded', async () => {
    const policy = useAdminIdentityPolicy()
    expect(policy.onlyEnabled.value).toBe(true)
    getSettings.mockResolvedValue({ sso_only_enabled: false })
    await policy.load()
    expect(policy.ready.value).toBe(true)
    expect(policy.onlyEnabled.value).toBe(false)
    expect(policy.adminURL.value).toBe('')
    getSettings.mockResolvedValue({ sso_only_enabled: true, sso_admin_url: 'https://auth.example/login/built-in' })
    await policy.load()
    expect(policy.onlyEnabled.value).toBe(true)
    expect(policy.adminURL.value).toBe('https://auth.example/login/built-in')
  })

  it.each([null, {}])('rejects missing policy and closes after a refresh failure (%s)', async (response) => {
    const policy = useAdminIdentityPolicy()
    getSettings.mockResolvedValue({ sso_only_enabled: false })
    await policy.load()
    getSettings.mockResolvedValue(response)
    await policy.load()
    expect(policy.failed.value).toBe(true)
    expect(policy.ready.value).toBe(false)
    expect(policy.onlyEnabled.value).toBe(true)
    getSettings.mockRejectedValue(new Error('offline'))
    await policy.load()
    expect(policy.failed.value).toBe(true)
  })

  it('does not let an old response overwrite a later policy', async () => {
    let resolve!: (result: unknown) => void
    getSettings.mockReturnValueOnce(new Promise((r) => { resolve = r }))
    const policy = useAdminIdentityPolicy()
    const old = policy.load()
    getSettings.mockResolvedValue({ sso_only_enabled: true })
    await policy.load()
    resolve({ sso_only_enabled: false })
    await old
    expect(policy.onlyEnabled.value).toBe(true)
  })
})
