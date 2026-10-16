/**
 * System API endpoints for admin operations
 */

import { apiClient } from '../client'

export interface ReleaseInfo {
  name: string
  body: string
  published_at: string
  html_url: string
}

export interface VersionInfo {
  current_version: string
  latest_version: string
  has_update: boolean
  release_info?: ReleaseInfo
  cached: boolean
  warning?: string
  build_type: string // "source" for manual builds, "release" for CI builds
  update_mode?: 'container'
  updater_ready?: boolean
  updater_version?: string
  update_busy?: boolean
  target_image_digest?: string
  unavailable_reason?: string
  schedule_ready?: boolean
  next_scheduled_at?: string
}

export interface ContainerUpdateJob {
  operation_id: string
  source_version: string
  target_version: string
  status: string
  message: string
  created_at: string
  updated_at: string
  scheduled_at?: string
  preparation?: string
}

export interface ContainerUpdateResult {
  job: ContainerUpdateJob | null
  operation_id?: string
  progress_url?: string
}

export async function startContainerUpdate(
  version: string,
  expectedDigest: string,
  idempotencyKey: string
): Promise<ContainerUpdateResult> {
  const { data } = await apiClient.post<ContainerUpdateResult>(
    '/admin/system/update',
    { version, expected_digest: expectedDigest },
    { headers: { 'Idempotency-Key': idempotencyKey }, timeout: 45000 }
  )
  return data
}

export async function currentContainerUpdate(): Promise<ContainerUpdateResult> {
  const { data } = await apiClient.get<ContainerUpdateResult>('/admin/system/update-job', { timeout: 45000 })
  return data
}

export async function currentUpdateSchedule(): Promise<ContainerUpdateResult> {
  const { data } = await apiClient.get<ContainerUpdateResult>('/admin/system/update-schedule', { timeout: 45000 })
  return data
}

export async function createUpdateSchedule(version: string, expectedDigest: string, expectedScheduledAt: string, idempotencyKey: string): Promise<ContainerUpdateResult> {
  const { data } = await apiClient.post<ContainerUpdateResult>('/admin/system/update-schedule',
    { version, expected_digest: expectedDigest, expected_scheduled_at: expectedScheduledAt },
    { headers: { 'Idempotency-Key': idempotencyKey }, timeout: 45000 })
  return data
}

export async function cancelUpdateSchedule(id: string): Promise<void> {
  await apiClient.delete(`/admin/system/update-schedule/${encodeURIComponent(id)}`, { timeout: 45000 })
}

/**
 * Get current version
 */
export async function getVersion(): Promise<{ version: string }> {
  const { data } = await apiClient.get<{ version: string }>('/admin/system/version')
  return data
}

/**
 * Check for updates
 * @param force - Force refresh from GitHub API
 */
export async function checkUpdates(force = false): Promise<VersionInfo> {
  const { data } = await apiClient.get<VersionInfo>('/admin/system/check-updates', {
    params: force ? { force: 'true' } : undefined
  })
  return data
}

export interface UpdateResult {
  message: string
  need_restart: boolean
}

export interface RollbackVersionInfo {
  version: string
  published_at: string
  html_url: string
}

/**
 * Get versions available for rollback (up to 3 versions older than current)
 */
export async function getRollbackVersions(): Promise<{ versions: RollbackVersionInfo[] }> {
  const { data } = await apiClient.get<{ versions: RollbackVersionInfo[] }>(
    '/admin/system/rollback-versions'
  )
  return data
}

/**
 * In-place update/rollback downloads a full release binary from GitHub, which
 * can take several minutes on slow links. The global 30s axios timeout would
 * abort the request mid-download (#4504), so these calls wait as long as the
 * backend allows (15 minutes server-side).
 */
const UPDATE_REQUEST_TIMEOUT_MS = 15 * 60 * 1000

/**
 * Perform system update
 * Downloads and applies the latest version
 */
export async function performUpdate(): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>('/admin/system/update', undefined, {
    timeout: UPDATE_REQUEST_TIMEOUT_MS
  })
  return data
}

/**
 * Rollback to a previous version
 * @param version - Target version (e.g. "0.1.146"); omit to restore the local backup binary
 */
export async function rollback(version?: string): Promise<UpdateResult> {
  const { data } = await apiClient.post<UpdateResult>(
    '/admin/system/rollback',
    version ? { version } : undefined,
    { timeout: UPDATE_REQUEST_TIMEOUT_MS }
  )
  return data
}

/**
 * Restart the service
 */
export async function restartService(): Promise<{ message: string }> {
  const { data } = await apiClient.post<{ message: string }>('/admin/system/restart')
  return data
}

export const systemAPI = {
  getVersion,
  checkUpdates,
  performUpdate,
  getRollbackVersions,
  rollback,
  restartService
}

export default systemAPI
