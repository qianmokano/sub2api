import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ContainerUpgradePanel from '../ContainerUpgradePanel.vue'
import type { ContainerUpdateResult, VersionInfo } from '@/api/admin/system'

const api = vi.hoisted(() => ({ currentContainerUpdate: vi.fn(), startContainerUpdate: vi.fn(), currentUpdateSchedule: vi.fn(), createUpdateSchedule: vi.fn(), cancelUpdateSchedule: vi.fn() }))
vi.mock('@/api/admin/system', () => api)
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function info(): VersionInfo {
  return { current_version: 'v0.2.16-1', latest_version: 'v0.2.16-2', has_update: true, cached: false, build_type: 'kano-container', updater_ready: true, target_image_digest: `sha256:${'a'.repeat(64)}` }
}
function progress(status = 'restoring'): ContainerUpdateResult {
  return { job: { operation_id: `up-${'a'.repeat(32)}`, source_version: 'v0.2.16-1', target_version: 'v0.2.16-2', status, message: 'safe status', created_at: '', updated_at: '' }, progress_url: `/kano-updates/up-${'a'.repeat(32)}` }
}

describe('ContainerUpgradePanel', () => {
  beforeEach(() => { vi.clearAllMocks(); api.currentContainerUpdate.mockResolvedValue({ job: null }) })

  it('explains why an uninstalled updater cannot upgrade', () => {
    const wrapper = mount(ContainerUpgradePanel, { props: { info: { ...info(), updater_ready: false } } })
    expect(wrapper.text()).toContain('version.updaterUnavailable')
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    expect(api.currentContainerUpdate).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('restores an active job and shows its independent progress link', async () => {
    api.currentContainerUpdate.mockResolvedValue(progress())
    const wrapper = mount(ContainerUpgradePanel, { props: { info: info() } })
    await flushPromises()
    expect(wrapper.get('a').attributes('href')).toBe(progress().progress_url)
    expect(wrapper.text()).toContain('version.jobStatus.restoring')
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('submits once with a stable idempotency key and recovers a lost response', async () => {
    let rejectRequest!: (cause: Error) => void
    api.startContainerUpdate.mockImplementation(() => new Promise((_, reject) => { rejectRequest = reject }))
    const wrapper = mount(ContainerUpgradePanel, { props: { info: info() } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await wrapper.get('button').trigger('click')
    expect(api.startContainerUpdate).toHaveBeenCalledTimes(1)
    expect(api.startContainerUpdate).toHaveBeenCalledWith(info().latest_version, info().target_image_digest, expect.any(String))
    api.currentContainerUpdate.mockResolvedValue(progress('checking'))
    rejectRequest(new Error('Response lost'))
    await flushPromises()
    expect(wrapper.text()).toContain('version.jobStatus.checking')
    expect(wrapper.get('a').attributes('href')).toBe(progress().progress_url)
    wrapper.unmount()
  })

  it('shows the final result and ignores untrusted progress URLs', async () => {
    api.currentContainerUpdate.mockResolvedValue({ ...progress('failed_rolled_back'), progress_url: 'https://untrusted.test' })
    const wrapper = mount(ContainerUpgradePanel, { props: { info: info() } })
    await flushPromises()
    expect(wrapper.text()).toContain('version.jobStatus.failed_rolled_back')
    expect(wrapper.find('a').exists()).toBe(false)
    wrapper.unmount()
  })

  it('refreshes a stale version after a completed upgrade', async () => {
    api.currentContainerUpdate.mockResolvedValue(progress('succeeded'))
    const wrapper = mount(ContainerUpgradePanel, { props: { info: info() } })
    await flushPromises()
    expect(wrapper.emitted('refresh')).toHaveLength(1)
    wrapper.unmount()
  })

  it('keeps the same key when retrying an unacknowledged request', async () => {
    api.startContainerUpdate.mockRejectedValue({ response: { data: { message: 'Temporarily unavailable' } } })
    const wrapper = mount(ContainerUpgradePanel, { props: { info: info() } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    const key = api.startContainerUpdate.mock.calls[0][2]
    expect(wrapper.get('[role="alert"]').text()).toBe('Temporarily unavailable')
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(api.startContainerUpdate.mock.calls[1][2]).toBe(key)
    wrapper.unmount()
  })

  it('accepts a task but refuses to navigate to an untrusted destination', async () => {
    api.startContainerUpdate.mockResolvedValue({ ...progress('queued'), progress_url: 'https://untrusted.test' })
    const wrapper = mount(ContainerUpgradePanel, { props: { info: info() } })
    await flushPromises()
    await wrapper.get('button').trigger('click')
    await flushPromises()
    expect(wrapper.text()).toContain('version.jobStatus.queued')
    expect(wrapper.find('a').exists()).toBe(false)
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('shows a deployment blocker and a safe release link', async () => {
    const wrapper = mount(ContainerUpgradePanel, { props: { info: {
      ...info(), unavailable_reason: 'An upgrade is already running',
      release_info: { name: 'release', body: '', published_at: '', html_url: 'https://github.com/qianmokano/sub2api/releases/tag/v0.2.16-2' }
    } } })
    await flushPromises()
    expect(wrapper.text()).toContain('An upgrade is already running')
    expect(wrapper.get('button').attributes('disabled')).toBeDefined()
    expect(wrapper.get('a').attributes('rel')).toBe('noopener noreferrer')
    wrapper.unmount()
  })

  it('confirms the host date and target, then publishes the reservation immediately', async () => {
    const at = '2026-10-17T20:00:00Z'
    api.currentUpdateSchedule.mockResolvedValue({ job: null })
    api.createUpdateSchedule.mockResolvedValue({ ...progress('scheduled'), job: { ...progress('scheduled').job, scheduled_at: at, preparation: 'pending' } })
    const listener = vi.fn()
    window.addEventListener('kano-maintenance-refresh', listener)
    const wrapper = mount(ContainerUpgradePanel, { props: { info: { ...info(), schedule_ready: true, next_scheduled_at: at } } })
    await flushPromises()
    await wrapper.get('[data-testid="schedule-upgrade"]').trigger('click')
    expect(api.createUpdateSchedule).not.toHaveBeenCalled()
    expect(wrapper.findComponent({ name: 'ConfirmDialog' }).props('show')).toBe(true)
    wrapper.findComponent({ name: 'ConfirmDialog' }).vm.$emit('confirm')
    await flushPromises()
    expect(api.createUpdateSchedule).toHaveBeenCalledWith(info().latest_version, info().target_image_digest, at, expect.any(String))
    expect(listener).toHaveBeenCalledTimes(1)
    expect(wrapper.get('[data-testid="cancel-schedule"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('version.preparingOnline')
    expect(wrapper.emitted('refresh')).toHaveLength(1)
    window.removeEventListener('kano-maintenance-refresh', listener)
    wrapper.unmount()
  })

  it('recovers and cancels a reservation instead of starting a competing upgrade', async () => {
    const job = { ...progress('scheduled'), job: { ...progress('scheduled').job, scheduled_at: '2026-10-17T20:00:00Z', preparation: 'ready' } }
    api.currentUpdateSchedule.mockResolvedValue(job)
    api.cancelUpdateSchedule.mockResolvedValue(undefined)
    const wrapper = mount(ContainerUpgradePanel, { props: { info: { ...info(), schedule_ready: true, next_scheduled_at: '2026-10-17T20:00:00Z' } } })
    await flushPromises()
    expect(wrapper.text()).toContain('version.preparationReady')
    expect(wrapper.get('[data-testid="schedule-upgrade"]').attributes('disabled')).toBeDefined()
    api.currentUpdateSchedule.mockResolvedValue(progress('cancelled'))
    await wrapper.get('[data-testid="cancel-schedule"]').trigger('click')
    await flushPromises()
    expect(api.cancelUpdateSchedule).toHaveBeenCalledWith(job.job.operation_id)
    expect(api.startContainerUpdate).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('version.jobStatus.cancelled')
    wrapper.unmount()
  })

  it('explains how to enable reservations on an older host service', async () => {
    const wrapper = mount(ContainerUpgradePanel, { props: { info: { ...info(), schedule_ready: false } } })
    await flushPromises()
    expect(wrapper.text()).toContain('version.scheduleUnavailable')
    expect(wrapper.get('[data-testid="schedule-upgrade"]').attributes('disabled')).toBeDefined()
    expect(api.currentUpdateSchedule).not.toHaveBeenCalled()
    wrapper.unmount()
  })
})
