import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import MaintenanceBanner from '../MaintenanceBanner.vue'
import { formatBeijingTime } from '@/utils/maintenance'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ locale: 'zh-CN', t: (_key: string, values: { time: string }) => `维护 ${values.time} 北京时间` }) }))

describe('MaintenanceBanner', () => {
  const fetchMock = vi.fn()
  beforeEach(() => { vi.useFakeTimers(); vi.stubGlobal('fetch', fetchMock); fetchMock.mockReset() })
  afterEach(() => { vi.useRealTimers(); vi.unstubAllGlobals() })

  it('fetches without tokens immediately and syncs cancellation within 15 seconds', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ maintenance: false, scheduled_at: '2026-10-16T20:00:00Z' }) })
    const wrapper = mount(MaintenanceBanner)
    await flushPromises()
    expect(fetchMock).toHaveBeenCalledWith('/kano-maintenance/status', expect.objectContaining({ cache: 'no-store', credentials: 'omit' }))
    expect(wrapper.text()).toContain('2026/10/17 04:00')
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ maintenance: false }) })
    await vi.advanceTimersByTimeAsync(10000)
    await flushPromises()
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    wrapper.unmount()
    const calls = fetchMock.mock.calls.length
    await vi.advanceTimersByTimeAsync(15000)
    expect(fetchMock).toHaveBeenCalledTimes(calls)
  })

  it('refreshes on admin mutation and focus, preserving the last notice on a short outage', async () => {
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ maintenance: false, scheduled_at: '2026-10-16T20:00:00Z' }) })
    const wrapper = mount(MaintenanceBanner)
    await flushPromises()
    fetchMock.mockRejectedValue(new Error('socket restarting'))
    window.dispatchEvent(new Event('focus'))
    await flushPromises()
    expect(wrapper.text()).toContain('04:00')
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ maintenance: false }) })
    window.dispatchEvent(new Event('kano-maintenance-refresh'))
    await flushPromises()
    expect(wrapper.find('aside').exists()).toBe(false)
    wrapper.unmount()
  })

  it('ignores malformed status and invalid dates', async () => {
    expect(formatBeijingTime('invalid')).toBe('')
    fetchMock.mockResolvedValue({ ok: true, json: async () => ({ scheduled_at: 'invalid' }) })
    const wrapper = mount(MaintenanceBanner)
    await flushPromises()
    expect(wrapper.find('aside').exists()).toBe(false)
    wrapper.unmount()
  })
})
