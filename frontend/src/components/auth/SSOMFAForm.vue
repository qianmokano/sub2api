<template>
  <form class="space-y-4 rounded-xl border border-gray-200 p-5 dark:border-dark-600" @submit.prevent="emit('verify', method, code)">
    <h3 class="font-semibold">{{ t('auth.sso.verification') }}</h3>
    <label class="input-label" for="sso-method">{{ t('auth.sso.method') }}</label>
    <select id="sso-method" v-model="method" class="input" :disabled="loading">
      <option v-for="item in challenge.methods" :key="item.mfa_type" :value="item.mfa_type">{{ item.mfa_type }}</option>
    </select>
    <label class="input-label" for="sso-code">{{ t('auth.sso.code') }}</label>
    <input id="sso-code" v-model="code" class="input" autocomplete="one-time-code" required maxlength="128" :disabled="loading" />
    <p v-if="error" role="alert" class="text-sm text-red-600">{{ error }}</p>
    <button class="btn btn-primary w-full" :disabled="loading || !code.trim()">{{ t('auth.sso.verify') }}</button>
    <button type="button" class="btn w-full" :disabled="loading" @click="emit('cancel')">{{ t('common.cancel') }}</button>
  </form>
</template>
<script setup lang="ts">
import { ref } from 'vue'
import { useI18n } from 'vue-i18n'
import type { SSOChallenge } from '@/api/sso'
const props = defineProps<{ challenge: SSOChallenge; loading: boolean; error: string }>()
const emit = defineEmits<{ verify: [method: string, code: string]; cancel: [] }>()
const { t } = useI18n()
const method = ref(props.challenge.methods[0]?.mfa_type || '')
const code = ref('')
</script>
