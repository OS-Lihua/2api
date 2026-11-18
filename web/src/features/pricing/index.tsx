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
import { getRouteApi } from '@tanstack/react-router'
import { useState } from 'react'
import { useTranslation } from 'react-i18next'

import { CopyButton } from '@/components/copy-button'
import { StaticDataTable } from '@/components/data-table/static/static-data-table'
import { EmptyState } from '@/components/empty-state'
import { ErrorState } from '@/components/error-state'
import { PublicLayout } from '@/components/layout/components/public-layout'
import { PageTransition } from '@/components/page-transition'
import { Badge } from '@/components/ui/badge'
import { Button, buttonVariants } from '@/components/ui/button'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Skeleton } from '@/components/ui/skeleton'
import { parseModelTags } from '@/features/models/lib/model-utils'
import { useAuthStore } from '@/stores/auth-store'

import { useOfferingCatalog, type CatalogOffering } from './catalog'
import { SearchBar } from './components/search-bar'
import { parseVisualBillingDocument } from './lib/billing-expression/visual'

export type CatalogSearch = {
  search: string
  sort: string
  vendor: string
  tag: string
  tokenUnit: 'M' | 'K'
  view: 'card' | 'table'
}

const defaultSearch: CatalogSearch = {
  search: '',
  sort: 'name',
  vendor: 'all',
  tag: 'all',
  tokenUnit: 'M',
  view: 'card',
}

function perThousand(value: string): string | null {
  if (!/^(?:\d+(?:\.\d*)?|\.\d+)$/.test(value)) return null
  const [whole, fraction = ''] = value.split('.')
  const digits = `${whole || '0'}${fraction}`.padStart(fraction.length + 4, '0')
  const split = digits.length - fraction.length - 3
  const integer = digits.slice(0, split).replace(/^0+(?=\d)/, '')
  const decimals = digits.slice(split).replace(/0+$/, '')
  return decimals ? `${integer}.${decimals}` : integer
}

export function OfferingPrice(props: {
  offering: CatalogOffering
  tokenUnit: 'M' | 'K'
}) {
  const { t } = useTranslation()
  const price = props.offering.price
  if (!price) return <p>{t('No current sale price')}</p>
  const document = parseVisualBillingDocument(price.expression)
  const tier = document?.root.kind === 'tier' ? document.root : null
  return (
    <div className='min-w-0 space-y-2'>
      <p className='text-sm font-medium'>{t('Base sale price')}</p>
      <p className='text-muted-foreground text-xs'>
        {t(
          'Base prices come from the current price version. Your group discount, request usage and pricing conditions determine the final charge.'
        )}
      </p>
      <Badge variant='outline'>{price.currency}</Badge>
      {tier?.billingUnit === 'request' && (
        <p>
          {tier.fixedPrice} {price.currency} / {t('request')}
        </p>
      )}
      {tier?.billingUnit === 'token' &&
        tier.prices.map((item) => {
          const value =
            props.tokenUnit === 'K' ? perThousand(item.value) : item.value
          if (value === null) return null
          return (
            <p key={item.variable} className='font-mono text-xs break-all'>
              {item.variable}: {value} {price.currency} /{' '}
              {props.tokenUnit === 'K' ? '1K' : '1M'} {t('tokens')}
            </p>
          )
        })}
      <p className='text-muted-foreground text-xs'>
        {t(
          'Token coefficients are per 1M tokens; fixed() is per request; expressions using u() return amounts in the expression currency.'
        )}
      </p>
      <div className='flex min-w-0 items-start gap-1'>
        <pre
          className='bg-muted min-w-0 flex-1 overflow-x-auto rounded-md p-3 text-xs break-all whitespace-pre-wrap'
          aria-label={t('Complete price expression')}
        >
          {price.expression}
        </pre>
        <CopyButton
          value={price.expression}
          aria-label={t('Copy price expression')}
        />
      </div>
    </div>
  )
}

