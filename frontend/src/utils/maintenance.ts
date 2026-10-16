export function formatBeijingTime(value: string, locale = 'zh-CN'): string {
  const date = new Date(value)
  if (!Number.isFinite(date.getTime())) return ''
  return new Intl.DateTimeFormat(locale, {
    timeZone: 'Asia/Shanghai', year: 'numeric', month: '2-digit', day: '2-digit',
    hour: '2-digit', minute: '2-digit', hour12: false
  }).format(date)
}

export function refreshMaintenanceStatus(): void {
  window.dispatchEvent(new Event('kano-maintenance-refresh'))
}
