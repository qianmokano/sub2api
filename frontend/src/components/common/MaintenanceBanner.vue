<template>
  <aside v-if="scheduledAt" role="status" data-testid="maintenance-banner"
    class="relative z-40 border-b border-amber-200 bg-amber-50 px-4 py-3 text-center text-sm text-amber-900 dark:border-amber-900 dark:bg-amber-950 dark:text-amber-100">
    {{ t('maintenance.scheduled', { time: formatBeijingTime(scheduledAt, locale) }) }}
  </aside>
</template>

<script setup lang="ts">
import { onMounted, onBeforeUnmount, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatBeijingTime } from '@/utils/maintenance'

const { t, locale } = useI18n()
const scheduledAt = ref('')
let timer: ReturnType<typeof setInterval> | undefined
let inFlight = false
let leaving = false
let controller: AbortController | undefined

function enterMaintenance() {
  if (leaving) return
  leaving = true
  window.location.reload()
}

async function refresh() {
  if (inFlight) return
  inFlight = true
  controller = new AbortController()
  const timeout = setTimeout(() => controller?.abort(), 3000)
  try {
    // This independent public route must not carry app tokens or rely on Axios.
    const response = await fetch('/kano-maintenance/status', { cache: 'no-store', credentials: 'omit', signal: controller.signal })
    if (!response.ok) return
    const status = await response.json() as { maintenance?: boolean; scheduled_at?: string }
    if (status.maintenance === true) { enterMaintenance(); return }
    if (status.maintenance !== false) return
    scheduledAt.value = status.scheduled_at && formatBeijingTime(status.scheduled_at) ? status.scheduled_at : ''
  } catch { /* A short host-service restart keeps the last known announcement. */ }
  finally { clearTimeout(timeout); inFlight = false }
}

function onVisible() { if (document.visibilityState === 'visible') void refresh() }
onMounted(() => {
  void refresh()
  timer = setInterval(() => void refresh(), 10000)
  window.addEventListener('focus', refresh)
  window.addEventListener('kano-maintenance-refresh', refresh)
  window.addEventListener('kano-maintenance-active', enterMaintenance)
  document.addEventListener('visibilitychange', onVisible)
})
onBeforeUnmount(() => {
  clearInterval(timer)
  controller?.abort()
  window.removeEventListener('focus', refresh)
  window.removeEventListener('kano-maintenance-refresh', refresh)
  window.removeEventListener('kano-maintenance-active', enterMaintenance)
  document.removeEventListener('visibilitychange', onVisible)
})
</script>
