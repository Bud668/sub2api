import type { UserSubscription } from '@/types'

const ONE_DAY_MS = 24 * 60 * 60 * 1000

// Share neutral themed cards with group borders; purple is reserved for debug.
export function subscriptionBorderStyle(subscription: Pick<UserSubscription, 'group_id' | 'admin_debug'>) {
  const accent = subscription.admin_debug ? '#a78bfa' : `hsl(${30 + (subscription.group_id * 137.508) % 210} 65% 57%)`
  return {
    '--subscription-accent': accent,
    borderColor: `color-mix(in srgb, ${accent} 42%, var(--subscription-card-base))`,
    backgroundColor: 'var(--subscription-card-base)',
    boxShadow: 'var(--subscription-card-shadow)'
  }
}

export type ExpirationDateRelation = 'expired' | 'today' | 'tomorrow' | 'later'

// Match the rounded-up day label; expired remains red, invalid dates neutral.
export function subscriptionExpiryClass(expiresAt: string, now: Date = new Date()): string {
  const days = Math.ceil((new Date(expiresAt).getTime() - now.getTime()) / ONE_DAY_MS)
  if (!Number.isFinite(days)) return 'bg-gray-100 text-gray-700 dark:bg-dark-600 dark:text-gray-300'
  if (days <= 3) return 'bg-red-100 text-red-800 dark:bg-red-900/40 dark:text-red-200'
  if (days <= 7) return 'bg-yellow-100 text-yellow-800 dark:bg-yellow-900/40 dark:text-yellow-200'
  return 'bg-green-100 text-green-800 dark:bg-green-900/40 dark:text-green-200'
}

export type RemainingExpiryDuration =
  | { unit: 'days'; days: number }
  | { unit: 'hoursMinutes'; hours: number; minutes: number }

export interface RemainingDurationParts {
  days: number
  hours: number
  minutes: number
}

export function isOneTimeDailyQuota(
  subscription: Pick<UserSubscription, 'starts_at' | 'expires_at'>
): boolean {
  if (!subscription.starts_at || !subscription.expires_at) return false

  const startsAt = new Date(subscription.starts_at).getTime()
  const expiresAt = new Date(subscription.expires_at).getTime()

  if (!Number.isFinite(startsAt) || !Number.isFinite(expiresAt)) return false

  return expiresAt <= startsAt + ONE_DAY_MS
}

export function getRemainingDurationParts(
  targetAt: Date | string,
  now: Date = new Date()
): RemainingDurationParts | null {
  const targetTime = targetAt instanceof Date ? targetAt.getTime() : new Date(targetAt).getTime()
  const nowTime = now.getTime()

  if (!Number.isFinite(targetTime) || !Number.isFinite(nowTime)) return null

  const diffMs = targetTime - nowTime
  if (diffMs <= 0) return null

  const totalMinutes = Math.floor(diffMs / (1000 * 60))
  const days = Math.floor(totalMinutes / (24 * 60))
  const hours = Math.floor((totalMinutes % (24 * 60)) / 60)
  const minutes = totalMinutes % 60

  return { days, hours, minutes }
}

export function getExpirationDateRelation(
  targetAt: Date | string,
  now: Date = new Date()
): ExpirationDateRelation | null {
  const target = targetAt instanceof Date ? targetAt : new Date(targetAt)
  const targetTime = target.getTime()
  const nowTime = now.getTime()

  if (!Number.isFinite(targetTime) || !Number.isFinite(nowTime)) return null
  if (targetTime <= nowTime) return 'expired'

  const targetDay = Date.UTC(target.getFullYear(), target.getMonth(), target.getDate())
  const currentDay = Date.UTC(now.getFullYear(), now.getMonth(), now.getDate())
  const calendarDays = Math.round((targetDay - currentDay) / ONE_DAY_MS)

  if (calendarDays === 0) return 'today'
  if (calendarDays === 1) return 'tomorrow'
  return 'later'
}

export function getRemainingExpiryDuration(
  targetAt: Date | string,
  now: Date = new Date()
): RemainingExpiryDuration | null {
  const targetTime = targetAt instanceof Date ? targetAt.getTime() : new Date(targetAt).getTime()
  const nowTime = now.getTime()

  if (!Number.isFinite(targetTime) || !Number.isFinite(nowTime)) return null

  const diffMs = targetTime - nowTime
  if (diffMs <= 0) return null
  if (diffMs >= ONE_DAY_MS) {
    return { unit: 'days', days: Math.ceil(diffMs / ONE_DAY_MS) }
  }

  const totalMinutes = Math.ceil(diffMs / (60 * 1000))
  return {
    unit: 'hoursMinutes',
    hours: Math.floor(totalMinutes / 60),
    minutes: totalMinutes % 60
  }
}
