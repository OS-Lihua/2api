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
import type { TFunction } from 'i18next'
import z from 'zod'

import type { ModelOffering, ModelOfferingInput } from '../types'

export function offeringFormSchema(t: TFunction) {
  const ids = z.array(z.number().int().positive().safe())
  const name = z
    .string()
    .trim()
    .refine(
      (value) =>
        [...value].length >= 1 &&
        [...value].length <= 128 &&
        !/[\s\p{Cc}]/u.test(value),
      t(
        'ModelOffering names must contain 1 to 128 characters without whitespace'
      )
    )
  return z
    .object({
      name: name.min(1, t('Primary name is required')),
      aliases: z.array(name),
      visibility: z.enum(['all', 'groups']),
      groups: z.array(z.string().min(1)),
      tiers: z
        .array(
          z.object({
            tier: z
              .number({ error: t('Enter a positive integer') })
              .int(t('Enter a positive integer'))
              .positive(t('Enter a positive integer'))
              .safe(t('Enter a positive integer')),
            model_ids: z
              .array(z.string())
              .min(1, t('Select at least one model in each tier')),
          })
        )
        .min(1, t('Add at least one member tier')),
      channel_scope: z.object({
        include_provider_ids: ids,
        exclude_provider_ids: ids,
        include_channel_ids: ids,
        exclude_channel_ids: ids,
      }),
      route_method: z.enum(['cost', 'weight', 'latency']),
      accepted_conversion_losses: z.array(z.string().trim().min(1)),
      minimum_margin_rate: z
        .string()
        .trim()
        .regex(
          /^(?:0(?:\.\d+)?|1(?:\.0+)?)$/,
          t('Enter a decimal rate from 0 to 1')
        ),
      allow_loss_fallback: z.boolean(),
    })
    .superRefine((value, ctx) => {
      if (value.aliases.includes(value.name)) {
        ctx.addIssue({
          code: 'custom',
          path: ['aliases'],
          message: t('An alias cannot equal the primary name'),
        })
      }
      if (value.visibility === 'groups' && value.groups.length === 0) {
        ctx.addIssue({
          code: 'custom',
          path: ['groups'],
          message: t('Select at least one user group'),
        })
      }
      if (
        value.tiers.length > 0 &&
        !value.tiers.some((tier) => tier.tier === 1)
      ) {
        ctx.addIssue({
          code: 'custom',
          path: ['tiers', 0, 'tier'],
          message: t('Include a member tier numbered 1'),
        })
      }
      const tiers = new Set<number>()
      const models = new Set<string>()
      value.tiers.forEach((tier, index) => {
        if (tiers.has(tier.tier)) {
          ctx.addIssue({
            code: 'custom',
            path: ['tiers', index, 'tier'],
            message: t('Member tier numbers must be unique'),
          })
        }
        tiers.add(tier.tier)
        for (const id of tier.model_ids) {
          if (models.has(id)) {
            ctx.addIssue({
              code: 'custom',
              path: ['tiers', index, 'model_ids'],
              message: t('A model can belong to only one member tier'),
            })
          }
          models.add(id)
        }
      })
      for (const kind of ['provider', 'channel'] as const) {
        const included = value.channel_scope[`include_${kind}_ids`]
        const excluded = value.channel_scope[`exclude_${kind}_ids`]
        if (included.some((id) => excluded.includes(id))) {
          ctx.addIssue({
            code: 'custom',
            path: ['channel_scope', `exclude_${kind}_ids`],
            message: t('The same item cannot be both included and excluded'),
          })
        }
      }
    })
}

export type OfferingFormValues = z.infer<ReturnType<typeof offeringFormSchema>>

export function offeringFormValues(
  offering?: ModelOffering
): OfferingFormValues {
  if (!offering) {
    return {
      name: '',
      aliases: [],
      visibility: 'all',
      groups: [],
      tiers: [{ tier: 1, model_ids: [] }],
      channel_scope: {
        include_provider_ids: [],
        exclude_provider_ids: [],
        include_channel_ids: [],
        exclude_channel_ids: [],
      },
      route_method: 'cost',
      accepted_conversion_losses: [],
      minimum_margin_rate: '0',
      allow_loss_fallback: false,
    }
  }
  const tiers = new Map<number, string[]>()
  for (const member of offering.members) {
    tiers.set(member.tier, [
      ...(tiers.get(member.tier) ?? []),
      String(member.model_id),
    ])
  }
  return {
    name: offering.name,
    aliases: offering.aliases,
    visibility: offering.visibility,
    groups: offering.groups,
    tiers: [...tiers]
      .sort(([a], [b]) => a - b)
      .map(([tier, model_ids]) => ({ tier, model_ids })),
    channel_scope: offering.channel_scope,
    route_method: offering.route_method,
    accepted_conversion_losses: offering.accepted_conversion_losses,
    minimum_margin_rate: offering.minimum_margin_rate,
    allow_loss_fallback: offering.allow_loss_fallback,
  }
}

export function offeringPayload(
  values: OfferingFormValues
): ModelOfferingInput {
  return {
    name: values.name,
    aliases: values.aliases,
    visibility: values.visibility,
    groups: values.visibility === 'groups' ? values.groups : [],
    members: values.tiers.flatMap((tier) =>
      tier.model_ids.map((id) => ({ model_id: Number(id), tier: tier.tier }))
    ),
    channel_scope: values.channel_scope,
    route_method: values.route_method,
    accepted_conversion_losses: values.accepted_conversion_losses,
    minimum_margin_rate: values.minimum_margin_rate,
    allow_loss_fallback: values.allow_loss_fallback,
  }
}
