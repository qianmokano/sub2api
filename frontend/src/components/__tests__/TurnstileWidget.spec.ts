import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import TurnstileWidget from '../TurnstileWidget.vue'

vi.mock('vue-i18n', () => ({
  useI18n: () => ({ t: (key: string) => key })
}))

const scriptSelector = 'script[src*="challenges.cloudflare.com/turnstile"]'
let wrapper: VueWrapper | undefined

function installSDK() {
  const render = vi.fn<NonNullable<typeof window.turnstile>['render']>(() => 'widget-1')
  window.turnstile = { render, reset: vi.fn(), remove: vi.fn() }
  return render
}

afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  delete window.turnstile
  delete window.onTurnstileLoad
  document.querySelectorAll(scriptSelector).forEach((script) => script.remove())
  vi.restoreAllMocks()
})

describe('TurnstileWidget', () => {
  it('keeps site and Passport widget tokens and cleanup separate while sharing the SDK', async () => {
    wrapper = mount(TurnstileWidget, { props: { siteKey: 'site-key' } })
    const passport = mount(TurnstileWidget, { props: { siteKey: 'passport-key' } })
    try {
      expect(document.querySelectorAll(scriptSelector)).toHaveLength(1)
      const render = installSDK()
      render.mockReturnValueOnce('site-widget').mockReturnValueOnce('passport-widget')
      window.onTurnstileLoad?.()
      await flushPromises()
      expect(render).toHaveBeenCalledTimes(2)
      render.mock.calls[0]![1].callback('site-proof')
      render.mock.calls[1]![1].callback('passport-proof')
      expect(wrapper.emitted('verify')).toEqual([['site-proof']])
      expect(passport.emitted('verify')).toEqual([['passport-proof']])
      passport.unmount()
      expect(window.turnstile!.remove).toHaveBeenCalledWith('passport-widget')
      expect(window.turnstile!.remove).not.toHaveBeenCalledWith('site-widget')
    } finally {
      if (passport.exists()) passport.unmount()
    }
  })

  it('reports SDK failure to both waiting widgets', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    wrapper = mount(TurnstileWidget, { props: { siteKey: 'site-key' } })
    const passport = mount(TurnstileWidget, { props: { siteKey: 'passport-key' } })
    try {
      document.querySelector(scriptSelector)!.dispatchEvent(new Event('error'))
      await flushPromises()
      expect(wrapper.emitted('error')).toEqual([[]])
      expect(passport.emitted('error')).toEqual([[]])
      expect(passport.find('[role="status"]').exists()).toBe(false)
    } finally {
      passport.unmount()
    }
  })

  it('shows a loading placeholder until the SDK initializes the widget', async () => {
    wrapper = mount(TurnstileWidget, { props: { siteKey: 'site-key' } })

    expect(wrapper.get('[role="status"]').text()).toBe('auth.captchaLoading')
    expect(document.querySelector(scriptSelector)).not.toBeNull()
    const container = wrapper.get('.turnstile-container').element

    const render = installSDK()
    window.onTurnstileLoad?.()
    await flushPromises()

    expect(render).toHaveBeenCalledWith(container, expect.objectContaining({ sitekey: 'site-key' }))
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    expect(wrapper.get('.turnstile-container').element).toBe(container)
  })

  it('initializes an already loaded SDK and still forwards verification', async () => {
    const render = installSDK()
    wrapper = mount(TurnstileWidget, { props: { siteKey: 'site-key' } })
    await flushPromises()

    expect(render).toHaveBeenCalledOnce()
    expect(document.querySelector(scriptSelector)).toBeNull()
    expect(wrapper.find('[role="status"]').exists()).toBe(false)

    render.mock.calls[0]![1].callback('verified-token')
    expect(wrapper.emitted('verify')).toEqual([['verified-token']])
  })

  it('resets its own widget, forwards expiration and error, and re-renders a changed key', async () => {
    const render = installSDK()
    wrapper = mount(TurnstileWidget, { props: { siteKey: 'site-key' } })
    await flushPromises()
    ;(wrapper.vm as unknown as {reset: () => void}).reset()
    expect(window.turnstile!.reset).toHaveBeenCalledWith('widget-1')
    render.mock.calls[0]![1]['expired-callback']?.()
    render.mock.calls[0]![1]['error-callback']?.()
    expect(wrapper.emitted('expire')).toEqual([[]])
    expect(wrapper.emitted('error')).toEqual([[]])
    await wrapper.setProps({siteKey:'changed-key'})
    expect(window.turnstile!.remove).toHaveBeenCalledWith('widget-1')
    expect(render).toHaveBeenLastCalledWith(expect.any(HTMLElement), expect.objectContaining({sitekey:'changed-key'}))
  })

  it('ends loading and reports a script load failure', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    wrapper = mount(TurnstileWidget, { props: { siteKey: 'site-key' } })

    document.querySelector(scriptSelector)!.dispatchEvent(new Event('error'))
    await flushPromises()

    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    expect(wrapper.emitted('error')).toEqual([[]])
  })

  it('does not show a placeholder or load the SDK without a site key', async () => {
    wrapper = mount(TurnstileWidget, { props: { siteKey: '' } })
    await flushPromises()

    expect(wrapper.find('.turnstile-wrapper').exists()).toBe(false)
    expect(wrapper.find('[role="status"]').exists()).toBe(false)
    expect(document.querySelector(scriptSelector)).toBeNull()
  })
})
