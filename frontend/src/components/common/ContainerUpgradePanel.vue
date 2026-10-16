<template>
  <div class="space-y-3" data-testid="container-upgrade">
    <p class="text-xs text-gray-500 dark:text-dark-400">{{ t('version.containerHint') }}</p>
    <p v-if="!info?.updater_ready" class="text-xs text-amber-700 dark:text-amber-400" role="status">
      {{ t('version.updaterUnavailable') }}
    </p>
    <p v-else class="text-xs text-green-700 dark:text-green-400">{{ t('version.updaterReady') }}</p>
    <p
      v-if="info?.updater_ready && info.unavailable_reason"
      class="text-xs text-amber-700 dark:text-amber-400"
      role="status"
    >
      {{ info.unavailable_reason }}
    </p>
    <p v-if="error" class="text-xs text-red-600 dark:text-red-400" role="alert">{{ error }}</p>
    <div v-if="recent?.job" class="rounded-lg bg-gray-50 p-3 text-xs dark:bg-dark-700" role="status">
      <p class="font-medium text-gray-700 dark:text-dark-200">
        {{ formatVersion(recent.job.target_version) }}
      </p>
      <p class="mt-1 text-gray-500 dark:text-dark-400">
        {{ t(`version.jobStatus.${recent.job.status}`) }}
      </p>
      <p v-if="recent.job.scheduled_at" class="mt-1">
        {{ t('version.reservedAt', { time: formatBeijingTime(recent.job.scheduled_at, locale) }) }}
      </p>
      <p v-if="recent.job.status === 'scheduled'" class="mt-1">
        {{ recent.job.preparation === 'ready' ? t('version.preparationReady') : t('version.preparingOnline') }}
      </p>
      <p v-if="['missed', 'cancelled'].includes(recent.job.status)" class="mt-1">{{ recent.job.message }}</p>
      <a
        v-if="progressURL"
        :href="progressURL"
        class="mt-2 inline-block text-primary-600 dark:text-primary-400"
      >
        {{ t('version.viewProgress') }}
      </a>
      <button v-if="recent.job.status === 'scheduled'" type="button" data-testid="cancel-schedule"
        :disabled="submitting" @click="cancelReservation" class="mt-2 block text-red-600 dark:text-red-400 disabled:opacity-50">
        {{ t('version.cancelSchedule') }}
      </button>
    </div>
    <button
      v-if="info?.has_update"
      type="button"
      :disabled="!canUpgrade"
      @click="upgrade"
      class="w-full rounded-lg bg-primary-500 px-4 py-2 text-sm font-medium text-white hover:bg-primary-600 disabled:cursor-not-allowed disabled:opacity-50"
    >
      {{ submitting ? t('version.submittingUpgrade') : t('version.upgradeNow') }}
    </button>
    <button v-if="info?.has_update" type="button" data-testid="schedule-upgrade" :disabled="!canSchedule" @click="confirmReservation"
      class="w-full rounded-lg border border-primary-300 px-4 py-2 text-sm font-medium text-primary-700 hover:bg-primary-50 disabled:cursor-not-allowed disabled:opacity-50 dark:text-primary-300">
      {{ t('version.scheduleNext') }}
    </button>
    <p v-if="info?.updater_ready && !info.schedule_ready" class="text-xs text-amber-700 dark:text-amber-400">
      {{ t('version.scheduleUnavailable') }}
    </p>
    <ConfirmDialog :show="!!selection" :title="t('version.scheduleNext')"
      :message="t('version.scheduleConfirm', { version: formatVersion(selection?.version || ''), time: formatBeijingTime(selection?.at || '', locale) })"
      @confirm="reserveUpgrade" @cancel="selection = null" />
    <a
      v-if="info?.release_info?.html_url"
      :href="info.release_info.html_url"
      target="_blank"
      rel="noopener noreferrer"
      class="block text-center text-xs text-gray-500 dark:text-dark-400"
    >
      {{ t('version.viewChangelog') }}
    </a>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch, onMounted, onBeforeUnmount } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  currentContainerUpdate,
  startContainerUpdate,
  currentUpdateSchedule,
  createUpdateSchedule,
  cancelUpdateSchedule,
  type ContainerUpdateResult,
  type VersionInfo
} from '@/api/admin/system'
import { formatVersion } from '@/utils/version'
import { formatBeijingTime, refreshMaintenanceStatus } from '@/utils/maintenance'
import ConfirmDialog from './ConfirmDialog.vue'

