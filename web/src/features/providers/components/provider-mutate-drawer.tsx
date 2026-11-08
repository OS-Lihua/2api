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
import { zodResolver } from '@hookform/resolvers/zod'
import { useQueryClient } from '@tanstack/react-query'
import { useEffect, useState } from 'react'
import { type Path, useForm, type UseFormReturn } from 'react-hook-form'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'

import {
  SideDrawerSection,
  sideDrawerContentClassName,
  sideDrawerFooterClassName,
  sideDrawerFormClassName,
  sideDrawerHeaderClassName,
} from '@/components/drawer-layout'
import { JsonEditor } from '@/components/json-editor'
import { Button } from '@/components/ui/button'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import {
  Select,
  SelectContent,
  SelectGroup,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import {
  Sheet,
  SheetClose,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from '@/components/ui/sheet'
import { Textarea } from '@/components/ui/textarea'
import { AdapterDeploymentField } from '@/features/channels/components/drawers/adapter-deployment-field'
import {
  MULTI_KEY_MODES,
  channelTypeOptionsForTaskPluginBind,
} from '@/features/channels/constants'
import { getAdvancedCustomTemplateConfig } from '@/features/channels/lib/advanced-custom'
import { CHANNEL_FORM_DEFAULT_VALUES } from '@/features/channels/lib/channel-form'
import { RecommendationPicker } from '@/features/recommendations/recommendation-picker'
import {
  ADMIN_PERMISSION_ACTIONS,
  ADMIN_PERMISSION_RESOURCES,
  hasPermission,
} from '@/lib/admin-permissions'
import { handleServerError } from '@/lib/handle-server-error'
import { useAuthStore } from '@/stores/auth-store'

import {
  createProvider,
  getProvider,
  providersQueryKeys,
  updateProvider,
} from '../api'
import {
  PROVIDER_ADD_MODE_OPTIONS,
  PROVIDER_KEY_MODE_OPTIONS,
  SUCCESS_MESSAGES,
} from '../constants'
import {
  PROVIDER_FORM_DEFAULT_VALUES,
  type ProviderFormValues,
  getProviderFormSchema,
  providerToFormValues,
  toCreateProviderPayload,
  toUpdateProviderPayload,
} from '../lib/provider-form'
import type { Provider } from '../types'

type SelectOption = { value: string; label: string }

const adapterSettingsTemplates: Record<number, Record<string, unknown>> = {
  3: { azure_responses_version: 'preview' },
  20: {
    openrouter_enterprise: CHANNEL_FORM_DEFAULT_VALUES.is_enterprise_account,
  },
  33: {
    aws_key_type: CHANNEL_FORM_DEFAULT_VALUES.aws_key_type,
    aws_api_type: CHANNEL_FORM_DEFAULT_VALUES.aws_api_type,
  },
  41: {
    vertex_key_type: CHANNEL_FORM_DEFAULT_VALUES.vertex_key_type,
    vertex_project_id: '',
    vertex_api_version: CHANNEL_FORM_DEFAULT_VALUES.vertex_api_version,
  },
  58: { advanced_custom: getAdvancedCustomTemplateConfig('all_protocols') },
}

function ProviderSelectField(props: {
  form: UseFormReturn<ProviderFormValues>
  name: Path<ProviderFormValues>
  label: string
  options: SelectOption[]
  disabled?: boolean
  description?: string
  toValue?: (value: string) => string | number
}) {
  return (
    <FormField
      control={props.form.control}
      name={props.name}
      render={({ field }) => (
        <FormItem>
          <FormLabel>{props.label}</FormLabel>
          <Select
            disabled={props.disabled}
            items={props.options}
            value={String(field.value)}
            onValueChange={(value) =>
              value !== null &&
              field.onChange(props.toValue ? props.toValue(value) : value)
            }
          >
            <FormControl>
              <SelectTrigger className='w-full'>
                <SelectValue />
              </SelectTrigger>
            </FormControl>
            <SelectContent alignItemWithTrigger={false}>
              <SelectGroup>
                {props.options.map((option) => (
                  <SelectItem key={option.value} value={option.value}>
                    {option.label}
                  </SelectItem>
                ))}
              </SelectGroup>
            </SelectContent>
          </Select>
          {props.description && (
            <FormDescription>{props.description}</FormDescription>
          )}
          <FormMessage />
        </FormItem>
      )}
    />
  )
}

type ProviderMutateDrawerProps = {
  open: boolean
  onOpenChange: (open: boolean) => void
  currentRow?: Provider
}

export function ProviderMutateDrawer(props: ProviderMutateDrawerProps) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const currentUser = useAuthStore((state) => state.auth.user)
  const canBindTaskPlugin = hasPermission(
    currentUser,
    ADMIN_PERMISSION_RESOURCES.TASK_PLUGIN,
    ADMIN_PERMISSION_ACTIONS.BIND
  )
  const providerId = props.currentRow?.id
  const isUpdate = providerId !== undefined
  const [loaded, setLoaded] = useState<Provider | null>(null)
  const [isSubmitting, setIsSubmitting] = useState(false)

  const form = useForm<ProviderFormValues>({
    resolver: zodResolver(getProviderFormSchema(t, isUpdate)),
    defaultValues: PROVIDER_FORM_DEFAULT_VALUES,
  })

  useEffect(() => {
    setLoaded(null)
    form.reset(PROVIDER_FORM_DEFAULT_VALUES)
    if (!props.open || providerId === undefined) return
    let ignore = false
    getProvider(providerId)
      .then((result) => {
        if (ignore) return
        if (!result.success || !result.data) {
          handleServerError(result, t('Failed to load'))
          return
        }
        setLoaded(result.data)
        form.reset(providerToFormValues(result.data))
      })
      .catch((error: unknown) => {
        if (!ignore) handleServerError(error)
      })
    return () => {
      ignore = true
    }
  }, [props.open, providerId, form, t])

  const isReady = !isUpdate || loaded !== null
  const mode = form.watch('mode')
  const type = form.watch('type')
  const key = form.watch('key')
  const isMultiKey = loaded?.channel_info?.is_multi_key === true

  const onSubmit = async (values: ProviderFormValues) => {
    setIsSubmitting(true)
    try {
      const result =
        isUpdate && loaded
          ? await updateProvider(
              loaded.id,
              toUpdateProviderPayload(values, loaded)
            )
          : await createProvider(toCreateProviderPayload(values))
      if (!result.success) {
        handleServerError(result)
        return
      }
      toast.success(
        t(isUpdate ? SUCCESS_MESSAGES.UPDATED : SUCCESS_MESSAGES.CREATED)
      )
      await queryClient.invalidateQueries({
        queryKey: providersQueryKeys.lists(),
      })
      props.onOpenChange(false)
    } catch (error) {
      handleServerError(error)
    } finally {
      setIsSubmitting(false)
    }
  }

  const typeOptions = channelTypeOptionsForTaskPluginBind(
    canBindTaskPlugin
  ).map((option) => ({ value: String(option.value), label: option.label }))
  const translate = (options: readonly SelectOption[]) =>
    options.map((option) => ({ value: option.value, label: t(option.label) }))
  let submitLabel = t('Save changes')
  if (!isReady) submitLabel = t('Loading...')
  if (isSubmitting) submitLabel = t('Saving...')

  return (
    <Sheet open={props.open} onOpenChange={props.onOpenChange}>
      <SheetContent className={sideDrawerContentClassName('sm:max-w-[600px]')}>
        <SheetHeader className={sideDrawerHeaderClassName()}>
          <SheetTitle>
            {isUpdate ? t('Edit Provider') : t('New Provider')}
          </SheetTitle>
          <SheetDescription>
            {t(
              'A provider is one upstream account: its address, credentials, and account quota.'
            )}
          </SheetDescription>
        </SheetHeader>
        <Form {...form}>
          <form
            id='provider-form'
            onSubmit={form.handleSubmit(onSubmit)}
            className={sideDrawerFormClassName()}
            aria-busy={!isReady}
          >
            <fieldset disabled={!isReady || isSubmitting} className='contents'>
              <SideDrawerSection>
                <ProviderSelectField
                  form={form}
                  name='type'
                  label={t('Type')}
                  options={typeOptions}
                  toValue={Number}
                  disabled={isUpdate}
                  description={
                    isUpdate
                      ? t('Create a new Provider to use a different type.')
                      : undefined
                  }
                />
                <FormField
                  control={form.control}
                  name='name'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Name')}</FormLabel>
                      <FormControl>
                        <Input {...field} placeholder={t('Enter a name')} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                <FormField
                  control={form.control}
                  name='base_url'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Base URL')}</FormLabel>
                      <FormControl>
                        <Input {...field} placeholder='https://' />
                      </FormControl>
                      <FormDescription>
                        {t(
                          'Leave empty to use the default address of the type.'
                        )}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                {!isUpdate && (
                  <ProviderSelectField
                    form={form}
                    name='mode'
                    label={t('Key mode')}
                    options={translate(PROVIDER_ADD_MODE_OPTIONS)}
                  />
                )}
                {!isUpdate && props.open && (
                  <RecommendationPicker
                    kind='provider'
                    input={{ url: form.watch('base_url'), type }}
                    onConfirm={(rule) => {
                      form.setValue('name', rule.provider_name, {
                        shouldDirty: true,
                      })
                      form.setValue('type', rule.provider_type, {
                        shouldDirty: true,
                      })
                    }}
                  />
                )}
                {!isUpdate && mode === 'multi_to_single' && (
                  <ProviderSelectField
                    form={form}
                    name='multi_key_mode'
                    label={t('Key rotation')}
                    options={translate(MULTI_KEY_MODES)}
                  />
                )}
                <FormField
                  control={form.control}
                  name='key'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Key')}</FormLabel>
                      <FormControl>
                        <Textarea
                          {...field}
                          autoComplete='off'
                          spellCheck={false}
                          rows={4}
                          className='font-mono'
                          placeholder={
                            isUpdate
                              ? t('Leave empty to keep the current key')
                              : t('Enter the key; one key per line')
                          }
                        />
                      </FormControl>
                      <FormDescription>
                        {t('Saved keys are never shown again.')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
                {isUpdate && isMultiKey && key.trim() !== '' && (
                  <ProviderSelectField
                    form={form}
                    name='key_mode'
                    label={t('Key update mode')}
                    options={translate(PROVIDER_KEY_MODE_OPTIONS)}
                  />
                )}
                <div
                  className='grid gap-4 sm:grid-cols-3'
                  role='group'
                  aria-label={t('Rate limits')}
                >
                  {(['rpm', 'tpm', 'max_concurrency'] as const).map((name) => (
                    <FormField
                      key={name}
                      control={form.control}
                      name={name}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>
                            {t(
                              {
                                rpm: 'RPM',
                                tpm: 'TPM',
                                max_concurrency: 'Max concurrency',
                              }[name]
                            )}
                          </FormLabel>
                          <FormControl>
                            <Input inputMode='numeric' {...field} />
                          </FormControl>
                          <FormDescription>
                            {t('0 means unlimited')}
                          </FormDescription>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  ))}
                </div>
                <FormField
                  control={form.control}
                  name='remark'
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{t('Remark')}</FormLabel>
                      <FormControl>
                        <Input {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              </SideDrawerSection>
              <SideDrawerSection>
                <h3 className='text-sm font-medium'>{t('Adapter settings')}</h3>
                <p className='text-muted-foreground text-sm'>
                  {t(
                    'Saved adapter settings are never shown. Leave blank to keep existing values; enter a complete JSON object to replace a setting.'
                  )}
                </p>
                <AdapterDeploymentField
                  control={form.control}
                  name='other'
                  type={type}
                  required={!isUpdate}
                />
                {type === 1 && (
                  <FormField
                    control={form.control}
                    name='openai_organization'
                    render={({ field }) => (
                      <FormItem>
                        <FormLabel>
                          {t('OpenAI Organization ID (optional)')}
                        </FormLabel>
                        <FormControl>
                          <Input {...field} />
                        </FormControl>
                        <FormMessage />
                      </FormItem>
                    )}
                  />
                )}
                {(['settings', 'setting', 'header_override'] as const).map(
                  (name) => (
                    <FormField
                      key={name}
                      control={form.control}
                      name={name}
                      render={({ field }) => (
                        <FormItem>
                          <FormLabel>
                            {t(
                              {
                                settings: 'Type-specific settings (JSON)',
                                setting: 'Connection settings (JSON)',
                                header_override: 'Header Override',
                              }[name]
                            )}
                          </FormLabel>
                          <FormControl>
                            <JsonEditor
                              value={field.value || ''}
                              onChange={field.onChange}
                              valueType='any'
                              disabled={isSubmitting || !isReady}
                              template={
                                name === 'settings'
                                  ? adapterSettingsTemplates[type]
                                  : undefined
                              }
                            />
                          </FormControl>
                          <FormMessage />
                        </FormItem>
                      )}
                    />
                  )
                )}
              </SideDrawerSection>
            </fieldset>
          </form>
        </Form>
        <SheetFooter className={sideDrawerFooterClassName()}>
          <SheetClose render={<Button variant='outline' />}>
            {t('Close')}
          </SheetClose>
          <Button
            form='provider-form'
            type='submit'
            disabled={isSubmitting || !isReady}
          >
            {submitLabel}
          </Button>
        </SheetFooter>
      </SheetContent>
    </Sheet>
  )
}
