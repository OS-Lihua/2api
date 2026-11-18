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
import { z } from 'zod'

const rewardAmountSchema = z.object({
  nanos: z.string().regex(/^\d+$/),
  currency: z.string().regex(/^[A-Z]{3}$/),
})

export const referralRewardsSchema = z
  .object({
    available: rewardAmountSchema.refine(
      (amount) =>
        /^\d+$/.test(amount.nanos) &&
        BigInt(amount.nanos) <= 9_223_372_036_854_775_807n
    ),
    earned: rewardAmountSchema,
    invited: z.number().int().nonnegative().max(Number.MAX_SAFE_INTEGER),
  })
  .refine((rewards) => rewards.available.currency === rewards.earned.currency)

export type ReferralRewards = z.infer<typeof referralRewardsSchema>
