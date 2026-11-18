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
import { useQuery } from '@tanstack/react-query'
import { z } from 'zod'

import { api } from '@/lib/api'
import { requireServerSuccess } from '@/lib/server-error-message'
import { useAuthStore } from '@/stores/auth-store'

const catalogOfferingSchema = z.object({
  id: z.number().int().positive(),
  name: z.string().min(1),
  aliases: z.array(z.string()),
  members: z.array(
    z.object({
      model_id: z.number().int().positive(),
      name: z.string(),
      tier: z.number().int().positive(),
      description: z.string(),
      icon: z.string(),
      tags: z.string(),
      publisher_id: z.number().int().nonnegative(),
      publisher_name: z.string(),
      capabilities: z.record(z.string(), z.unknown()).nullable(),
    })
  ),
  price: z
    .object({
      id: z.number().int().positive(),
      currency: z.string().regex(/^[A-Z]{3}$/),
      expression: z.string().min(1),
      effective_at: z.number().int().positive(),
    })
    .nullable(),
})

const catalogSchema = z.object({
  success: z.literal(true),
  data: z.array(catalogOfferingSchema),
  as_of: z.number().int().positive(),
})

export type CatalogOffering = z.infer<typeof catalogOfferingSchema>

export function useOfferingCatalog() {
  const user = useAuthStore((state) => state.auth.user)
  const sessionID = useAuthStore((state) => state.auth.session?.sid)
  return useQuery({
    queryKey: ['offering-catalog', user?.id, sessionID, user?.group],
    enabled: Boolean(user),
    queryFn: async ({ signal }) => {
      const response = await api.get('/api/pricing', { signal })
      const current = useAuthStore.getState().auth
      if (current.user?.id !== user?.id || current.session?.sid !== sessionID) {
        throw new Error('Account changed')
      }
      return catalogSchema.parse(requireServerSuccess(response.data))
    },
    staleTime: 0,
    refetchInterval: 30_000,
    retry: false,
  })
}
