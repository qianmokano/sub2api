<template>
  <div v-if="challenge?.required" class="space-y-2" data-testid="passport-captcha">
    <label class="input-label">{{ t('auth.sso.captcha') }}</label>
    <div v-if="challenge.type === 'image'" class="flex flex-wrap items-center gap-3">
      <button type="button" :aria-label="t('auth.sso.refreshCaptcha')" @click="emit('refresh')">
        <img :src="challenge.image_base64" :alt="t('auth.sso.captcha')" class="h-12 rounded border" />
      </button>
      <input :value="modelValue" class="input min-w-0 flex-1" autocomplete="off" :aria-label="t('auth.sso.captcha')" @input="emit('update:modelValue', ($event.target as HTMLInputElement).value)" />
    </div>
    <template v-else-if="challenge.type === 'turnstile'">
      <TurnstileWidget :key="challenge.challenge" :site-key="challenge.site_key || ''" @verify="emit('update:modelValue', $event)" @expire="emit('update:modelValue', '')" @error="emit('update:modelValue', '')" />
      <button type="button" class="text-sm text-primary-600" @click="emit('refresh')">{{ t('auth.sso.refreshCaptcha') }}</button>
    </template>
  </div>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import type { SSOCaptchaChallenge } from '@/api/sso'
import TurnstileWidget from '@/components/TurnstileWidget.vue'
defineProps<{ challenge: SSOCaptchaChallenge | null; modelValue: string }>()
const emit = defineEmits<{
  (event: 'update:modelValue', value: string): void
  (event: 'refresh'): void
}>()
const { t } = useI18n()
</script>
