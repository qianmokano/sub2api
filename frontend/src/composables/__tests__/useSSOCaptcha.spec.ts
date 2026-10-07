import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApp, h, nextTick, reactive, type App } from 'vue'
import { useSSOCaptcha } from '@/composables/useSSOCaptcha'
import SSOCaptcha from '@/components/auth/SSOCaptcha.vue'

const mocks = vi.hoisted(() => ({ prepare: vi.fn() }))
vi.mock('@/api/sso', () => ({ prepareCaptcha: mocks.prepare }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/components/TurnstileWidget.vue', () => ({ default: {
  props: ['siteKey'], emits: ['verify','expire','error'],
  template: '<div><button class="turnstile" :data-key="siteKey" @click="$emit(\'verify\',\'solved\')">Verify</button><button class="expire" @click="$emit(\'expire\')">Expire</button><button class="error" @click="$emit(\'error\')">Error</button></div>',
} }))

let app: App | undefined
beforeEach(() => { vi.clearAllMocks(); vi.useRealTimers() })
afterEach(() => { app?.unmount(); app = undefined; document.body.innerHTML = ''; vi.useRealTimers() })
const response = (data: unknown) => data

describe('passport challenge lifecycle', () => {
  it('skips the widget when no CAPTCHA is required and preserves action/account binding', async () => {
    mocks.prepare.mockResolvedValue(response({ required: false }))
    const captcha = useSSOCaptcha()
    expect(captcha.proof()).toBeUndefined()
    expect(await captcha.ensure('login', ' buyer@example.com ')).toBe(true)
    expect(mocks.prepare).toHaveBeenCalledWith({ action: 'login', account: 'buyer@example.com' })
    expect(await captcha.ensure('login', 'buyer@example.com')).toBe(true)
    expect(mocks.prepare).toHaveBeenCalledTimes(1)
    expect(await captcha.ensure('register', 'buyer@example.com')).toBe(true)
    expect(await captcha.ensure('register', 'other@example.com')).toBe(true)
    expect(mocks.prepare).toHaveBeenCalledTimes(3)
  })
  it('requires an answer, expires after five minutes and invalidates consumed proofs', async () => {
    vi.useFakeTimers(); vi.setSystemTime(1000)
    mocks.prepare.mockResolvedValue(response({ required:true, challenge:'token', type:'image', expires_in:900 }))
    const captcha = useSSOCaptcha()
    expect(await captcha.ensure('login','buyer')).toBe(false)
    captcha.answer.value = ' answer '
    expect(await captcha.ensure('login','buyer')).toBe(true)
    expect(captcha.proof()).toEqual({ challenge:'token', answer:'answer' })
    vi.advanceTimersByTime(300001)
    expect(await captcha.ensure('login','buyer')).toBe(false)
    expect(captcha.answer.value).toBe('')
    captcha.invalidate()
    expect(captcha.proof()).toBeUndefined()
    expect(captcha.challenge.value).toBeNull()
    mocks.prepare.mockRejectedValueOnce(new Error('offline'))
    await expect(captcha.ensure('login','buyer')).rejects.toThrow('offline')
    expect(captcha.loading.value).toBe(false)
    expect(captcha.challenge.value).toBeNull()
  })
  it('clears loading when a pending challenge is cancelled', async () => {
    let finish!: (value: unknown) => void
    mocks.prepare.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const captcha = useSSOCaptcha()
    const pending = captcha.ensure('login', 'buyer')
    expect(captcha.loading.value).toBe(true)
    captcha.invalidate()
    expect(captcha.loading.value).toBe(false)
    finish(response({ required: true, challenge: 'stale', type: 'image' }))
    expect(await pending).toBe(false)
    expect(captcha.challenge.value).toBeNull()
  })
  it('ignores stale responses after an account or operation changes', async () => {
    let finish!: (value: unknown) => void
    mocks.prepare.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    mocks.prepare.mockResolvedValueOnce(response({ required:true, challenge:'new', type:'turnstile', site_key:'public' }))
    const captcha = useSSOCaptcha()
    const old = captcha.ensure('register-send-code','old@example.com')
    expect(captcha.loading.value).toBe(true)
    expect(await captcha.ensure('register','new@example.com')).toBe(false)
    finish(response({ required:false }))
    expect(await old).toBe(false)
    expect(captcha.challenge.value?.challenge).toBe('new')
    expect(captcha.loading.value).toBe(false)
    captcha.answer.value = 'turnstile-token'
    expect(await captcha.ensure('register','new@example.com')).toBe(true)
  })
})

describe('passport CAPTCHA field', () => {
  it('renders only required verification and emits image answers, refreshes and Turnstile proofs', async () => {
    const props = reactive<any>({ challenge:null, modelValue:'' })
    const answer = vi.fn(); const refresh = vi.fn()
    const root = document.createElement('div'); document.body.appendChild(root)
    app = createApp({ render: () => h(SSOCaptcha,{ ...props, 'onUpdate:modelValue':answer, onRefresh:refresh }) }); app.mount(root)
    expect(root.querySelector('input')).toBeNull()
    props.challenge = { required:false }; await nextTick()
    expect(root.querySelector('input')).toBeNull()
    props.challenge = { required:true, type:'image', challenge:'image', image_base64:'data:image/png;base64,aGVsbG8=' }; await nextTick()
    const input = root.querySelector('input')!
    input.value = '123'; input.dispatchEvent(new Event('input')); await nextTick()
    expect(answer).toHaveBeenCalledWith('123')
    root.querySelector('button')!.click()
    expect(refresh).toHaveBeenCalled()
    props.challenge = { required:true, type:'turnstile', challenge:'turnstile', site_key:'public' }; await nextTick()
    expect(root.querySelector('.turnstile')?.getAttribute('data-key')).toBe('public')
    root.querySelector<HTMLButtonElement>('.turnstile')!.click()
    expect(answer).toHaveBeenCalledWith('solved')
    root.querySelector<HTMLButtonElement>('.expire')!.click()
    expect(answer).toHaveBeenLastCalledWith('')
    root.querySelector<HTMLButtonElement>('.error')!.click()
    expect(answer).toHaveBeenLastCalledWith('')
    root.querySelector<HTMLButtonElement>('[data-testid="passport-captcha"] > button')!.click()
    expect(refresh).toHaveBeenCalledTimes(2)
    props.challenge = { required:true, type:'turnstile' }; await nextTick()
    expect(root.querySelector('.turnstile')?.getAttribute('data-key')).toBe('')
    props.challenge = { required:true, type:'unsupported' }; await nextTick()
    expect(root.querySelector('button')).toBeNull()
  })
})
