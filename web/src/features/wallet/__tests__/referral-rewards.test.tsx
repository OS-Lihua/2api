/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, cleanup, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

import { api } from '@/lib/api'
import { useAuthStore } from '@/stores/auth-store'

import { AffiliateRewardsCard } from '../components/affiliate-rewards-card'
import { TransferDialog } from '../components/dialogs/transfer-dialog'
import { useAffiliate } from '../hooks/use-affiliate'

let client: QueryClient
const available = { nanos: '9007199254740993', currency: 'USD' }
const rewards = {
  available,
  earned: { nanos: '19007199254740993', currency: 'USD' },
  invited: 3,
}

function Rewards() {
  const account = useAuthStore((state) => state.auth.user?.id)
  return account ? <RewardsContent key={account} userId={account} /> : null
}

function RewardsContent(props: { userId: number }) {
  const affiliate = useAffiliate(props.userId)
  const [open, setOpen] = useState(false)
  return (
    <>
      <AffiliateRewardsCard
        rewards={affiliate.rewards}
        hasPendingTransfer={affiliate.transferAttempt !== null}
        affiliateLink={affiliate.affiliateLink}
        onTransfer={() => setOpen(true)}
        loading={affiliate.loading}
        error={affiliate.error}
        onRetry={() => void affiliate.refetch()}
      />
      {affiliate.rewards && (
        <TransferDialog
          open={open}
          onOpenChange={setOpen}
          available={affiliate.rewards.available}
          attempt={affiliate.transferAttempt}
          onConfirm={affiliate.transferRewards}
          transferring={affiliate.transferring}
        />
      )}
    </>
  )
}

beforeEach(() => {
  useAuthStore.getState().auth.setUser({ id: 1, username: 'inviter', role: 1 })
  client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  vi.spyOn(api, 'get').mockImplementation(async (url) => ({
    data: {
      success: true,
      data: url === '/api/user/aff' ? 'referral-code' : rewards,
    },
  }))
})

afterEach(() => {
  cleanup()
  client.clear()
  useAuthStore.getState().auth.reset()
  vi.restoreAllMocks()
})

function renderRewards() {
  return render(
    <QueryClientProvider client={client}>
      <Rewards />
    </QueryClientProvider>
  )
}

describe('exact referral rewards', () => {
  it('shows exact pending and cumulative money and transfers one nano without a legacy minimum', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockResolvedValue({ data: { success: true } })
    const user = userEvent.setup()
    renderRewards()
    expect(await screen.findByText('9,007,199.254740993 USD')).toBeVisible()
    expect(screen.getByText('19,007,199.254740993 USD')).toBeVisible()
    await user.click(screen.getByRole('button', { name: 'Transfer to wallet' }))
    const input = screen.getByLabelText('Transfer Amount')
    await user.clear(input)
    await user.type(input, '0.000000001')
    await user.click(screen.getByRole('button', { name: 'Transfer' }))
    await waitFor(() =>
      expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    )
    expect(post).toHaveBeenCalledWith(
      '/api/user/aff_transfer',
      {
        amount: { nanos: '1', currency: 'USD' },
        transfer_id: expect.any(String),
      },
      { skipBusinessError: true }
    )
  })

  it('retains the exact amount and identity after a lost response, including closing and reopening', async () => {
    const post = vi
      .spyOn(api, 'post')
      .mockRejectedValueOnce(new Error('Connection interrupted'))
      .mockResolvedValueOnce({ data: { success: true } })
    const user = userEvent.setup()
    renderRewards()
    await user.click(
      await screen.findByRole('button', { name: 'Transfer to wallet' })
    )
    await user.click(screen.getByRole('button', { name: 'Transfer' }))
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Connection interrupted'
    )
    expect(screen.getByLabelText('Transfer Amount')).toHaveValue(
      '9007199.254740993'
    )
    expect(screen.getByLabelText('Transfer Amount')).toBeDisabled()
    await user.click(screen.getByRole('button', { name: 'Cancel' }))
    await user.click(screen.getByRole('button', { name: 'Transfer to wallet' }))
    await user.click(screen.getByRole('button', { name: 'Transfer' }))
    await waitFor(() => expect(post).toHaveBeenCalledTimes(2))
    expect(post.mock.calls[0]?.[1]).toEqual(post.mock.calls[1]?.[1])
    expect(post.mock.calls[0]?.[1]).toEqual({
      amount: available,
      transfer_id: expect.any(String),
    })
  })

  it('keeps input and confirmation disabled during a transfer and discards a late response after account change', async () => {
    let finish: ((value: unknown) => void) | undefined
    const post = vi.spyOn(api, 'post').mockImplementation(
      () =>
        new Promise((resolve) => {
          finish = resolve
        })
    )
    const user = userEvent.setup()
    renderRewards()
    await user.click(
      await screen.findByRole('button', { name: 'Transfer to wallet' })
    )
    await user.click(screen.getByRole('button', { name: 'Transfer' }))
    expect(screen.getByRole('button', { name: 'Transfer' })).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Cancel' })).toBeDisabled()
    expect(screen.getByLabelText('Transfer Amount')).toBeDisabled()
    await act(async () => {
      useAuthStore
        .getState()
        .auth.setUser({ id: 2, username: 'other', role: 1 })
    })
    await act(async () => {
      finish?.({ data: { success: true } })
    })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(post).toHaveBeenCalledTimes(1)
  })

  it.each(['0', '-1', '1e3', '0.0000000001', '9007199.254740994'])(
    'rejects invalid or excessive amount %s before sending a transfer',
    async (value) => {
      const post = vi.spyOn(api, 'post')
      const user = userEvent.setup()
      renderRewards()
      await user.click(
        await screen.findByRole('button', { name: 'Transfer to wallet' })
      )
      await user.clear(screen.getByLabelText('Transfer Amount'))
      await user.type(screen.getByLabelText('Transfer Amount'), value)
      await user.click(screen.getByRole('button', { name: 'Transfer' }))
      expect(await screen.findByRole('alert')).toHaveTextContent(
        'Invalid amount'
      )
      expect(post).not.toHaveBeenCalled()
    }
  )

  it('shows a retryable error instead of a zero balance when the rewards request fails', async () => {
    vi.mocked(api.get).mockImplementation(async (url) => ({
      data:
        url === '/api/user/aff'
          ? { success: true, data: 'referral-code' }
          : { success: false, message: 'Rewards unavailable' },
    }))
    renderRewards()
    expect(await screen.findByText('Rewards unavailable')).toBeVisible()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeEnabled()
    expect(
      screen.queryByRole('button', { name: 'Transfer to wallet' })
    ).not.toBeInTheDocument()
    expect(screen.queryByText('0 USD')).not.toBeInTheDocument()
  })
})
