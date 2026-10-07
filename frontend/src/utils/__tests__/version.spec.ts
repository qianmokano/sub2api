import { describe, expect, it } from 'vitest'
import { formatVersion } from '../version'

describe('formatVersion', () => {
  it.each([
    ['v0.2.14-1', 'v0.2.14-1'],
    ['0.2.14-2', 'v0.2.14-2'],
    ['v0.2.15-1', 'v0.2.15-1'],
    ['v0.2.14-kano.1', 'v0.2.14-kano.1'],
    ['0.2.14', 'v0.2.14'],
    ['0.0.0-dev', 'v0.0.0-dev'],
    ['  V0.2.14-1  ', 'v0.2.14-1'],
    ['vv0.2.14-1', 'v0.2.14-1'],
    ['', ''],
    ['  ', ''],
    ['v', ''],
    [null, ''],
    [undefined, '']
  ])('formats %s as %s', (version, expected) => {
    expect(formatVersion(version)).toBe(expected)
  })
})
