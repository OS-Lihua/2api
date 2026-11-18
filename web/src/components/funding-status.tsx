import { useTranslation } from 'react-i18next'

export function FundingStatus({
  pending,
  compact = false,
}: {
  pending?: boolean
  compact?: boolean
}) {
  const { t } = useTranslation()
  if (pending === false) return null
  if (pending === undefined) {
    return (
      <span className='text-muted-foreground block text-xs wrap-anywhere'>
        {t('Funding status unavailable')}
      </span>
    )
  }
  return (
    <span className='block space-y-1 text-xs wrap-anywhere'>
      <span className='block font-medium text-amber-700 dark:text-amber-300'>
        {t('Funding allocation pending')}
      </span>
      {!compact && (
        <span className='text-muted-foreground block'>
          {t(
            'Revenue is recorded, but funding allocation is incomplete. Zero outstanding debt does not mean fully paid.'
          )}
        </span>
      )}
    </span>
  )
}