function OfferingInformation(props: {
  offering: CatalogOffering
  tokenUnit: 'M' | 'K'
}) {
  const { t } = useTranslation()
  const { offering } = props
  return (
    <div className='min-w-0 space-y-6'>
      <div className='flex min-w-0 items-center gap-2'>
        <code className='break-all'>{offering.name}</code>
        <CopyButton
          value={offering.name}
          aria-label={t('Copy Offering name')}
        />
      </div>
      {offering.aliases.length > 0 && (
        <p className='break-all'>
          {t('Aliases')}: {offering.aliases.join(', ')}
        </p>
      )}
      <OfferingPrice offering={offering} tokenUnit={props.tokenUnit} />
      <h3 className='font-medium'>{t('Member Models')}</h3>
      <StaticDataTable
        data={offering.members}
        getRowKey={(member) => member.model_id}
        columns={[
          { id: 'tier', header: t('Tier'), cell: (member) => member.tier },
          {
            id: 'name',
            header: t('Model'),
            cell: (member) => (
              <div className='max-w-72 break-words whitespace-normal'>
                <p>{member.name}</p>
                <p className='text-muted-foreground text-xs'>
                  {member.description}
                </p>
              </div>
            ),
          },
          {
            id: 'publisher',
            header: t('Publisher'),
            cell: (member) => member.publisher_name || '-',
          },
          {
            id: 'tags',
            header: t('Tags'),
            cell: (member) => parseModelTags(member.tags).join(', ') || '-',
          },
        ]}
      />
      {offering.members.map(
        (member) =>
          member.capabilities &&
          Object.keys(member.capabilities).length > 0 && (
            <details key={member.model_id} className='min-w-0'>
              <summary className='wrap-anywhere'>
                {member.name} · {t('Capabilities')}
              </summary>
              <pre className='overflow-auto text-xs break-all whitespace-pre-wrap'>
                {JSON.stringify(member.capabilities, null, 2)}
              </pre>
            </details>
          )
      )}
      <h3 className='font-medium'>{t('API examples')}</h3>
      <p className='text-muted-foreground text-sm'>
        {t('Use the Offering name in API requests.')}
      </p>
      <pre className='bg-muted overflow-x-auto rounded-md p-3 text-xs break-all whitespace-pre-wrap'>
        {JSON.stringify({ model: offering.name }, null, 2)}
      </pre>
    </div>
  )
}

