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
import { getRouteApi, useNavigate } from '@tanstack/react-router'
import { lazy, Suspense } from 'react'
import { useTranslation } from 'react-i18next'

import { SectionPageLayout } from '@/components/layout'
import { LoadingState } from '@/components/loading-state'
import { Tabs, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { ROLE } from '@/lib/roles'
import { useAuthStore } from '@/stores/auth-store'

import { FinanceDashboard } from './components/finance-dashboard'
import { OverviewDashboard } from './components/overview/overview-dashboard'
import {
  DASHBOARD_DEFAULT_SECTION,
  DASHBOARD_SECTION_IDS,
  type DashboardSectionId,
} from './section-registry'

const route = getRouteApi('/_authenticated/dashboard/$section')
const PerformanceOverview = lazy(() =>
  import('./components/models/performance-overview').then((module) => ({
    default: module.PerformanceOverview,
  }))
)
const titles: Record<DashboardSectionId, string> = {
  overview: 'Overview',
  models: 'Model Call Analytics',
  flow: 'Flow',
  users: 'User Analytics',
}
export function Dashboard() {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const params = route.useParams()
  const user = useAuthStore((state) => state.auth.user)
  const sid = useAuthStore((state) => state.auth.session?.sid)
  const section = (params.section ??
    DASHBOARD_DEFAULT_SECTION) as DashboardSectionId
  const admin = (user?.role ?? 0) >= ROLE.ADMIN
  if (section === 'overview') {
    return <OverviewDashboard key={`${user?.id}:${sid}`} />
  }
  return (
    <SectionPageLayout>
      <SectionPageLayout.Title>{t(titles[section])}</SectionPageLayout.Title>
      <SectionPageLayout.Content>
        <div className='min-w-0 space-y-4'>
          <Tabs
            value={section}
            onValueChange={(value) =>
              void navigate({
                to: '/dashboard/$section',
                params: { section: value as DashboardSectionId },
              })
            }
          >
            <TabsList className='max-w-full flex-wrap group-data-horizontal/tabs:h-auto'>
              {DASHBOARD_SECTION_IDS.filter(
                (value) => value !== 'overview' && (value !== 'users' || admin)
              ).map((value) => (
                <TabsTrigger key={value} value={value}>
                  {t(titles[value])}
                </TabsTrigger>
              ))}
            </TabsList>
          </Tabs>
          <FinanceDashboard view={section} />
          {section === 'models' && admin && (
            <Suspense fallback={<LoadingState />}>
              <PerformanceOverview />
            </Suspense>
          )}
        </div>
      </SectionPageLayout.Content>
    </SectionPageLayout>
  )
}
