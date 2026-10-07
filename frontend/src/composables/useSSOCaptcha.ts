import { ref } from 'vue'
import { prepareCaptcha } from '@/api/sso'
import type { SSOCaptchaAction, SSOCaptchaChallenge, SSOCaptchaProof } from '@/api/sso'

export function useSSOCaptcha() {
  const challenge = ref<SSOCaptchaChallenge | null>(null)
  const answer = ref('')
  const loading = ref(false)
  let activeAction: SSOCaptchaAction | null = null
  let activeAccount = ''
  let expiresAt = 0
  let generation = 0

  const invalidate = () => {
    generation += 1
	loading.value = false
    challenge.value = null
    answer.value = ''
    activeAction = null
    activeAccount = ''
    expiresAt = 0
  }

  const refresh = async (action: SSOCaptchaAction, account: string) => {
    invalidate()
    const requestGeneration = generation
    loading.value = true
    try {
      const response = await prepareCaptcha({ action, account: account.trim() })
      if (requestGeneration !== generation) return
      challenge.value = response
      activeAction = action
      activeAccount = account.trim()
      expiresAt = Date.now() + Math.min(challenge.value.expires_in || 300, 300) * 1000
    } finally {
      if (requestGeneration === generation) loading.value = false
    }
  }

  const ensure = async (action: SSOCaptchaAction, account: string): Promise<boolean> => {
    if (activeAction !== action || activeAccount !== account.trim() || expiresAt <= Date.now()) {
      await refresh(action, account)
    }
    return activeAction === action && activeAccount === account.trim() &&
      !!challenge.value && (!challenge.value.required || !!answer.value.trim())
  }

  const proof = (): SSOCaptchaProof | undefined => challenge.value?.required && challenge.value.challenge
    ? { challenge: challenge.value.challenge, answer: answer.value.trim() }
    : undefined

  return { challenge, answer, loading, ensure, proof, refresh, invalidate }
}
