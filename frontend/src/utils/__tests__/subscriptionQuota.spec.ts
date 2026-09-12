import { describe, expect, it } from 'vitest'

import { getExpirationDateRelation, getRemainingExpiryDuration, subscriptionBorderStyle, subscriptionExpiryClass } from '../subscriptionQuota'

it('shares neutral themed cards with soft shadows and reserves purple borders for debug', () => {
  const normal = subscriptionBorderStyle({ group_id: 7 })
  expect(normal).toEqual(subscriptionBorderStyle({ group_id: 7, admin_debug: false }))
  expect(normal['--subscription-accent']).not.toBe(subscriptionBorderStyle({ group_id: 8 })['--subscription-accent'])
  const debug = subscriptionBorderStyle({ group_id: 7, admin_debug: true })
  expect(debug['--subscription-accent']).toBe('#a78bfa')
  expect(debug).toEqual(subscriptionBorderStyle({ group_id: 8, admin_debug: true }))
  expect(normal.backgroundColor).toBe('var(--subscription-card-base)')
  expect(debug.backgroundColor).toBe(normal.backgroundColor)
  expect(normal.boxShadow).toBe('var(--subscription-card-shadow)')
  expect(debug.boxShadow).toBe(normal.boxShadow)
  expect(normal.borderColor).toContain('42%, var(--subscription-card-base)')
})

describe('subscription expiry timing', () => {
  it('uses red through 3 days, yellow through 7, green beyond, with both themes', () => {
    const now = new Date('2026-09-12T12:00:00Z')
    for (const [days, color] of [[-1, 'red'], [0, 'red'], [3, 'red'], [3.001, 'yellow'], [7, 'yellow'], [7.001, 'green'], [23, 'green']] as const) {
      const classes = subscriptionExpiryClass(new Date(now.getTime() + days * 86400_000).toISOString(), now)
      expect(classes).toBe(`bg-${color}-100 text-${color}-800 dark:bg-${color}-900/40 dark:text-${color}-200`)
    }
    expect(subscriptionExpiryClass('invalid', now)).toContain('bg-gray-100')
  })

  it('uses local calendar dates for today and tomorrow', () => {
    const now = new Date(2026, 2, 7, 23, 30)

    expect(getExpirationDateRelation(new Date(2026, 2, 7, 23, 45), now)).toBe('today')
    expect(getExpirationDateRelation(new Date(2026, 2, 8, 3, 30), now)).toBe('tomorrow')
  })

  it('treats the exact expiry instant and elapsed expiries as expired', () => {
    const now = new Date(2026, 6, 30, 9, 0)

    expect(getExpirationDateRelation(now, now)).toBe('expired')
    expect(getRemainingExpiryDuration(now, now)).toBeNull()
    expect(getExpirationDateRelation(new Date(2026, 6, 30, 8, 59), now)).toBe('expired')
    expect(getRemainingExpiryDuration(new Date(2026, 6, 30, 8, 59), now)).toBeNull()
  })

  it('rejects invalid target and current dates', () => {
    const invalid = new Date('invalid')
    const valid = new Date(2026, 6, 30, 9, 0)

    expect(getExpirationDateRelation(invalid, valid)).toBeNull()
    expect(getExpirationDateRelation(valid, invalid)).toBeNull()
    expect(getRemainingExpiryDuration(invalid, valid)).toBeNull()
    expect(getRemainingExpiryDuration(valid, invalid)).toBeNull()
  })

  it('returns rounded-up hours and minutes for an expiry under 24 hours away', () => {
    const now = new Date(2026, 6, 30, 9, 0)

    expect(getRemainingExpiryDuration(new Date(2026, 6, 31, 8, 30), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 23,
      minutes: 30
    })
    expect(getRemainingExpiryDuration(new Date(now.getTime() + 1), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 0,
      minutes: 1
    })
    expect(getRemainingExpiryDuration(new Date(now.getTime() + 23 * 60 * 60 * 1000 + 1), now)).toEqual({
      unit: 'hoursMinutes',
      hours: 23,
      minutes: 1
    })
  })

  it('preserves rounded-up day display from 24 hours onward', () => {
    const now = new Date(2026, 6, 30, 9, 0)

    expect(getRemainingExpiryDuration(new Date(now.getTime() + 24 * 60 * 60 * 1000), now)).toEqual({
      unit: 'days',
      days: 1
    })
    expect(getRemainingExpiryDuration(new Date(now.getTime() + 24 * 60 * 60 * 1000 + 1), now)).toEqual({
      unit: 'days',
      days: 2
    })
  })
})