export function OfferingCatalog(props: {
  search?: CatalogSearch
  onSearchChange?: (values: CatalogSearch) => void
  detailName?: string
  onOpenPage?: (name: string) => void
  onBack?: () => void
}) {
  const { t } = useTranslation()
  const user = useAuthStore((state) => state.auth.user)
  const query = useOfferingCatalog()
  const [localSearch, setLocalSearch] = useState(defaultSearch)
  const search = props.search ?? localSearch
  const change = (patch: Partial<CatalogSearch>) => {
    const next = { ...search, ...patch }
    if (
      patch.search !== undefined ||
      patch.sort !== undefined ||
      patch.vendor !== undefined ||
      patch.tag !== undefined
    ) {
      setPage(1)
    }
    if (props.onSearchChange) props.onSearchChange(next)
    else setLocalSearch(next)
  }
  const [page, setPage] = useState(1)
  const [selectedID, setSelectedID] = useState<number | null>(null)
  const offerings = query.data?.data ?? []
  const publisherNames = new Map<number, string>()
  for (const offering of offerings) {
    for (const member of offering.members) {
      if (member.publisher_id > 0) {
        publisherNames.set(member.publisher_id, member.publisher_name)
      }
    }
  }
  const publishers = [...publisherNames].sort((a, b) =>
    a[1].localeCompare(b[1])
  )
  const tags = [
    ...new Set(
      offerings.flatMap((offering) =>
        offering.members.flatMap((member) => parseModelTags(member.tags))
      )
    ),
  ].sort()
  const filtered = offerings
    .filter((offering) => {
      const text = [
        offering.name,
        ...offering.aliases,
        ...offering.members.flatMap((member) => [
          member.name,
          member.description,
          member.publisher_name,
          ...parseModelTags(member.tags),
        ]),
      ]
        .join(' ')
        .toLowerCase()
      return (
        text.includes(search.search.toLowerCase()) &&
        (search.vendor === 'all' ||
          offering.members.some(
            (member) => String(member.publisher_id) === search.vendor
          )) &&
        (search.tag === 'all' ||
          offering.members.some((member) =>
            parseModelTags(member.tags).includes(search.tag)
          ))
      )
    })
    .sort((a, b) =>
      search.sort === 'name-desc'
        ? b.name.localeCompare(a.name)
        : a.name.localeCompare(b.name)
    )
  const pageCount = Math.max(1, Math.ceil(filtered.length / 12))
  const currentPage = Math.min(page, pageCount)
  const visible = filtered.slice((currentPage - 1) * 12, currentPage * 12)
  const selected = offerings.find((offering) => offering.id === selectedID)

  if (!user) {
    return (
      <EmptyState
        title={t('Sign in to view Offering prices')}
        description={t(
          'Sale prices are available for Offerings visible to your account.'
        )}
        action={
          <a
            className={buttonVariants()}
            href={`/sign-in?redirect=${encodeURIComponent(window.location.pathname + window.location.search)}`}
          >
            {t('Sign in')}
          </a>
        }
      />
    )
  }
  if (query.isPending) {
    return (
      <Skeleton
        className='h-64 w-full'
        role='status'
        aria-label={t('Loading...')}
      />
    )
  }
  if (query.isError) {
    return (
      <ErrorState
        title={t('Unable to load Offering prices')}
        onRetry={() => void query.refetch()}
      />
    )
  }
  if (props.detailName !== undefined) {
    const offering = offerings.find(
      (item) =>
        item.name === props.detailName ||
        item.aliases.includes(props.detailName ?? '')
    )
    return (
      <div className='mx-auto w-full max-w-5xl space-y-6 p-4 md:p-8'>
        <Button variant='outline' onClick={props.onBack}>
          {t('Back to pricing')}
        </Button>
        {offering ? (
          <>
            <h1 className='text-2xl font-semibold break-all'>
              {offering.name}
            </h1>
            <OfferingInformation
              offering={offering}
              tokenUnit={search.tokenUnit}
            />
          </>
        ) : (
          <EmptyState
            title={t('Offering unavailable')}
            description={t('This Offering is not visible to your account.')}
          />
        )}
      </div>
    )
  }
  return (
    <div className='mx-auto w-full max-w-7xl space-y-6 p-4 md:p-8'>
      <h1 className='text-2xl font-semibold'>{t('Offering sale catalog')}</h1>
      <div className='flex flex-wrap items-center gap-3'>
        <SearchBar
          value={search.search}
          onChange={(value) => change({ search: value })}
          onClear={() => change({ search: '' })}
        />
        <Select
          items={[
            { value: 'all', label: t('All publishers') },
            ...publishers.map(([id, name]) => ({
              value: String(id),
              label: name,
            })),
          ]}
          value={search.vendor}
          onValueChange={(value) => change({ vendor: value ?? 'all' })}
        >
          <SelectTrigger
            aria-label={t('Publisher')}
            className='h-auto max-w-full min-w-0 *:data-[slot=select-value]:line-clamp-none'
          >
            <SelectValue className='min-w-0 wrap-anywhere whitespace-normal' />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value='all'>{t('All publishers')}</SelectItem>
            {publishers.map(([id, name]) => (
              <SelectItem key={id} value={String(id)}>
                <span className='min-w-0 wrap-anywhere whitespace-normal'>
                  {name}
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          items={[
            { value: 'all', label: t('All tags') },
            ...tags.map((tag) => ({ value: tag, label: tag })),
          ]}
          value={search.tag}
          onValueChange={(value) => change({ tag: value ?? 'all' })}
        >
          <SelectTrigger
            aria-label={t('Tags')}
            className='h-auto max-w-full min-w-0 *:data-[slot=select-value]:line-clamp-none'
          >
            <SelectValue className='min-w-0 wrap-anywhere whitespace-normal' />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value='all'>{t('All tags')}</SelectItem>
            {tags.map((tag) => (
              <SelectItem key={tag} value={tag}>
                <span className='min-w-0 wrap-anywhere whitespace-normal'>
                  {tag}
                </span>
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
        <Select
          items={[
            { value: 'name', label: t('Name ascending') },
            { value: 'name-desc', label: t('Name descending') },
          ]}
          value={search.sort}
          onValueChange={(value) => change({ sort: value ?? 'name' })}
        >
          <SelectTrigger aria-label={t('Sort')}>
            <SelectValue />
          </SelectTrigger>
          <SelectContent>
            <SelectItem value='name'>{t('Name ascending')}</SelectItem>
            <SelectItem value='name-desc'>{t('Name descending')}</SelectItem>
          </SelectContent>
        </Select>
        <Button
          variant='outline'
          onClick={() =>
            change({ tokenUnit: search.tokenUnit === 'M' ? 'K' : 'M' })
          }
        >
          {search.tokenUnit === 'M' ? '1M' : '1K'} {t('tokens')}
        </Button>
        <Button
          variant='outline'
          onClick={() =>
            change({ view: search.view === 'card' ? 'table' : 'card' })
          }
        >
          {search.view === 'card' ? t('Table view') : t('Card view')}
        </Button>
      </div>
      {filtered.length === 0 && (
        <EmptyState title={t('No visible Offerings')} />
      )}
      {search.view === 'card' ? (
        <div className='grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3'>
          {visible.map((offering) => (
            <article
              key={offering.id}
              className='min-w-0 space-y-3 rounded-xl border p-4'
            >
              <div className='flex min-w-0 items-center gap-2'>
                <Button
                  variant='link'
                  className='h-auto min-w-0 flex-1 shrink justify-start p-0 text-left break-all whitespace-normal'
                  onClick={() => setSelectedID(offering.id)}
                >
                  {offering.name}
                </Button>
                <CopyButton
                  value={offering.name}
                  aria-label={t('Copy Offering name')}
                />
              </div>
              <OfferingPrice offering={offering} tokenUnit={search.tokenUnit} />
            </article>
          ))}
        </div>
      ) : (
        <StaticDataTable
          data={visible}
          getRowKey={(offering) => offering.id}
          columns={[
            {
              id: 'name',
              header: t('Offering'),
              cell: (offering) => (
                <Button
                  variant='link'
                  onClick={() => setSelectedID(offering.id)}
                >
                  {offering.name}
                </Button>
              ),
            },
            {
              id: 'price',
              header: t('Base sale price'),
              cell: (offering) => (
                <OfferingPrice
                  offering={offering}
                  tokenUnit={search.tokenUnit}
                />
              ),
            },
          ]}
        />
      )}
      {filtered.length > 0 && (
        <nav
          className='flex items-center justify-end gap-3'
          aria-label={t('Pagination')}
        >
          <Button
            variant='outline'
            disabled={currentPage === 1}
            onClick={() => setPage(currentPage - 1)}
          >
            {t('Previous')}
          </Button>
          <span>
            {currentPage} / {pageCount}
          </span>
          <Button
            variant='outline'
            disabled={currentPage === pageCount}
            onClick={() => setPage(currentPage + 1)}
          >
            {t('Next')}
          </Button>
        </nav>
      )}
      <Sheet
        open={Boolean(selected)}
        onOpenChange={(open) => {
          if (!open) setSelectedID(null)
        }}
      >
        <SheetContent className='w-full overflow-y-auto sm:max-w-3xl'>
          <SheetHeader>
            <SheetTitle className='break-all'>{selected?.name}</SheetTitle>
            <SheetDescription>
              {t('Offering sale price and member Models')}
            </SheetDescription>
          </SheetHeader>
          {selected && (
            <div className='min-w-0 space-y-6 px-4 pb-6'>
              <OfferingInformation
                offering={selected}
                tokenUnit={search.tokenUnit}
              />
              <Button onClick={() => props.onOpenPage?.(selected.name)}>
                {t('Open details page')}
              </Button>
            </div>
          )}
        </SheetContent>
      </Sheet>
    </div>
  )
}

const listRoute = getRouteApi('/pricing/')
const detailRoute = getRouteApi('/pricing/$modelId/')

export function Pricing() {
  const search = listRoute.useSearch()
  const navigate = listRoute.useNavigate()
  return (
    <PublicLayout>
      <PageTransition>
        <OfferingCatalog
          search={search}
          onSearchChange={(next) =>
            void navigate({ search: next, replace: true })
          }
          onOpenPage={(name) =>
            void navigate({
              to: '/pricing/$modelId',
              params: { modelId: name },
              search,
            })
          }
        />
      </PageTransition>
    </PublicLayout>
  )
}

export function OfferingDetails() {
  const search = detailRoute.useSearch()
  const { modelId } = detailRoute.useParams()
  const navigate = detailRoute.useNavigate()
  return (
    <PublicLayout>
      <PageTransition>
        <OfferingCatalog
          search={search}
          detailName={modelId}
          onBack={() => void navigate({ to: '/pricing', search })}
        />
      </PageTransition>
    </PublicLayout>
  )
}
