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
      <a
        v-if="progressURL"
        :href="progressURL"
        class="mt-2 inline-block text-primary-600 dark:text-primary-400"
      >
        {{ t('version.viewProgress') }}
      </a>
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
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  currentContainerUpdate,
  startContainerUpdate,
  type ContainerUpdateResult,
  type VersionInfo
} from '@/api/admin/system'
import { formatVersion } from '@/utils/version'

const props = defineProps<{ info: VersionInfo | null }>()
const emit = defineEmits<{ refresh: [] }>()
const { t } = useI18n()
const recent = ref<ContainerUpdateResult | null>(null)
const submitting = ref(false)
const recovering = ref(false)
const error = ref('')
let requestKey = ''

const progressURL = computed(() => {
  const url = recent.value?.progress_url
  return url && /^\/kano-updates\/up-[a-f0-9]{32}$/.test(url) ? url : ''
})
const active = computed(
  () => recent.value?.job && !['succeeded', 'failed', 'failed_rolled_back'].includes(recent.value.job.status)
)
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
watch(() => props.info?.latest_version, () => { requestKey = '' })
watch(() => recent.value?.job?.status, (status) => {
  if (status && ['failed', 'failed_rolled_back'].includes(status)) requestKey = ''
})
</script>
