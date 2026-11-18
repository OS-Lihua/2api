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
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { StaticDataTable } from '@/components/data-table/static/static-data-table'
import { StaticRowActions } from '@/components/data-table/static/static-row-actions'
import { Button } from '@/components/ui/button'
import { toIntlLocale } from '@/i18n/languages'
import { formatMoneyAmount, parseDecimalMoneyAmount } from '@/lib/currency'

import {
  CreemProductDialog,
  type CreemProductData,
} from './creem-product-dialog'

export function CreemProductsVisualEditor(props: {
  provider: 'stripe' | 'creem'
  value: CreemProductData[]
  onChange: (products: CreemProductData[]) => void
  disabled?: boolean
}) {
  const { t, i18n } = useTranslation()
  const locale = toIntlLocale(i18n.resolvedLanguage || i18n.language)
  const [editing, setEditing] = useState<number | null>(null)
  return (
    <div className='w-full max-w-full min-w-0 space-y-3'>
      <StaticDataTable
        data={props.value}
        getRowKey={(product) => product.product_id}
        emptyContent={t(
          'No products configured. Click "Add product" to get started.'
        )}
        columns={[
          {
            id: 'name',
            header: t('Product name'),
            cell: (product) => product.name,
          },
          {
            id: 'id',
            header:
              props.provider === 'stripe'
                ? t('Stripe Price ID')
                : t('Creem Product ID'),
            cell: (product) => product.product_id,
          },
          {
            id: 'reward',
            header: t('Reward amount per item'),
            cell: (product) =>
              formatMoneyAmount(
                parseDecimalMoneyAmount(product.amount, product.currency),
                locale
              ),
          },
          {
            id: 'actions',
            header: t('Actions'),
            cell: (_product, index) => (
              <StaticRowActions
                editLabel={t('Edit')}
                deleteLabel={t('Delete')}
                menuLabel={t('Open menu')}
                editDisabled={props.disabled}
                deleteDisabled={props.disabled}
                onEdit={() => setEditing(index)}
                onDelete={() =>
                  props.onChange(
                    props.value.filter(
                      (_, productIndex) => productIndex !== index
                    )
                  )
                }
              />
            ),
          },
        ]}
      />
      <Button
        type='button'
        variant='outline'
        disabled={props.disabled}
        onClick={() => setEditing(-1)}
      >
        {t('Add external product')}
      </Button>
      {editing !== null && (
        <CreemProductDialog
          provider={props.provider}
          editData={props.value[editing] ?? null}
          productIDs={props.value
            .filter((_, index) => index !== editing)
            .map((product) => product.product_id)}
          onClose={() => setEditing(null)}
          onSave={(product) => {
            if (editing === -1) {
              props.onChange([...props.value, product])
            } else {
              props.onChange(
                props.value.map((previous, index) =>
                  index === editing ? product : previous
                )
              )
            }
          }}
        />
      )}
    </div>
  )
}
