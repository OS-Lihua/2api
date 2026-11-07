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
// Values are i18n keys; render them with t().
export const SUCCESS_MESSAGES = {
  CREATED: 'Provider created',
  UPDATED: 'Provider updated',
  DELETED: 'Provider deleted',
  ENABLED: 'Provider enabled',
  DISABLED: 'Provider disabled',
} as const

export const ERROR_MESSAGES = {
  LOAD_FAILED: 'Failed to load providers',
  BALANCE_FAILED: 'Failed to update balance',
} as const

export const PROVIDER_STATUS_FILTER_OPTIONS = [
  { value: 'enabled', label: 'Enabled' },
  { value: 'disabled', label: 'Disabled' },
] as const

export const PROVIDER_ADD_MODE_OPTIONS = [
  { value: 'single', label: 'Single Key' },
  { value: 'batch', label: 'Batch Add (one provider per key line)' },
  { value: 'multi_to_single', label: 'Multi-Key (one provider, several keys)' },
] as const

export const PROVIDER_KEY_MODE_OPTIONS = [
  { value: 'append', label: 'Append to existing keys' },
  { value: 'replace', label: 'Replace all keys' },
] as const
