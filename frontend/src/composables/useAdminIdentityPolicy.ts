import { computed, ref } from 'vue'
import { adminAPI } from '@/api/admin'
import type { SystemSettings } from '@/api/admin/settings'

export function useAdminIdentityPolicy() {
  const settings = ref<SystemSettings | null>(null)
  const ready = ref(false)
  const failed = ref(false)
  let sequence = 0

  async function load() {
    const request = ++sequence
    ready.value = false
    failed.value = false
    try {
      const result = await adminAPI.settings.getSettings()
      if (typeof result.sso_only_enabled !== 'boolean') throw new Error('Account policy unavailable')
      if (request !== sequence) return
      settings.value = result
      ready.value = true
    } catch {
      if (request === sequence) failed.value = true
    }
  }

  return {
    ready,
    failed,
    onlyEnabled: computed(() => !ready.value || settings.value?.sso_only_enabled === true),
    adminURL: computed(() => settings.value?.sso_admin_url || ''),
    load,
  }
}