const props = defineProps<{ info: VersionInfo | null }>()
const emit = defineEmits<{ refresh: [] }>()
const { t, locale } = useI18n()
const recent = ref<ContainerUpdateResult | null>(null)
const submitting = ref(false)
const recovering = ref(false)
const error = ref('')
let requestKey = ''
let reservationKey = ''
const selection = ref<{ version: string; digest: string; at: string } | null>(null)
let poll: ReturnType<typeof setInterval> | undefined

const progressURL = computed(() => {
  const url = recent.value?.progress_url
  return url && /^\/kano-updates\/up-[a-f0-9]{32}$/.test(url) ? url : ''
})
const active = computed(
  () => recent.value?.job && !['succeeded', 'failed', 'failed_rolled_back', 'cancelled', 'missed'].includes(recent.value.job.status)
)
const canSchedule = computed(() => canUpgrade.value && props.info?.schedule_ready && props.info.next_scheduled_at)
const canUpgrade = computed(
  () => props.info?.updater_ready && props.info.has_update && props.info.target_image_digest &&
    !props.info.unavailable_reason && !props.info.update_busy && !active.value &&
    !submitting.value && !recovering.value
)

function message(cause: unknown): string {
  const value = cause as { response?: { data?: { message?: string } }; message?: string }
  return value.response?.data?.message || value.message || t('version.updateFailed')
}

async function restoreProgress() {
  if (!props.info?.updater_ready || recovering.value) return
  recovering.value = true
  try {
    recent.value = await currentContainerUpdate()
    if (props.info.schedule_ready) {
      const schedule = await currentUpdateSchedule()
      if (schedule.job && (!recent.value.job || schedule.job.status === 'scheduled' ||
        new Date(schedule.job.created_at) >= new Date(recent.value.job.created_at))) recent.value = schedule
    }
    if (
      recent.value.job?.status === 'succeeded' &&
      recent.value.job.target_version !== props.info?.current_version
    ) emit('refresh')
  } catch (cause) {
    error.value = message(cause)
  } finally {
    recovering.value = false
  }
}

function confirmReservation() {
  if (!canSchedule.value || !props.info?.target_image_digest || !props.info.next_scheduled_at) return
  selection.value = { version: props.info.latest_version, digest: props.info.target_image_digest, at: props.info.next_scheduled_at }
}

async function reserveUpgrade() {
  if (!selection.value || submitting.value) return
  const chosen = selection.value
  selection.value = null
  submitting.value = true
  error.value = ''
  if (!reservationKey) reservationKey = crypto.randomUUID()
  try {
    recent.value = await createUpdateSchedule(chosen.version, chosen.digest, chosen.at, reservationKey)
    refreshMaintenanceStatus()
    emit('refresh')
  } catch (cause) {
    error.value = message(cause)
    await restoreProgress()
  } finally { submitting.value = false }
}

async function cancelReservation() {
  const id = recent.value?.job?.operation_id
  if (!id || submitting.value) return
  submitting.value = true
  error.value = ''
  try {
    await cancelUpdateSchedule(id)
    reservationKey = ''
    await restoreProgress()
    refreshMaintenanceStatus()
    emit('refresh')
  } catch (cause) { error.value = message(cause) }
  finally { submitting.value = false }
}

async function upgrade() {
  if (!canUpgrade.value || !props.info?.target_image_digest) return
  submitting.value = true
  error.value = ''
  if (!requestKey) requestKey = crypto.randomUUID()
  try {
    recent.value = await startContainerUpdate(
      props.info.latest_version,
      props.info.target_image_digest,
      requestKey
    )
    if (progressURL.value) window.location.assign(progressURL.value)
  } catch (cause) {
    error.value = message(cause)
    await restoreProgress()
  } finally {
    submitting.value = false
  }
}

watch(
  () => props.info?.updater_ready,
  (ready) => { if (ready) void restoreProgress() },
  { immediate: true }
)
watch(() => props.info?.latest_version, () => { requestKey = ''; reservationKey = '' })
watch(() => recent.value?.job?.status, (status) => {
  if (status && ['failed', 'failed_rolled_back', 'cancelled', 'missed'].includes(status)) { requestKey = ''; reservationKey = '' }
})
onMounted(() => { poll = setInterval(() => { if (recent.value?.job?.status === 'scheduled') void restoreProgress() }, 10000) })
onBeforeUnmount(() => { clearInterval(poll) })
</script>
