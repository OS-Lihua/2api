import { act, render, renderHook, screen, within } from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { toast } from 'sonner'
import { beforeEach, expect, it, vi } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'
import { api } from '@/lib/api'

import { getSelfWallet } from '../api'
import { WalletStatsCard } from '../components/wallet-stats-card'
import { useRedemption } from '../hooks/use-redemption'
import type { WalletBalance } from '../types'

const i18n = createInstance()
const wallet: WalletBalance = {
  user_id: 2,
  balance: '9007199.254740993',
  currency: 'USD',
  updated_at: 1,
}

beforeEach(async () => {
  await i18n.use(initReactI18next).init({
    lng: 'en',
    fallbackLng: false,
    nsSeparator: false,
    resources: { en, fr, zhCN: zh, zhTW },
    interpolation: { escapeValue: false },
  })
})

it('shows exact wallet funds and explicit zero unpaid balance', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard wallet={wallet} />
    </I18nextProvider>
  )
  expect(screen.getByText('9,007,199.254740993 USD')).toBeVisible()
  expect(screen.getByText('0 USD')).toBeVisible()
  expect(screen.getByText('Allowed')).toBeVisible()
  expect(screen.queryByText('Total Usage')).not.toBeInTheDocument()
})

it('shows unpaid debt and paused requests independently of spendable funds', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard
        wallet={{
          ...wallet,
          balance: '0',
          outstanding: '0.000000001',
          requests_paused: true,
        }}
      />
    </I18nextProvider>
  )
  expect(screen.getByText('0.000000001 USD')).toBeVisible()
  expect(screen.getByText('Paused')).toBeVisible()
  expect(screen.queryByText('Allowed')).not.toBeInTheDocument()
})

it('does not invent a zero balance or an allowed status when wallet loading fails', () => {
  const view = render(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard wallet={null} />
    </I18nextProvider>
  )
  expect(within(view.container).getAllByText('-')).toHaveLength(3)
  expect(screen.queryByText('0 USD')).not.toBeInTheDocument()
  expect(screen.queryByText('Allowed')).not.toBeInTheDocument()
})

it('keeps nanos precision when switching French and both Chinese interface locales', async () => {
  render(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard wallet={wallet} />
    </I18nextProvider>
  )
  await act(() => i18n.changeLanguage('fr'))
  expect(
    screen.getByText('9 007 199,254740993 USD', { normalizer: (text) => text })
  ).toBeVisible()
  for (const language of ['zhCN', 'zhTW']) {
    await act(() => i18n.changeLanguage(language))
    expect(screen.getByText('9,007,199.254740993 USD')).toBeVisible()
  }
})

it('keeps balances unknown while their initial request is loading', () => {
  render(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard wallet={null} loading />
    </I18nextProvider>
  )
  expect(screen.queryByText('0 USD')).not.toBeInTheDocument()
  expect(screen.queryByText('Allowed')).not.toBeInTheDocument()
})

it('stacks exact balances on small screens and keeps labels and currency readable', () => {
  const view = render(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard
        wallet={{ ...wallet, balance: '9223372036.854775807' }}
      />
    </I18nextProvider>
  )
  expect(view.container.querySelector('dl')).toHaveClass(
    'grid-cols-1',
    'sm:grid-cols-3'
  )
  expect(screen.getByText('Current Balance')).not.toHaveClass('truncate')
  expect(screen.getByText('Unpaid balance')).not.toHaveClass('truncate')
  const amount = screen.getByText('9,223,372,036.854775807 USD')
  expect(amount).not.toHaveClass('break-all')
  expect(amount).toHaveClass('wrap-anywhere')
  view.rerender(
    <I18nextProvider i18n={i18n}>
      <WalletStatsCard wallet={null} loading />
    </I18nextProvider>
  )
  expect(view.container.firstElementChild).toHaveClass(
    'grid-cols-1',
    'sm:grid-cols-3'
  )
})

it('reports the exact credited currency after redeeming and follows language changes', async () => {
  const success = vi
    .spyOn(toast, 'success')
    .mockImplementation(() => 'redemption-toast')
  const post = vi.spyOn(api, 'post').mockResolvedValue({
    data: {
      success: true,
      data: { nanos: '9007199254740993', currency: 'EUR' },
    },
  })
  const { result } = renderHook(() => useRedemption(), {
    wrapper: (props) => (
      <I18nextProvider i18n={i18n}>{props.children}</I18nextProvider>
    ),
  })
  await act(async () =>
    expect(await result.current.redeemCode('voucher')).toBe(true)
  )
  expect(post).toHaveBeenCalledWith('/api/user/topup', { key: 'voucher' })
  expect(success).toHaveBeenLastCalledWith(
    'Redemption successful! Added: 9,007,199.254740993 EUR'
  )
  await act(() => i18n.changeLanguage('fr'))
  await act(async () =>
    expect(await result.current.redeemCode('voucher-fr')).toBe(true)
  )
  expect(success).toHaveBeenLastCalledWith(
    'Échange réussi ! Ajouté : 9 007 199,254740993 EUR'
  )
  expect(result.current.redeeming).toBe(false)
})

it('does not report a credit after a rejected redemption and permits a retry', async () => {
  const success = vi
    .spyOn(toast, 'success')
    .mockImplementation(() => 'redemption-toast')
  const error = vi
    .spyOn(toast, 'error')
    .mockImplementation(() => 'redemption-error')
  const post = vi
    .spyOn(api, 'post')
    .mockResolvedValueOnce({
      data: { success: false, message: 'Voucher expired' },
    })
    .mockRejectedValueOnce(new Error('Redemption unavailable'))
    .mockResolvedValueOnce({
      data: { success: true, data: { nanos: '1', currency: 'USD' } },
    })
  const { result } = renderHook(() => useRedemption(), {
    wrapper: (props) => (
      <I18nextProvider i18n={i18n}>{props.children}</I18nextProvider>
    ),
  })
  await act(async () =>
    expect(await result.current.redeemCode('expired')).toBe(false)
  )
  expect(error).toHaveBeenLastCalledWith('Voucher expired')
  expect(success).not.toHaveBeenCalled()
  expect(result.current.redeeming).toBe(false)
  await act(async () =>
    expect(await result.current.redeemCode('unavailable')).toBe(false)
  )
  expect(success).not.toHaveBeenCalled()
  expect(result.current.redeeming).toBe(false)
  await act(async () =>
    expect(await result.current.redeemCode('valid')).toBe(true)
  )
  expect(success).toHaveBeenLastCalledWith(
    'Redemption successful! Added: 0.000000001 USD'
  )
  expect(post).toHaveBeenCalledTimes(3)
  expect(result.current.redeeming).toBe(false)
})

it('reads the authenticated wallet endpoint and rejects business failures', async () => {
  const get = vi
    .spyOn(api, 'get')
    .mockResolvedValueOnce({ data: { success: true, data: wallet } })
  const signal = new AbortController().signal
  await expect(getSelfWallet(signal)).resolves.toEqual(wallet)
  expect(get).toHaveBeenCalledWith('/api/user/self/wallet', { signal })
  get.mockResolvedValueOnce({
    data: { success: false, message: 'Database unavailable' },
  })
  await expect(getSelfWallet()).rejects.toThrow('Database unavailable')
})
