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
import { toast } from 'sonner'

import { PricingIssueList } from '../components/pricing-issue-list'
import type { TaskPluginPricingDiagnostics } from '../types'
import { isTaskPluginScopeCurrent } from './pricing-diagnostics'

/** Saving succeeded independently of the availability of post-save diagnostics. */
export function toastPluginChange(
  title: string,
  diagnostics: TaskPluginPricingDiagnostics,
  pluginKey: string,
  scope: string
) {
  let id: string | number | undefined
  let dismissed = false
  const showDiagnostics = (current: TaskPluginPricingDiagnostics) => {
    if (dismissed || !isTaskPluginScopeCurrent(scope)) return
    const options = {
      id,
      duration: undefined,
      description: (
        <PricingIssueList
          diagnostics={current}
          pluginKey={pluginKey}
          scope={scope}
          onDiagnosticsChange={showDiagnostics}
          saved
        />
      ),
      onDismiss: () => {
        dismissed = true
      },
      onAutoClose: () => {
        dismissed = true
      },
    }
    // Updating the same toast lets Sonner remeasure its changed description.
    if (current.state === 'ready' && current.issues.length === 0) {
      id = toast.success(title, options)
    } else {
      id = toast.warning(title, { ...options, duration: Infinity })
    }
  }
  showDiagnostics(diagnostics)
}
