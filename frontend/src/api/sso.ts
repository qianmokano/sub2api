import { apiClient } from './client'
import type { LoginResponse } from './auth'
import type { ActionCaptchaRequestProof } from '@/types'

export interface SSOChallenge {
  token: string
  methods: { mfa_type: string }[]
}
export interface SSOMFAResponse {
  requires_sso_mfa: true
  challenge: SSOChallenge
}
export type SSOLoginResponse = LoginResponse | SSOMFAResponse
export const isSSOMFARequired = (response: SSOLoginResponse): response is SSOMFAResponse =>
  'requires_sso_mfa' in response && response.requires_sso_mfa === true

export async function passwordLogin(request: ActionCaptchaRequestProof & { account: string; password: string }): Promise<SSOLoginResponse> {
  const { data } = await apiClient.post<SSOLoginResponse>('/auth/sso/password-login', request)
  return data
}
export async function completeMFA(request: { challenge: string; mfa_type: string; passcode: string }): Promise<LoginResponse> {
  const { data } = await apiClient.post<LoginResponse>('/auth/sso/mfa', request)
  return data
}
export async function sendCode(request: ActionCaptchaRequestProof & { email: string }): Promise<{ countdown: number }> {
  const { data } = await apiClient.post<{ countdown: number }>('/auth/sso/register/send-code', request)
  return data
}
export async function register(request: ActionCaptchaRequestProof & { email: string; password: string; code: string }): Promise<LoginResponse> {
  const { data } = await apiClient.post<LoginResponse>('/auth/sso/register', request)
  return data
}
