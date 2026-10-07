export function formatVersion(version: string | null | undefined): string {
  const normalized = version?.trim().replace(/^v+/i, '') ?? ''
  return normalized ? `v${normalized}` : ''
}
